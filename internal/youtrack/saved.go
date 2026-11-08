package youtrack

import (
	"context"
	"net/url"
)

// SavedQuery is one saved search the operator reads YouTrack through.
type SavedQuery struct {
	Name  string
	Query string
}

// savedQueries are the searches the Python mirror created. They are created
// by name, so a run that finds one already there writes nothing.
var savedQueries = []SavedQuery{
	{Name: "hk: 진행 중", Query: "project: HK hk 상태: {개발 중}, {검토 중}, {조사 중} sort by: updated desc"},
	{Name: "hk: 콘솔 조작 개편", Query: "project: HK 레인: handoffkeep-design #Unresolved sort by: {hk ID} asc"},
	{Name: "hk: 오늘 바뀐 것", Query: "project: HK updated: Today sort by: updated desc"},
	{Name: "hk: 미해결 전체", Query: "project: HK #Unresolved sort by: {hk 우선순위} asc"},
	{Name: "결정: 대기 중", Query: "project: DEC #Unresolved sort by: {결정 기한} asc, created desc"},
}

// EnsureSavedQueries creates the named searches that do not exist yet and
// returns how many it created.
func (c *Client) EnsureSavedQueries(ctx context.Context, queries []SavedQuery) (int, error) {
	q := url.Values{}
	q.Set("fields", "name")
	q.Set("$top", "500")
	var existing []struct {
		Name string `json:"name"`
	}
	if err := c.get(ctx, "/api/savedQueries", q, &existing); err != nil {
		return 0, err
	}
	have := make(map[string]bool, len(existing))
	for _, e := range existing {
		have[e.Name] = true
	}
	created := 0
	for _, sq := range queries {
		if have[sq.Name] {
			continue
		}
		body := map[string]string{"name": sq.Name, "query": sq.Query}
		if err := c.postOnce(ctx, "/api/savedQueries", url.Values{"fields": {"id"}}, body, nil); err != nil {
			return created, err
		}
		created++
	}
	return created, nil
}
