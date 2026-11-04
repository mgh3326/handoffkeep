package store

import (
	"encoding/json"
	"testing"
)

func TestProjectMetadataPatchJSON(t *testing.T) {
	for _, tc := range []struct {
		data  string
		valid bool
	}{
		{`{"start_at":"2026-01-01T00:00:00Z"}`, false},
		{`{"start_at":null}`, false},
		{`{"target_at":null}`, false},
		{`{"set_start":true,"start_at":null}`, true},
		{`{"set_target":true,"target_at":"2026-01-01T00:00:00Z"}`, true},
		{`{"unknown":1}`, false},
	} {
		var p ProjectMetadataPatch
		err := json.Unmarshal([]byte(tc.data), &p)
		if (err == nil) != tc.valid {
			t.Errorf("%s: err=%v", tc.data, err)
		}
	}
}
