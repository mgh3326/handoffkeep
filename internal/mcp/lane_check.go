package mcp

import (
	"context"
	"errors"
	"strings"
	"time"

	gmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

// lane_look records a lane's look at its lane_check notice (operator decision
// 418, hk 5201). The findings hash comes from the notice id, so a malformed or
// another lane's id is refused; a repeat of the same look changes nothing.
func registerLaneCheckTools(s *gmcp.Server, o operatorTools) {
	gmcp.AddTool(s, &gmcp.Tool{
		Name:        "lane_look",
		Description: "Record that this design lane looked at the state its lane_check notice named. notice is the lanecheck-<lane>-<sha8>-<bucket> event id from the wake; lane names the lane being looked at. The check skips a lane whose last look carries the current findings hash, and a change of state re-notices it. note is optional. A repeat of the same (lane, notice) changes nothing. Returns changed and the look.",
		Annotations: writeAnnotations(),
	}, o.laneLook)
}

type laneLookInput struct {
	Lane   string `json:"lane"`
	Notice string `json:"notice"`
	Note   string `json:"note,omitempty"`
}

func (o operatorTools) laneLook(ctx context.Context, _ *gmcp.CallToolRequest, in laneLookInput) (*gmcp.CallToolResult, any, error) {
	lane := strings.TrimSpace(in.Lane)
	if lane == "" {
		return nil, nil, errors.New("lane is required")
	}
	look, changed, err := o.service.Store.RecordLaneLook(ctx, lane, strings.TrimSpace(in.Notice), strings.TrimSpace(in.Note), writeAuthor(o.principal), time.Now().UTC())
	if err != nil {
		return nil, nil, err
	}
	return jsonResult(map[string]any{"changed": changed, "look": look})
}
