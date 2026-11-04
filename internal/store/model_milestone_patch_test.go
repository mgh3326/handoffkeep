package store

import (
	"encoding/json"
	"testing"
)

func TestMilestonePatchJSON(t *testing.T) {
	for _, tc := range []struct {
		data  string
		valid bool
	}{
		{`{"target_at":null}`, false},
		{`{"target_at":"2026-01-01T00:00:00Z"}`, false},
		{`{"set_target":true,"target_at":null}`, true},
		{`{"name":"new"}`, true},
		{`{"unknown":1}`, false},
	} {
		var p MilestonePatch
		err := json.Unmarshal([]byte(tc.data), &p)
		if (err == nil) != tc.valid {
			t.Errorf("%s: err=%v", tc.data, err)
		}
	}
}
