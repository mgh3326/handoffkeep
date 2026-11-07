package main

import "testing"

func TestMCPAccessFromEnv(t *testing.T) {
	for _, tc := range []struct {
		name              string
		team, aud, emails string
		wantConfigured    bool
	}{
		{"missing all", "", "", "", false},
		{"missing audience", "team.example", "", "operator@example.com", false},
		{"missing allowlist", "team.example", "mcp-aud", "", false},
		{"complete", "team.example", "mcp-aud", "operator@example.com", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HANDOFFKEEP_MCP_CF_TEAM_DOMAIN", tc.team)
			t.Setenv("HANDOFFKEEP_MCP_CF_AUD", tc.aud)
			t.Setenv("HANDOFFKEEP_MCP_ALLOWED_EMAILS", tc.emails)
			access, err := mcpAccessFromEnv()
			if err != nil {
				t.Fatal(err)
			}
			if configured := access != nil; configured != tc.wantConfigured {
				t.Fatalf("configured=%v want %v", configured, tc.wantConfigured)
			}
		})
	}
}
