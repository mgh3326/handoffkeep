package fleetmetrics

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/mgh3326/handoffkeep/internal/store"
)

// HK is the read-only slice of the hk client the collector uses. Every call
// is a GET.
type HK interface {
	ExportTasks(ctx context.Context, lane, state, parentLane string, limit int) ([]byte, error)
	GetTask(ctx context.Context, id int64) (store.Task, bool, error)
	ListRelayEventsPage(ctx context.Context, kind string, afterID int64, limit int) ([]store.RelayEvent, error)
	ListDocuments(ctx context.Context, prefix, kind, session string, limit int) ([]store.Document, error)
	GetDocument(ctx context.Context, key string) (store.Document, bool, error)
	ListTaskComments(ctx context.Context, taskID, afterID int64, limit int) ([]store.TaskComment, error)
}

// Runner executes a read-only local command (scopefuel, gh, herdr).
type Runner func(ctx context.Context, name string, args ...string) ([]byte, error)

type CollectConfig struct {
	HK           HK
	Run          Runner
	Since, Until time.Time
	Now          time.Time
	JobRoots     []JobRoot
	// RepsLocal runs `scopefuel reps list` on this machine as one source.
	RepsLocal    bool
	LocalMachine string
	// RepsFiles are saved `scopefuel reps list` outputs, "path" or "path=source".
	RepsFiles     []string
	HostsConfig   string
	SlotOverrides []SlotCapacity
	GitHub        bool
	Herdr         bool
	Parallel      int
}

const relayPage = 1000

// Collect builds a Snapshot from the live sources. A source that fails is
// recorded in Notes and left empty; its metrics then report lower coverage.
func Collect(ctx context.Context, cfg CollectConfig) (Snapshot, error) {
	s := Snapshot{Schema: SnapshotSchema, CollectedAt: cfg.Now.UTC(), Since: cfg.Since.UTC(), Until: cfg.Until.UTC(), PRs: map[string]PRFact{}, Contains: map[string]bool{}, TaskComments: map[int64][]Comment{}}
	if cfg.Parallel < 1 {
		cfg.Parallel = 8
	}
	raw, err := cfg.HK.ExportTasks(ctx, "", "", "", store.ExportLimitMax)
	if err != nil {
		return s, fmt.Errorf("hk tasks export: %w", err)
	}
	var export store.TaskExport
	if err := json.Unmarshal(raw, &export); err != nil {
		return s, fmt.Errorf("hk tasks export: %w", err)
	}
	if !export.Complete {
		s.Notes = append(s.Notes, fmt.Sprintf("hk task export truncated: %d of %d rows", export.RowsReturned, export.Counts.Total))
	}
	s.Tasks = export.Tasks
	// Event history is fetched for every task touched in the window and every
	// non-terminal task (whose claim time open-work age needs); an untouched
	// terminal task keeps Events == nil and its current state for the window.
	var need []int
	for i, t := range s.Tasks {
		terminal := t.State == "merged" || t.State == "dropped"
		if !terminal || !t.UpdatedAt.Before(s.Since) {
			need = append(need, i)
		}
	}
	var mu sync.Mutex
	var failed []string
	parallel(ctx, cfg.Parallel, len(need), func(k int) {
		i := need[k]
		x, found, err := cfg.HK.GetTask(ctx, s.Tasks[i].ID)
		if err != nil || !found {
			mu.Lock()
			failed = append(failed, fmt.Sprint(s.Tasks[i].ID))
			mu.Unlock()
			return
		}
		if x.Events == nil {
			x.Events = []store.TaskEvent{}
		}
		s.Tasks[i] = x
		if x.State != "backlog" {
			cs, err := cfg.HK.ListTaskComments(ctx, x.ID, 0, store.TaskCommentListMax)
			if err == nil && len(cs) > 0 {
				mu.Lock()
				for _, c := range cs {
					s.TaskComments[x.ID] = append(s.TaskComments[x.ID], Comment{ID: c.ID, Body: c.Body, At: c.CreatedAt})
				}
				mu.Unlock()
			}
		}
	})
	if len(failed) > 0 {
		sort.Strings(failed)
		s.Notes = append(s.Notes, "task event fetch failed for: "+strings.Join(failed, ","))
	}
	for after := int64(0); ; {
		page, err := cfg.HK.ListRelayEventsPage(ctx, "", after, relayPage)
		if err != nil {
			s.Notes = append(s.Notes, "relay events: "+err.Error())
			break
		}
		for _, e := range page {
			if e.Kind != "lane.event" || strings.Contains(e.EventID, "decision-") || strings.HasPrefix(e.Text, "[decision") {
				s.Relay = append(s.Relay, e)
			}
		}
		if len(page) < relayPage {
			break
		}
		after = page[len(page)-1].ID
	}
	collectDeploys(ctx, cfg, &s)
	collectJobs(ctx, cfg, &s)
	collectReps(ctx, cfg, &s)
	collectSlots(cfg, &s)
	if cfg.GitHub && cfg.Run != nil {
		collectGitHub(ctx, cfg, &s)
	} else {
		s.Notes = append(s.Notes, "GitHub not read: merge times come from hk merged transitions")
	}
	return s, nil
}

func parallel(ctx context.Context, n, count int, f func(int)) {
	var wg sync.WaitGroup
	ch := make(chan int)
	for w := 0; w < n; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range ch {
				f(i)
			}
		}()
	}
	for i := 0; i < count; i++ {
		if ctx.Err() != nil {
			break
		}
		ch <- i
	}
	close(ch)
	wg.Wait()
}

func collectDeploys(ctx context.Context, cfg CollectConfig, s *Snapshot) {
	docs, err := cfg.HK.ListDocuments(ctx, "deploy/", "", "", 1000)
	if err != nil {
		s.Notes = append(s.Notes, "deploy records: "+err.Error())
		return
	}
	sort.Slice(docs, func(i, j int) bool { return docs[i].Key < docs[j].Key })
	for _, d := range docs {
		full, found, err := cfg.HK.GetDocument(ctx, d.Key)
		if err != nil || !found {
			s.Notes = append(s.Notes, "deploy record unreadable: "+d.Key)
			continue
		}
		s.Deploys = append(s.Deploys, ParseDeployDoc(full.Key, full.Body))
	}
}

func collectJobs(ctx context.Context, cfg CollectConfig, s *Snapshot) {
	live := map[string]bool{}
	if cfg.Herdr && cfg.Run != nil {
		out, err := cfg.Run(ctx, "herdr", "agent", "list")
		var parsed struct {
			Result struct {
				Agents []struct {
					PaneID string `json:"pane_id"`
				} `json:"agents"`
			} `json:"result"`
		}
		if err == nil && json.Unmarshal(out, &parsed) == nil {
			for _, a := range parsed.Result.Agents {
				live[a.PaneID] = true
			}
		} else {
			s.Notes = append(s.Notes, "herdr agent list unavailable: jobs without a terminal event have an unknown end")
		}
	}
	for _, root := range cfg.JobRoots {
		jobs, skipped, err := ReadJobRoot(root.Path, root.Machine, s.Since)
		if err != nil {
			s.Notes = append(s.Notes, "job root "+root.Path+": "+err.Error())
			continue
		}
		s.JobRoots = append(s.JobRoots, root)
		if skipped > 0 {
			s.Notes = append(s.Notes, fmt.Sprintf("job root %s: %d directories without parseable events skipped", root.Path, skipped))
		}
		for i := range jobs {
			if root.Machine == cfg.LocalMachine && jobs[i].PaneID != "" && live[jobs[i].PaneID] {
				jobs[i].Live = true
			}
		}
		s.Jobs = append(s.Jobs, jobs...)
	}
}

func collectReps(ctx context.Context, cfg CollectConfig, s *Snapshot) {
	if cfg.RepsLocal && cfg.Run != nil {
		out, err := cfg.Run(ctx, "scopefuel", "reps", "list", "--limit", "100000")
		if err != nil {
			s.Notes = append(s.Notes, "scopefuel reps list (local): "+err.Error())
		} else {
			src := "scopefuel reps list @" + cfg.LocalMachine
			s.Reps = append(s.Reps, ParseRepsList(string(out), src)...)
			s.RepSources = append(s.RepSources, src)
		}
	}
	for _, spec := range cfg.RepsFiles {
		path, src, ok := strings.Cut(spec, "=")
		if !ok {
			src = path
		}
		b, err := os.ReadFile(path)
		if err != nil {
			s.Notes = append(s.Notes, "reps file "+path+": "+err.Error())
			continue
		}
		s.Reps = append(s.Reps, ParseRepsList(string(b), src)...)
		s.RepSources = append(s.RepSources, src)
	}
}

func collectSlots(cfg CollectConfig, s *Snapshot) {
	byMachine := map[string]SlotCapacity{}
	if cfg.HostsConfig != "" {
		b, err := os.ReadFile(cfg.HostsConfig)
		if err != nil {
			s.Notes = append(s.Notes, "hosts config: "+err.Error())
		} else if caps, err := ParseHostsSlots(b, cfg.LocalMachine, cfg.HostsConfig); err != nil {
			s.Notes = append(s.Notes, "hosts config: "+err.Error())
		} else {
			for _, c := range caps {
				byMachine[c.Machine] = c
			}
			s.Notes = append(s.Notes, "machines without --slots take their capacity from "+cfg.HostsConfig+" for the whole window (no in-window history)")
		}
	}
	// --slots entries replace the config for their machine (a dated timeline
	// is how in-window capacity changes are replayed).
	overridden := map[string]bool{}
	for _, c := range cfg.SlotOverrides {
		overridden[c.Machine] = true
	}
	for m, c := range byMachine {
		if !overridden[m] {
			s.Slots = append(s.Slots, c)
		}
	}
	s.Slots = append(s.Slots, cfg.SlotOverrides...)
	sort.SliceStable(s.Slots, func(i, j int) bool { return s.Slots[i].Machine < s.Slots[j].Machine })
}

func collectGitHub(ctx context.Context, cfg CollectConfig, s *Snapshot) {
	ix := buildIndex(s)
	urls := map[string]bool{}
	for id, t := range ix.tasks {
		if t.State != "merged" {
			continue
		}
		if at := terminalAt(t); at == nil || !inWindow(*at, s) {
			continue
		}
		for u := range ix.taskPRs[id] {
			urls[u] = true
		}
	}
	list := make([]string, 0, len(urls))
	for u := range urls {
		list = append(list, u)
	}
	sort.Strings(list)
	var mu sync.Mutex
	parallel(ctx, cfg.Parallel, len(list), func(i int) {
		u := list[i]
		_, repo, n := CanonicalPR(u)
		fact := PRFact{URL: u, Repo: repo, Number: n}
		out, err := cfg.Run(ctx, "gh", "api", fmt.Sprintf("repos/%s/pulls/%d", repo, n))
		var pr struct {
			MergedAt       *time.Time `json:"merged_at"`
			MergeCommitSHA string     `json:"merge_commit_sha"`
			Head           struct {
				SHA string `json:"sha"`
			} `json:"head"`
		}
		if err != nil || json.Unmarshal(out, &pr) != nil {
			fact.FetchNote = "gh api failed"
		} else {
			fact.MergedAt, fact.HeadSHA = pr.MergedAt, pr.Head.SHA
			if pr.MergedAt != nil {
				fact.MergeSHA = pr.MergeCommitSHA
			}
		}
		mu.Lock()
		s.PRs[u] = fact
		mu.Unlock()
	})
	// Ancestry: for each merged PR in a deploy repo, ask whether each later
	// successful deploy of that repo's services contains the merge commit.
	type pair struct{ repo, merge, ref string }
	var pairs []pair
	seen := map[string]bool{}
	for _, u := range list {
		f := s.PRs[u]
		if f.MergedAt == nil || f.MergeSHA == "" {
			continue
		}
		for _, d := range s.Deploys {
			if DeployRepos[d.Service] != f.Repo || d.Result != "success" || d.DeployedAt == nil || d.DeployedAt.Before(*f.MergedAt) {
				continue
			}
			ref := strings.TrimPrefix(d.DeployedRef, "pw-")
			if len(ref) < 7 || !isHex(ref) || strings.HasPrefix(f.MergeSHA, ref) {
				continue
			}
			k := f.Repo + "@" + f.MergeSHA + ".." + ref
			if !seen[k] {
				seen[k] = true
				pairs = append(pairs, pair{f.Repo, f.MergeSHA, ref})
			}
		}
	}
	parallel(ctx, cfg.Parallel, len(pairs), func(i int) {
		p := pairs[i]
		out, err := cfg.Run(ctx, "gh", "api", fmt.Sprintf("repos/%s/compare/%s...%s", p.repo, p.merge, p.ref), "--jq", ".status")
		if err != nil {
			return
		}
		status := strings.TrimSpace(string(out))
		if status == "" {
			return
		}
		mu.Lock()
		s.Contains[p.repo+"@"+p.merge+".."+p.ref] = status == "ahead" || status == "identical"
		mu.Unlock()
	})
}

func isHex(s string) bool {
	for _, r := range s {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
			return false
		}
	}
	return true
}
