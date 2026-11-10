package mcp

import (
	"context"
	"strings"
	"time"

	"github.com/mgh3326/handoffkeep/internal/guard"
	"github.com/mgh3326/handoffkeep/internal/store"
	gmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

// syncRecentWindow is how fresh the newest sync timestamp must be for
// sync_status to call the sync recent. The Dagu job runs every two minutes;
// the window tolerates a few missed runs without hiding a stalled sync.
const syncRecentWindow = 15 * time.Minute

const syncWriterNote = "The Python mirror may still be the active writer until the Dagu job switches (docs/youtrack.md)."

// registerMetaTools adds the read-only project, state-machine and YouTrack
// sync status tools.
func registerMetaTools(s *gmcp.Server, o operatorTools) {
	gmcp.AddTool(s, &gmcp.Tool{
		Name:        "fleet_capacity",
		Description: "현재 quota 관측, host-account presence, 최근 placement 시도를 보여줍니다. accounts는 account_key별 pool과 window 사용률, 리셋 시각, 관측 나이를 담습니다. presence는 host별 관측된 각 account_key와 해당 계정의 마지막 관측 나이를 담습니다. placements는 job, host, since 필터를 지원하며 기본 20건, 최대 100건을 최신순으로 반환합니다. 각 시도에는 job_id, 결정 시각, 결과와 사유, 선택된 host/profile/model, 후보 host별 결과와 사유가 포함됩니다. 결과는 저장된 증거이며 어떤 변경이나 spawn도 지시하지 않습니다. hk에는 panewire의 quota staleness 기준이 없어 age_seconds만 보고하고 stale 판정은 하지 않습니다. 기록이 없으면 placement_note에 no placement records라고 표시합니다.",
		Annotations: readOnlyAnnotations(),
	}, o.fleetCapacity)
	gmcp.AddTool(s, &gmcp.Tool{
		Name:        "projects_list",
		Description: "List every hk task project with its design-session owner (design_lane and effective_owner), status, start and target dates, open task counts by state, and health: in_progress, waiting, blocked, done and overdue counts, open assignees, and the top blocked tasks (with blocker ids) and overdue tasks. as_of is the server time the counts were read at. Read-only.",
		Annotations: readOnlyAnnotations(),
	}, o.projectsList)
	gmcp.AddTool(s, &gmcp.Tool{
		Name:        "task_states",
		Description: "Describe the hk task state machine: every state with its category, terminal and claimable flags, allowed manual transitions, and the closure requirement for settled states. The values are derived from the tables the store enforces.",
		Annotations: readOnlyAnnotations(),
	}, o.taskStates)
	gmcp.AddTool(s, &gmcp.Tool{
		Name:        "sync_status",
		Description: "Report YouTrack sync health: cursor, pending outbox writes (count, oldest pending age, max attempts, last error sample), processed activities in the last 24 hours by outcome, mirrored task count, and the last run time derived from sync timestamps (\"never\" when there is none). The Python mirror may still be the active writer until the Dagu job switches (docs/youtrack.md).",
		Annotations: readOnlyAnnotations(),
	}, o.syncStatus)
}

func (o operatorTools) projectsList(ctx context.Context, _ *gmcp.CallToolRequest, _ struct{}) (*gmcp.CallToolResult, any, error) {
	asOf := time.Now().UTC()
	projects, err := o.service.Store.ListProjectOverviews(ctx, asOf)
	if err != nil {
		return nil, nil, err
	}
	return jsonResult(map[string]any{"as_of": asOf, "projects": projects})
}

func (o operatorTools) taskStates(_ context.Context, _ *gmcp.CallToolRequest, _ struct{}) (*gmcp.CallToolResult, any, error) {
	return jsonResult(map[string]any{
		"states":                 store.TaskStateMachine(),
		"categories":             store.TaskCategories(),
		"accepted_state_names":   store.AcceptedStateNames(),
		"legacy_state_names":     store.LegacyTaskStates(),
		"awaiting_decision_flag": store.AwaitingDecisionKey,
	})
}

type syncCursorView struct {
	Cursor         string    `json:"cursor"`
	LastActivityID string    `json:"last_activity_id"`
	UpdatedAt      time.Time `json:"updated_at"`
}

type syncOutboxView struct {
	Pending                 int        `json:"pending"`
	OldestPendingAt         *time.Time `json:"oldest_pending_at"`
	OldestPendingAgeSeconds *int64     `json:"oldest_pending_age_seconds"`
	MaxAttempts             int        `json:"max_attempts"`
	LastError               string     `json:"last_error,omitempty"`
}

type syncStatusView struct {
	Provider            string          `json:"provider"`
	Cursor              *syncCursorView `json:"cursor"`
	LastRun             string          `json:"last_run"`
	LastRunAt           *time.Time      `json:"last_run_at"`
	LastRunAgeSeconds   *int64          `json:"last_run_age_seconds"`
	Recent              bool            `json:"recent"`
	RecentWindowSeconds int             `json:"recent_window_seconds"`
	Outbox              syncOutboxView  `json:"outbox"`
	Processed24h        map[string]int  `json:"processed_24h"`
	Processed24hTotal   int             `json:"processed_24h_total"`
	MirroredTasks       int             `json:"mirrored_tasks"`
	PendingReverts      int             `json:"pending_reverts"`
	WriterNote          string          `json:"writer_note"`
}

func (o operatorTools) syncStatus(ctx context.Context, _ *gmcp.CallToolRequest, _ struct{}) (*gmcp.CallToolResult, any, error) {
	now := time.Now().UTC()
	status, err := o.service.Store.YouTrackSyncStatus(ctx, now)
	if err != nil {
		return nil, nil, err
	}
	out := syncStatusView{
		Provider:            store.YouTrackProvider,
		LastRun:             "never",
		RecentWindowSeconds: int(syncRecentWindow / time.Second),
		Outbox: syncOutboxView{
			Pending:     status.OutboxPending,
			MaxAttempts: status.OutboxMaxAttempts,
			LastError:   safeErrorSample(status.OutboxLastError),
		},
		Processed24h:      status.Processed24h,
		Processed24hTotal: status.Processed24hTotal,
		MirroredTasks:     status.MirroredTasks,
		PendingReverts:    status.PendingReverts,
		WriterNote:        syncWriterNote,
	}
	if status.Cursor != nil {
		out.Cursor = &syncCursorView{
			Cursor: status.Cursor.Cursor, LastActivityID: status.Cursor.LastActivityID, UpdatedAt: status.Cursor.UpdatedAt,
		}
	}
	if status.OutboxOldestPendingAt != nil {
		age := int64(now.Sub(*status.OutboxOldestPendingAt).Seconds())
		out.Outbox.OldestPendingAt = status.OutboxOldestPendingAt
		out.Outbox.OldestPendingAgeSeconds = &age
	}
	if status.LastRunAt != nil {
		age := int64(now.Sub(*status.LastRunAt).Seconds())
		out.LastRun = status.LastRunAt.Format(time.RFC3339)
		out.LastRunAt = status.LastRunAt
		out.LastRunAgeSeconds = &age
		out.Recent = now.Sub(*status.LastRunAt) <= syncRecentWindow
	}
	return jsonResult(out)
}

// safeErrorSample keeps a stored sync error readable while refusing to return
// credential-shaped text.
func safeErrorSample(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	if pattern := guard.Match(value); pattern != "" {
		return "[redacted: " + pattern + "]"
	}
	return value
}
