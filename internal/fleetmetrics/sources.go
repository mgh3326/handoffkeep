package fleetmetrics

import (
	"bufio"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
	"go.yaml.in/yaml/v3"
)

// ---------------------------------------------------------------- job dirs

type rawJobEvent struct {
	Kind      string          `json:"kind"`
	Seq       int             `json:"seq"`
	CreatedAt string          `json:"created_at"`
	At        string          `json:"at"`
	EventTime string          `json:"event_time"`
	Epoch     int             `json:"epoch"`
	Reason    string          `json:"reason"`
	PR        string          `json:"pr"`
	Head      string          `json:"head"`
	OwnerLane string          `json:"owner_lane"`
	PaneID    string          `json:"pane_id"`
	Payload   json.RawMessage `json:"payload"`
}

type rawPayload struct {
	Role       string `json:"role"`
	OwnerLane  string `json:"owner_lane"`
	ParentLane string `json:"parent_lane"`
	TLevel     string `json:"t_level"`
	Profile    string `json:"profile"`
	PaneID     string `json:"pane_id"`
	Epoch      int    `json:"epoch"`
	PR         string `json:"pr"`
	Head       string `json:"head"`
}

func parseTime(s string) *time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05.999999-07:00", "2006-01-02 15:04:05.999999-07:00"} {
		if t, err := time.Parse(layout, s); err == nil {
			t = t.UTC()
			return &t
		}
	}
	return nil
}

// ParseJobDir reads <dir>/events/*.json. The flat completion records written
// by wrk done/joined/escalate carry no timestamp; for those the file's
// modification time is used and marked AtSource="mtime". A job without a
// job.claim record is returned with Role "" (its role is unknown).
func ParseJobDir(dir, machine string) (Job, error) {
	j := Job{JobID: filepath.Base(dir), Machine: machine}
	files, err := filepath.Glob(filepath.Join(dir, "events", "*.json"))
	if err != nil {
		return j, err
	}
	if len(files) == 0 {
		return j, errors.New("no events")
	}
	sort.Strings(files)
	for i, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		var raw rawJobEvent
		if json.Unmarshal(b, &raw) != nil || raw.Kind == "" {
			continue
		}
		var pl rawPayload
		if len(raw.Payload) > 0 {
			_ = json.Unmarshal(raw.Payload, &pl)
		}
		e := JobEvent{Seq: raw.Seq, Kind: raw.Kind, Epoch: raw.Epoch, Reason: raw.Reason, PR: raw.PR, Head: raw.Head}
		if e.Seq == 0 {
			e.Seq = i + 1
		}
		if e.Epoch == 0 {
			e.Epoch = pl.Epoch
		}
		if e.PR == "" {
			e.PR = pl.PR
		}
		for _, s := range []string{raw.CreatedAt, raw.At, raw.EventTime} {
			if t := parseTime(s); t != nil {
				e.At, e.AtSource = t, "payload"
				break
			}
		}
		if e.At == nil {
			if st, err := os.Stat(f); err == nil {
				t := st.ModTime().UTC()
				e.At, e.AtSource = &t, "mtime"
			}
		}
		switch raw.Kind {
		case "job.claim":
			j.Role, j.OwnerLane, j.ParentLane, j.Tier = pl.Role, pl.OwnerLane, pl.ParentLane, pl.TLevel
			if j.OwnerLane == "" {
				j.OwnerLane = raw.OwnerLane
			}
		case "job.spawned":
			j.Profile, j.PaneID = pl.Profile, pl.PaneID
		}
		if j.PaneID == "" && raw.PaneID != "" {
			j.PaneID = raw.PaneID
		}
		j.Events = append(j.Events, e)
	}
	if len(j.Events) == 0 {
		return j, errors.New("no parseable events")
	}
	if b, err := os.ReadFile(filepath.Join(dir, "completion-sentinel.log")); err == nil {
		j.Status = ParseSentinelLog(string(b))
	}
	return j, nil
}

// sentinelGap is the longest silence between two samples that still counts
// as one continuous observation (the sentinel samples every ~30s).
const sentinelGap = 2 * time.Minute

func normalizeStatus(s string) string {
	switch {
	case s == "working":
		return "working"
	case s == "idle" || s == "done" || s == "blocked":
		return "idle"
	case s == "err:agent_not_found":
		return "gone"
	}
	return "unknown"
}

// ParseSentinelLog folds "<RFC3339> status=<s> …" lines into runs. A run
// ends at its last sample (plus nothing): time after the last sample of a
// run is unobserved unless the next sample follows within sentinelGap.
func ParseSentinelLog(text string) []StatusRun {
	var out []StatusRun
	for _, line := range strings.Split(text, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || !strings.HasPrefix(fields[1], "status=") {
			continue
		}
		at, err := time.Parse(time.RFC3339, fields[0])
		if err != nil {
			continue
		}
		at = at.UTC()
		st := normalizeStatus(strings.TrimPrefix(fields[1], "status="))
		if n := len(out); n > 0 && !at.Before(out[n-1].To) && at.Sub(out[n-1].To) <= sentinelGap {
			if out[n-1].Status == st {
				out[n-1].To = at
				continue
			}
			// A status change inside the gap: the previous run lasts until now.
			out[n-1].To = at
		}
		out = append(out, StatusRun{From: at, To: at, Status: st})
	}
	return out
}

// ReadJobRoot reads every job directory under root. Jobs whose every event is
// before notBefore are skipped (they cannot overlap the window) unless they
// are builders with no terminal record — those are kept so the window can be
// marked unknown instead of empty.
func ReadJobRoot(root, machine string, notBefore time.Time) ([]Job, int, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, 0, err
	}
	var out []Job
	skipped := 0
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		j, err := ParseJobDir(filepath.Join(root, entry.Name()), machine)
		if err != nil {
			skipped++
			continue
		}
		recent, terminal := false, false
		for _, e := range j.Events {
			if e.At != nil && !e.At.Before(notBefore) {
				recent = true
			}
			switch e.Kind {
			case "job.reaped", "job.lost", "job.revoked":
				terminal = true
			}
		}
		if !recent && (j.Role != "builder" || terminal) {
			continue
		}
		out = append(out, j)
	}
	return out, skipped, nil
}

// ---------------------------------------------------------------- reps

// ParseRepsList parses `scopefuel reps list` output: one row per line of
// space-separated key=value pairs, with notes= running to end of line.
func ParseRepsList(text, source string) []Rep {
	var out []Rep
	sc := bufio.NewScanner(strings.NewReader(text))
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if !strings.HasPrefix(line, "id=") {
			continue
		}
		if i := strings.Index(line, " notes="); i >= 0 {
			line = line[:i]
		}
		kv := map[string]string{}
		for _, f := range strings.Fields(line) {
			k, v, ok := strings.Cut(f, "=")
			if ok {
				kv[k] = v
			}
		}
		id, err := strconv.ParseInt(kv["id"], 10, 64)
		if err != nil {
			continue
		}
		num := func(k string) *int {
			if v, err := strconv.Atoi(kv[k]); err == nil {
				return &v
			}
			return nil
		}
		r := Rep{ID: id, Source: source, Profile: kv["profile"], Task: kv["task"], Role: kv["role"], Rounds: num("rounds"), BlockersFound: num("blockers-found"), Completed: num("completed")}
		if t := parseTime(kv["recorded_at"]); t != nil {
			r.RecordedAt = *t
		}
		out = append(out, r)
	}
	return out
}

// ---------------------------------------------------------------- deploys

// ParseDeployDoc parses a deploy/<service>/<stamp> document body. The R12
// contract is JSON (deploy-record/v0); a YAML-shaped body is read for the
// same field names. A missing result stays "" (unknown), never "success".
func ParseDeployDoc(key, body string) DeployRecord {
	d := DeployRecord{Key: key, Format: "invalid"}
	parts := strings.Split(key, "/")
	if len(parts) >= 2 {
		d.Service = parts[1]
	}
	var m map[string]any
	if json.Unmarshal([]byte(body), &m) == nil {
		d.Format = "json"
	} else if yaml.Unmarshal([]byte(body), &m) == nil && m != nil {
		d.Format = "yaml"
	} else {
		return d
	}
	str := func(k string) string {
		switch v := m[k].(type) {
		case string:
			return v
		case time.Time:
			return v.UTC().Format(time.RFC3339)
		}
		return ""
	}
	if s := str("service"); s != "" {
		d.Service = s
	}
	d.Result = str("result")
	d.DeployedAt = parseTime(str("deployed_at"))
	for _, k := range []string{"deployed_ref", "head_sha", "sha"} {
		if v := str(k); v != "" {
			d.DeployedRef = v
			break
		}
	}
	if xs, ok := m["included_prs"].([]any); ok {
		for _, x := range xs {
			if s, ok := x.(string); ok {
				d.IncludedPRs = append(d.IncludedPRs, s)
			}
		}
	}
	return d
}

// ---------------------------------------------------------------- slots

// ParseHostsSlots reads wrk's hosts.toml: [local] max_active is the local
// machine's slot count, [hosts.<id>] capacity the others'.
func ParseHostsSlots(data []byte, localMachine, source string) ([]SlotCapacity, error) {
	var cfg struct {
		Local struct {
			MaxActive int `toml:"max_active"`
		} `toml:"local"`
		Hosts map[string]struct {
			Capacity int `toml:"capacity"`
		} `toml:"hosts"`
	}
	if _, err := toml.Decode(string(data), &cfg); err != nil {
		return nil, err
	}
	var out []SlotCapacity
	if cfg.Local.MaxActive > 0 && localMachine != "" {
		out = append(out, SlotCapacity{Machine: localMachine, Slots: cfg.Local.MaxActive, Source: source + " [local] max_active"})
	}
	for id, h := range cfg.Hosts {
		if h.Capacity > 0 {
			out = append(out, SlotCapacity{Machine: id, Slots: h.Capacity, Source: source + " [hosts." + id + "] capacity"})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Machine < out[j].Machine })
	return out, nil
}
