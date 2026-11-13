package mcp

import (
	"context"
	"time"

	"github.com/mgh3326/handoffkeep/internal/store"
	gmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

// registerMetaTools adds the read-only project and state-machine tools.
func registerMetaTools(s *gmcp.Server, o operatorTools) {
	gmcp.AddTool(s, &gmcp.Tool{
		Name:        "fleet_capacity",
		Description: "현재 quota 관측, host-account presence, 최근 placement 시도를 보여줍니다. accounts는 account_key별 pool과 window 사용률, 리셋 시각, 관측 나이를 담습니다. 관측 나이는 confirmed_at을 우선 사용하고, 없으면 measured_at을 사용합니다. 수동 quota에는 저장된 만료 시각과 만료 여부도 포함합니다. Devin free window에는 available, exhausted, unknown 상태가 표시됩니다. presence는 host별 관측된 각 account_key와 해당 계정의 마지막 관측 나이를 담습니다. placements는 job, host, since 필터를 지원하며 기본 20건, 최대 100건을 최신순으로 반환합니다. 각 시도에는 job_id, 결정 시각, 결과와 사유, 선택된 host/profile/model, 후보 host별 결과와 사유가 포함됩니다. 결과는 저장된 증거이며 어떤 변경이나 spawn도 지시하지 않습니다. hk에는 panewire quota의 stale 판정 기준이 없어 age_seconds에 stale 판정을 붙이지 않으며, 수동 quota의 expired 표시는 저장된 명시적 만료 시각으로 판단합니다. 기록이 없으면 placement_note에 no placement records라고 표시합니다.",
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
