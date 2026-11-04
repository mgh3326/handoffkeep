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
		{`{"name":"x","set_target":true}`, false},
		{`{"name":null}`, false},
		{`{"description":null}`, false},
		{`{"status":null}`, false},
		{`{"sort_order":null}`, false},
		{`{"project":null}`, false},
		{`{"description":""}`, true},
	} {
		var p MilestonePatch
		err := json.Unmarshal([]byte(tc.data), &p)
		if (err == nil) != tc.valid {
			t.Errorf("%s: err=%v", tc.data, err)
		}
	}
}

func TestMilestonePatchMarshalClearsTarget(t *testing.T) {
	name := "x"
	for _, tc := range []struct {
		patch MilestonePatch
		want  string
	}{
		{MilestonePatch{Name: &name}, `{"name":"x","set_target":false}`},
		{MilestonePatch{SetTarget: true}, `{"set_target":true,"target_at":null}`},
	} {
		got, err := json.Marshal(tc.patch)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != tc.want {
			t.Errorf("got %s want %s", got, tc.want)
		}
		var back MilestonePatch
		if err := json.Unmarshal(got, &back); err != nil {
			t.Errorf("round trip %s: %v", got, err)
		}
	}
}
