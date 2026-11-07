package youtrack

import (
	"context"
	"fmt"
	"log"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mgh3326/handoffkeep/internal/store"
)

// Store is the slice of internal/store the service uses. *store.Store
// implements it; tests use an in-memory fake so mapping and overlay behaviour
// is exercised without PostgreSQL.
type Store interface {
	GetTask(ctx context.Context, id int64) (store.Task, bool, error)
	ListTaskComments(ctx context.Context, taskID, afterID int64, limit int) ([]store.TaskComment, error)
	ListTranslations(ctx context.Context, entity string, sources []store.TranslationSource) ([]store.Translation, error)
	TaskProjectExists(ctx context.Context, name string) (bool, error)

	GetYouTrackMirror(ctx context.Context, taskID int64) (store.YouTrackMirror, bool, error)
	FindYouTrackMirrorByIssueID(ctx context.Context, issueID string) (store.YouTrackMirror, bool, error)
	ListYouTrackMirrors(ctx context.Context, taskIDs []int64) (map[int64]store.YouTrackMirror, error)
	UpsertYouTrackMirror(ctx context.Context, in store.YouTrackMirrorInput) (store.YouTrackMirror, error)
	UpdateYouTrackMirrorDisplay(ctx context.Context, taskID int64, displayRef, url string) error
	MarkYouTrackTaskSynced(ctx context.Context, taskID int64, at time.Time) error

	ListYouTrackProjects(ctx context.Context) ([]store.YouTrackProject, error)
	YouTrackProjectFor(ctx context.Context, hkProject string) (store.YouTrackProject, bool, error)
	YouTrackProjectByShortName(ctx context.Context, shortName string) (store.YouTrackProject, bool, error)
	PutYouTrackProject(ctx context.Context, p store.YouTrackProject) error
	SetYouTrackProjectID(ctx context.Context, hkProject, projectID string) error

	GetYouTrackFieldCache(ctx context.Context, taskID int64) (store.YouTrackFieldCache, bool, error)
	GetYouTrackFieldCaches(ctx context.Context, taskIDs []int64) (map[int64]store.YouTrackFieldCache, error)
	PutYouTrackFieldCache(ctx context.Context, x store.YouTrackFieldCache) error
	SetYouTrackCachedPriority(ctx context.Context, taskID int64, priority int, at time.Time) error
	SetYouTrackCachedLane(ctx context.Context, taskID int64, lane string, at time.Time) error
	MarkYouTrackFieldCacheStale(ctx context.Context, taskID int64) error
	RefreshYouTrackFields(ctx context.Context, limit int) ([]store.YouTrackMirror, error)

	EnqueueYouTrack(ctx context.Context, taskID int64, op, value string) error
	DueYouTrackOutbox(ctx context.Context, limit int) ([]store.YouTrackOutboxItem, error)
	MarkYouTrackOutboxDone(ctx context.Context, id int64, value string) error
	RescheduleYouTrackOutbox(ctx context.Context, id int64, value string, attempts int, next time.Time, lastError string) error

	DueYouTrackPushTasks(ctx context.Context, limit int) ([]store.Task, error)
	UnpushedYouTrackComments(ctx context.Context, taskID, afterID int64, limit int) ([]store.TaskComment, error)
	LinkYouTrackComment(ctx context.Context, taskID, hkCommentID int64, youtrackCommentID string, at time.Time) error
	FindYouTrackCommentLinkByYouTrackID(ctx context.Context, youtrackCommentID string) (store.YouTrackCommentLink, bool, error)

	GetYouTrackCursor(ctx context.Context) (store.YouTrackCursor, error)
	AdvanceYouTrackCursor(ctx context.Context, cursor string) error
	ApplyYouTrackActivity(ctx context.Context, in store.YouTrackActivity) (store.YouTrackApplyResult, error)
	PendingYouTrackReverts(ctx context.Context, limit int) ([]store.YouTrackPendingRevert, error)
	MarkYouTrackRevertDone(ctx context.Context, activityID string) error
}

// Options tune the service. Zero values get the documented defaults.
type Options struct {
	Schema           Schema
	Tenant           string
	DefaultHKProject string
	DefaultLane      string
	ConsoleBaseURL   string
	SeedLimit        int
	Now              func() time.Time
}

const (
	defaultHKProject      = "other"
	defaultLane           = "operator-desk"
	defaultConsoleBaseURL = "https://console.robinco.dev"
	defaultSeedLimit      = 10
)

type Service struct {
	store   Store
	client  *Client
	schema  Schema
	tenant  string
	project string
	lane    string
	console string
	seeds   int
	now     func() time.Time

	projectMu  sync.Mutex
	projectIDs map[string]string

	fieldMu sync.Mutex
	fields  map[string]FieldInfo
}

func NewService(st Store, client *Client, opt Options) *Service {
	if opt.Schema.HKIDField == "" {
		opt.Schema = DefaultSchema()
	}
	if opt.Tenant == "" {
		opt.Tenant = tenantFromClient(client)
	}
	if opt.DefaultHKProject == "" {
		opt.DefaultHKProject = defaultHKProject
	}
	if opt.DefaultLane == "" {
		opt.DefaultLane = defaultLane
	}
	if opt.ConsoleBaseURL == "" {
		opt.ConsoleBaseURL = defaultConsoleBaseURL
	}
	if opt.SeedLimit <= 0 {
		opt.SeedLimit = defaultSeedLimit
	}
	if opt.Now == nil {
		opt.Now = func() time.Time { return time.Now().UTC() }
	}
	return &Service{
		store:      st,
		client:     client,
		schema:     opt.Schema,
		tenant:     opt.Tenant,
		project:    opt.DefaultHKProject,
		lane:       opt.DefaultLane,
		console:    opt.ConsoleBaseURL,
		seeds:      opt.SeedLimit,
		now:        opt.Now,
		projectIDs: map[string]string{},
		fields:     map[string]FieldInfo{},
	}
}

func tenantFromClient(c *Client) string {
	if c == nil {
		return ""
	}
	u, err := url.Parse(c.baseURL)
	if err != nil || u.Host == "" {
		return c.baseURL
	}
	return u.Host
}

func (s *Service) Schema() Schema { return s.schema }
func (s *Service) Tenant() string { return s.tenant }

// Overlay replaces priority and lane on mirrored tasks with the cached values
// and says where they came from. An unmirrored task is left exactly as it
// was. The cache is refreshed by the sync; a read only seeds a missing row.
func (s *Service) Overlay(ctx context.Context, tasks []store.Task) {
	if s == nil || s.client == nil || len(tasks) == 0 {
		return
	}
	ids := make([]int64, 0, len(tasks))
	for _, task := range tasks {
		ids = append(ids, task.ID)
	}
	mirrors, err := s.store.ListYouTrackMirrors(ctx, ids)
	if err != nil {
		log.Printf("youtrack.overlay_mirrors_failed err=%q", err)
		return
	}
	if len(mirrors) == 0 {
		return
	}
	caches, err := s.store.GetYouTrackFieldCaches(ctx, ids)
	if err != nil {
		log.Printf("youtrack.overlay_cache_failed err=%q", err)
		return
	}
	seeds := s.seeds
	seedCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	for i := range tasks {
		mirror, mirrored := mirrors[tasks[i].ID]
		if !mirrored {
			continue
		}
		cache, cached := caches[tasks[i].ID]
		if !cached {
			if seeds <= 0 {
				// The YouTrack value is unknown; say the shown value is the
				// PG execution value, not a fresh mirror.
				tasks[i].PrioritySource = store.YouTrackSourceHK
				tasks[i].LaneSource = store.YouTrackSourceHK
				tasks[i].Stale = true
				continue
			}
			seeds--
			cache, _ = s.fetchCache(ctx, seedCtx, mirror, tasks[i])
			if cache.TaskID == 0 {
				continue
			}
		}
		tasks[i].Priority = cache.Priority
		tasks[i].Lane = cache.Lane
		tasks[i].PrioritySource = cache.Source
		tasks[i].LaneSource = cache.Source
		at := cache.FetchedAt
		tasks[i].ValuesFetchedAt = &at
		tasks[i].Stale = cache.Stale
	}
}

// fetchCache reads one issue for a read-path seed. fetchCtx bounds the
// YouTrack call; the cache write uses writeCtx so a row still lands after the
// seeding deadline, and the next read does not pay for the same stall again.
func (s *Service) fetchCache(writeCtx, fetchCtx context.Context, mirror store.YouTrackMirror, task store.Task) (store.YouTrackFieldCache, error) {
	issue, err := s.client.GetIssue(fetchCtx, mirror.IssueID)
	if err != nil {
		// PG is the execution value. Keep it as the last known value and say
		// the YouTrack read is stale.
		cache := store.YouTrackFieldCache{TaskID: task.ID, Priority: task.Priority, Lane: task.Lane, Source: store.YouTrackSourceHK, FetchedAt: s.now(), Stale: true}
		if perr := s.store.PutYouTrackFieldCache(writeCtx, cache); perr != nil {
			log.Printf("youtrack.cache_write_failed task_id=%d err=%q", task.ID, perr)
		}
		return cache, err
	}
	cache := s.cacheFromIssue(task.ID, issue, task)
	if perr := s.store.PutYouTrackFieldCache(writeCtx, cache); perr != nil {
		log.Printf("youtrack.cache_write_failed task_id=%d err=%q", task.ID, perr)
	}
	return cache, nil
}

func (s *Service) cacheFromIssue(taskID int64, issue Issue, fallback store.Task) store.YouTrackFieldCache {
	priority, lane := fallback.Priority, fallback.Lane
	if f, ok := IssueFieldByName(issue.CustomFields, s.schema.PriorityField); ok {
		if n, ok := f.IntValue(); ok {
			priority = n
		}
	}
	if f, ok := IssueFieldByName(issue.CustomFields, s.schema.LaneField); ok {
		if v := f.StringValue(); v != "" {
			lane = v
		}
	}
	return store.YouTrackFieldCache{TaskID: taskID, Priority: priority, Lane: lane, Source: store.YouTrackSourceYouTrack, FetchedAt: s.now()}
}

// RecordPriority runs after the PG write committed: it queues the YouTrack
// export and updates the served cache. A failure here never fails the hk
// write, and the next push catches the same change.
func (s *Service) RecordPriority(ctx context.Context, taskID int64, priority int) {
	if s == nil {
		return
	}
	if _, ok, err := s.store.GetYouTrackMirror(ctx, taskID); err != nil {
		log.Printf("youtrack.mirror_lookup_failed task_id=%d err=%q", taskID, err)
		return
	} else if !ok {
		return
	}
	if err := s.store.EnqueueYouTrack(ctx, taskID, store.YouTrackOpPriority, strconv.Itoa(priority)); err != nil {
		log.Printf("youtrack.outbox_priority_failed task_id=%d err=%q", taskID, err)
	}
	if err := s.store.SetYouTrackCachedPriority(ctx, taskID, priority, s.now()); err != nil {
		log.Printf("youtrack.cache_priority_failed task_id=%d err=%q", taskID, err)
	}
}

// RecordLane is RecordPriority for a lane change.
func (s *Service) RecordLane(ctx context.Context, taskID int64, lane string) {
	if s == nil {
		return
	}
	if _, ok, err := s.store.GetYouTrackMirror(ctx, taskID); err != nil {
		log.Printf("youtrack.mirror_lookup_failed task_id=%d err=%q", taskID, err)
		return
	} else if !ok {
		return
	}
	if err := s.store.EnqueueYouTrack(ctx, taskID, store.YouTrackOpLane, lane); err != nil {
		log.Printf("youtrack.outbox_lane_failed task_id=%d err=%q", taskID, err)
	}
	if err := s.store.SetYouTrackCachedLane(ctx, taskID, lane, s.now()); err != nil {
		log.Printf("youtrack.cache_lane_failed task_id=%d err=%q", taskID, err)
	}
}

// projectForTask resolves the YouTrack project for an hk project. An unmapped
// hk project uses OTHER; a missing YouTrack project is an error the push
// reports as a warning (the sync never creates projects). persist is false in
// dry-run mode, which must not write the resolved id back to PG.
func (s *Service) projectForTask(ctx context.Context, hkProject string, persist bool) (store.YouTrackProject, string, error) {
	hkProject = strings.TrimSpace(hkProject)
	if hkProject == "" {
		hkProject = s.project
	}
	p, found, err := s.store.YouTrackProjectFor(ctx, hkProject)
	if err != nil {
		return store.YouTrackProject{}, "", err
	}
	if !found {
		return store.YouTrackProject{}, "", ErrProjectMissing
	}
	id, err := s.resolveProjectID(ctx, p, persist)
	if err != nil {
		return store.YouTrackProject{}, "", err
	}
	return p, id, nil
}

func (s *Service) resolveProjectID(ctx context.Context, p store.YouTrackProject, persist bool) (string, error) {
	if p.ProjectID != "" {
		return p.ProjectID, nil
	}
	s.projectMu.Lock()
	defer s.projectMu.Unlock()
	if id := s.projectIDs[p.HKProject]; id != "" {
		return id, nil
	}
	id, err := s.client.ProjectID(ctx, p.ShortName)
	if err != nil {
		return "", err
	}
	s.projectIDs[p.HKProject] = id
	if persist {
		if err := s.store.SetYouTrackProjectID(ctx, p.HKProject, id); err != nil {
			log.Printf("youtrack.project_id_store_failed hk_project=%q err=%q", p.HKProject, err)
		}
	}
	return id, nil
}

// ensureValue makes a state or enum value exist before a write. A value that
// is already in the bundle costs one cached lookup.
func (s *Service) ensureValue(ctx context.Context, field, value string, resolved bool) error {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	s.fieldMu.Lock()
	defer s.fieldMu.Unlock()
	info, ok := s.fields[field]
	if ok {
		if _, present := info.Values[normalizeLabel(value)]; present {
			return nil
		}
	} else {
		_, projectID, err := s.projectForTask(ctx, s.project, true)
		if err != nil {
			return err
		}
		info, err = s.client.ProjectFieldValues(ctx, projectID, field)
		if err != nil {
			return err
		}
	}
	if err := s.client.AddEnumValue(ctx, info, value, resolved); err != nil {
		// Another process may have added the value between our read and the
		// write. Re-read once; a value that is now present is success.
		if IsTemporary(err) {
			return err
		}
		_, projectID, perr := s.projectForTask(ctx, s.project, true)
		if perr != nil {
			return err
		}
		fresh, ferr := s.client.ProjectFieldValues(ctx, projectID, field)
		if ferr != nil {
			return err
		}
		if _, present := fresh.Values[normalizeLabel(value)]; !present {
			return err
		}
		s.fields[field] = fresh
		return nil
	}
	if info.Values == nil {
		info.Values = map[string]string{}
	}
	info.Values[normalizeLabel(value)] = value
	s.fields[field] = info
	return nil
}

func (s *Service) stateFieldValue(ctx context.Context, state string) (CustomField, error) {
	label := StateLabel(state)
	if err := s.ensureValue(ctx, s.schema.StateField, label, StateResolved(state)); err != nil {
		return CustomField{}, err
	}
	return StateField(s.schema.StateField, label, StateResolved(state)), nil
}

func (s *Service) enumFieldValue(ctx context.Context, field, value string) (CustomField, bool, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return CustomField{}, false, nil
	}
	if err := s.ensureValue(ctx, field, value, false); err != nil {
		return CustomField{}, false, err
	}
	return EnumField(field, value), true, nil
}

func (s *Service) revert(ctx context.Context, issueID, field, value, refusal string) error {
	if strings.TrimSpace(issueID) == "" {
		return fmt.Errorf("no issue id for the revert")
	}
	switch field {
	case "state":
		state := strings.TrimSpace(value)
		if state == "" {
			return nil
		}
		label := StateLabel(state)
		if err := s.ensureValue(ctx, s.schema.StateField, label, StateResolved(state)); err != nil {
			return err
		}
		if err := s.client.SetStateValue(ctx, issueID, s.schema.StateField, label, StateResolved(state)); err != nil {
			return err
		}
	case "priority":
		n, err := strconv.Atoi(strings.TrimSpace(value))
		if err != nil {
			return fmt.Errorf("revert priority %q: %w", value, err)
		}
		if err := s.client.SetPriority(ctx, issueID, s.schema.PriorityField, n); err != nil {
			return err
		}
	case "lane":
		lane := strings.TrimSpace(value)
		if lane == "" {
			return nil
		}
		if err := s.ensureValue(ctx, s.schema.LaneField, lane, false); err != nil {
			return err
		}
		if err := s.client.SetLane(ctx, issueID, s.schema.LaneField, lane); err != nil {
			return err
		}
	}
	reason := strings.TrimSpace(refusal)
	if reason == "" {
		reason = "hk가 이 변경을 적용하지 않았습니다"
	}
	_, err := s.client.AddComment(ctx, issueID, "hk가 거부: "+reason)
	return err
}
