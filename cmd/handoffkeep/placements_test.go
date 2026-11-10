package main

import (
	"bytes"
	"encoding/json"
	"io"
	"reflect"
	"strings"
	"testing"
)

func TestPlacementsListCLI(t *testing.T) {
	const response = `{"placements":[{"id":7,"job_id":"hk3892-x","chosen_host":"desktop","outcome":"placed"}]}`
	h, calls := decisionCLIServer(t, response)
	var out bytes.Buffer
	args := []string{"placements", "list", "--limit", "5", "--job", "hk3892-x", "--since", "2026-11-01T00:00:00+09:00", "--host", "desktop", "--url", h.URL, "--token", "tok"}
	if err := run(args, &out, io.Discard); err != nil {
		t.Fatal(err)
	}
	// The server's fields come back with their values; the typed decode may
	// add zero-valued fields the fixture omits, so compare what the server sent.
	var got, want struct {
		Placements []map[string]any `json:"placements"`
	}
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("printed %q: %v", out.String(), err)
	}
	_ = json.Unmarshal([]byte(response), &want)
	for i, row := range want.Placements {
		for k, v := range row {
			if !reflect.DeepEqual(got.Placements[i][k], v) {
				t.Fatalf("placement %d field %s = %v, want %v (printed %s)", i, k, got.Placements[i][k], v, out.String())
			}
		}
	}
	if len(*calls) != 1 {
		t.Fatalf("calls=%+v", *calls)
	}
	c := (*calls)[0]
	if c.Method != "GET" || c.Path != "/v1/placements" {
		t.Fatalf("call=%+v", c)
	}
	// since is sent in UTC; url.Values sorts keys.
	if want := "host=desktop&job=hk3892-x&limit=5&since=2026-10-31T15%3A00%3A00Z"; c.Query != want {
		t.Fatalf("query=%q want %q", c.Query, want)
	}
}

func TestPlacementsListCLINoFlagsSendsNoQuery(t *testing.T) {
	h, calls := decisionCLIServer(t, `{"placements":[]}`)
	var out bytes.Buffer
	if err := run([]string{"placements", "list", "--url", h.URL, "--token", "tok"}, &out, io.Discard); err != nil {
		t.Fatal(err)
	}
	if c := (*calls)[0]; c.Path != "/v1/placements" || c.Query != "" {
		t.Fatalf("call=%+v", c)
	}
}

func TestPlacementsListCLIRefusesBadFlagsBeforeCalling(t *testing.T) {
	cases := map[string][]string{
		"zero limit":     {"--limit", "0"},
		"over max":       {"--limit", "1001"},
		"non-numeric":    {"--limit", "many"},
		"bad since":      {"--since", "yesterday"},
		"stray position": {"extra"},
	}
	for name, flags := range cases {
		t.Run(name, func(t *testing.T) {
			h, calls := decisionCLIServer(t, `{"placements":[]}`)
			args := append([]string{"placements", "list"}, flags...)
			args = append(args, "--url", h.URL, "--token", "tok")
			err := run(args, io.Discard, io.Discard)
			if err == nil || !strings.Contains(err.Error(), "placements") {
				t.Fatalf("err=%v, want a placements usage refusal", err)
			}
			if len(*calls) != 0 {
				t.Fatalf("refused input still called the server: %+v", *calls)
			}
		})
	}
}

func TestPlacementsUsageIsNamedInTopLevelUsage(t *testing.T) {
	err := run([]string{"nope"}, io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "|placements|") {
		t.Fatalf("usage=%v", err)
	}
}
