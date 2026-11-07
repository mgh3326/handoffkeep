package youtrack

import (
	"context"
	"fmt"
	"log"
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

	GetYouTrackLink(ctx context.Context, taskID int64) (store.YouTrackLink, bool, error)
	FindYouTrackLinkByReadableID(ctx context.Context, readableID string) (store.YouTrackLink, bool, error)
	ListYouTrackLinks(ctx context.Context, taskIDs []int64) (map[int64]store.YouTrackLink, error)
	LinkYouTrackIssue(ctx context.Context, taskID int64, issueID, readableID string, at time.Time) (store.YouTrackLink, error)
	MarkYouTrackTaskPushed(ctx context.Context, taskID int64, updatedAt time.Time) error
	TouchYouTrackTaskPushed(ctx context.Context, taskID int64) error

	GetYouTrackFieldCache(ctx context.Context, taskID int64) (store.YouTrackFieldCache, bool, error)
	GetYouTrackFieldCaches(ctx context.Context, taskIDs []int64) (map[int64]store.YouTrackFieldCache, error)
	PutYouTrackFieldCache(ctx context.Context, x store.YouTrackFieldCache) error
	SetYouTrackCachedPriority(ctx context.Context, taskID int64, priority int, at time.Time) error
	SetYouTrackCachedLane(ctx context.Context, taskID int64, lane string, at time.Time) error
	MarkYouTrackFieldCacheStale(ctx context.Context, taskID int64) error
	RefreshYouTrackFields(ctx context.Context, ttl time.Duration, limit int) ([]store.YouTrackLink, error)

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
	CacheTTL         time.Duration
	DefaultHKProject string
	DefaultLane      string
	ConsoleBaseURL   string
	SeedLimit        int
	Now              func() time.Time
}

const (
	defaultCacheTTL       = 60 * time.Second
	defaultHKProject      = "other"
	defaultLane           = "operator-desk"
	defaultConsoleBaseURL = "https://console.robinco.dev"
	defaultSeedLimit      = 10
)

type Service struct {
	store   Store
	client  *Client
	schema  Schema
	ttl     time.Duration
	project string
	lane    string
	console string
	seeds   int
	now     func() time.Time

	projectMu     sync.Mutex
	projectCached string

	fieldMu sync.Mutex
	fields  map[string]FieldInfo
}

func NewService(st Store, client *Client, opt Options) *Service {
	if opt.Schema.Project == "" {
		opt.Schema = DefaultSchema()
	}
	if opt.CacheTTL <= 0 {
		opt.CacheTTL = defaultCacheTTL
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
		store:   st,
		client:  client,
		schema:  opt.Schema,
		ttl:     opt.CacheTTL,
		project: opt.DefaultHKProject,
		lane:    opt.DefaultLane,
		console: opt.ConsoleBaseURL,
		seeds:   opt.SeedLimit,
		now:     opt.Now,
		fields:  map[string]FieldInfo{},
	}
}

func (s *Service) Schema() Schema { return s.schema }

// Overlay replaces priority and lane on linked tasks with the cached
// YouTrack-owned values and marks where they came from. An unlinked task is
// left exactly as it was.
func (s *Service) Overlay(ctx context.Context, tasks []store.Task) {
	if s == nil || s.client == nil || len(tasks) == 0 {
		return
	}
	ids := make([]int64, 0, len(tasks))
	for _, task := range tasks {
		ids = append(ids, task.ID)
	}
	links, err := s.store.ListYouTrackLinks(ctx, ids)
	if err != nil {
		log.Printf("youtrack.overlay_links_failed err=%q", err)
		return
	}
	if len(links) == 0 {
		return
	}
	caches, err := s.store.GetYouTrackFieldCaches(ctx, ids)
	if err != nil {
		log.Printf("youtrack.overlay_cache_failed err=%q", err)
		return
	}
	seeds := s.seeds
	for i := range tasks {
		link, linked := links[tasks[i].ID]
		if !linked {
			continue
		}
		cache, cached := caches[tasks[i].ID]
		if !cached {
			if seeds <= 0 {
				continue
			}
			seeds--
			cache, _ = s.fetchCache(ctx, link, tasks[i])
			if cache.TaskID == 0 {
				continue
			}
		}
		tasks[i].Priority = cache.Priority
		tasks[i].Lane = cache.Lane
		tasks[i].PrioritySource = "youtrack"
		tasks[i].LaneSource = "youtrack"
		tasks[i].Stale = cache.Stale
	}
}

func (s *Service) fetchCache(ctx context.Context, link store.YouTrackLink, task store.Task) (store.YouTrackFieldCache, error) {
	issue, err := s.client.GetIssue(ctx, link.IssueID)
	if err != nil {
		// Keep the last known value and say it is stale. On the very first
		// read there is no YouTrack value yet, so the task's own columns are
		// the best last-known value.
		cache := store.YouTrackFieldCache{TaskID: task.ID, Priority: task.Priority, Lane: task.Lane, FetchedAt: s.now(), Stale: true}
		if perr := s.store.PutYouTrackFieldCache(ctx, cache); perr != nil {
			log.Printf("youtrack.cache_write_failed task_id=%d err=%q", task.ID, perr)
		}
		return cache, err
	}
	cache := s.cacheFromIssue(task.ID, issue, task)
	if perr := s.store.PutYouTrackFieldCache(ctx, cache); perr != nil {
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
	return store.YouTrackFieldCache{TaskID: taskID, Priority: priority, Lane: lane, FetchedAt: s.now()}
}

// PreparePriority writes an hk priority change to YouTrack first. An
// unlinked task is untouched, so its behaviour is exactly what it was.
func (s *Service) PreparePriority(ctx context.Context, taskID int64, priority int) error {
	if s == nil || s.client == nil {
		return nil
	}
	link, ok, err := s.store.GetYouTrackLink(ctx, taskID)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}
	if err := s.client.SetPriority(ctx, link.IssueID, s.schema.PriorityField, priority); err != nil {
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	return nil
}

// CachePriority records an hk-originated priority write on both the cache and
// the push watermark. Call it only after the store write succeeded.
func (s *Service) CachePriority(ctx context.Context, taskID int64, priority int) {
	if s == nil {
		return
	}
	if err := s.store.SetYouTrackCachedPriority(ctx, taskID, priority, s.now()); err != nil {
		log.Printf("youtrack.cache_priority_failed task_id=%d err=%q", taskID, err)
		return
	}
	if err := s.store.TouchYouTrackTaskPushed(ctx, taskID); err != nil {
		log.Printf("youtrack.touch_pushed_failed task_id=%d err=%q", taskID, err)
	}
}

// PrepareLane writes an hk lane change to YouTrack first, adding the lane to
// the enum when the operator has not used it there yet.
func (s *Service) PrepareLane(ctx context.Context, taskID int64, lane string) error {
	if s == nil || s.client == nil {
		return nil
	}
	link, ok, err := s.store.GetYouTrackLink(ctx, taskID)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}
	if err := s.ensureValue(ctx, s.schema.LaneField, lane, false); err != nil {
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	if err := s.client.SetLane(ctx, link.IssueID, s.schema.LaneField, lane); err != nil {
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	return nil
}

func (s *Service) CacheLane(ctx context.Context, taskID int64, lane string) {
	if s == nil {
		return
	}
	if err := s.store.SetYouTrackCachedLane(ctx, taskID, lane, s.now()); err != nil {
		log.Printf("youtrack.cache_lane_failed task_id=%d err=%q", taskID, err)
		return
	}
	if err := s.store.TouchYouTrackTaskPushed(ctx, taskID); err != nil {
		log.Printf("youtrack.touch_pushed_failed task_id=%d err=%q", taskID, err)
	}
}

func (s *Service) projectID(ctx context.Context) (string, error) {
	s.projectMu.Lock()
	defer s.projectMu.Unlock()
	if s.projectCached != "" {
		return s.projectCached, nil
	}
	id, err := s.client.ProjectID(ctx, s.schema.Project)
	if err != nil {
		return "", err
	}
	s.projectCached = id
	return id, nil
}

// ensureValue makes a state or enum value exist before a write. A value that
// is already in the bundle costs one cached lookup.
func (s *Service) ensureValue(ctx context.Context, field, value string, resolved bool) error {
	value = trim(value)
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
		projectID, err := s.projectID(ctx)
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
		projectID, perr := s.projectID(ctx)
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
	value = trim(value)
	if value == "" {
		return CustomField{}, false, nil
	}
	if err := s.ensureValue(ctx, field, value, false); err != nil {
		return CustomField{}, false, err
	}
	if field == s.schema.StateField {
		return StateField(field, value, false), true, nil
	}
	return EnumField(field, value), true, nil
}

func trim(v string) string { return strings.TrimSpace(v) }
