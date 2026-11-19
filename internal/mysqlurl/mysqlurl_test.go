package mysqlurl

import (
	"strings"
	"testing"
	"time"
)

func TestParse(t *testing.T) {
	t.Setenv(caPEMEnv, "")
	cfg, err := Parse("mysql://test:password@localhost:3306/testdb?tls=true")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Net != "tcp" || cfg.Addr != "localhost:3306" || cfg.DBName != "testdb" || cfg.User != "test" || cfg.Passwd != "password" || !cfg.ParseTime || !cfg.ClientFoundRows || cfg.Loc != time.UTC || cfg.TLSConfig != "true" {
		t.Fatalf("unexpected config: %#v", cfg)
	}
	if cfg.Params["time_zone"] != "'+00:00'" || cfg.Params["transaction_isolation"] != "'READ-COMMITTED'" {
		t.Fatalf("incorrect session settings: %v", cfg.Params)
	}
	if cfg.Params["collation_connection"] != "'utf8mb4_0900_bin'" {
		t.Fatalf("connection collation is %q, want utf8mb4_0900_bin", cfg.Params["collation_connection"])
	}
}

func TestParseMalformedURLDoesNotEchoCredentials(t *testing.T) {
	const raw = "mysql://copy-user:secret%zz@localhost:3306/handoffkeep"
	_, err := Parse(raw)
	if err == nil {
		t.Fatal("Parse accepted malformed URL")
	}
	if got := err.Error(); got != "malformed database URL" || strings.Contains(got, "secret") || strings.Contains(got, raw) {
		t.Fatalf("malformed URL error=%q, want a fixed message with no credentials", got)
	}
}

func TestParseRejectsSessionOverrides(t *testing.T) {
	for _, raw := range []string{"postgres://a@localhost/db", "mysql://a@localhost/db?time_zone=UTC", "mysql://a@localhost/db?parseTime=false", "mysql://a@localhost/db?transaction_isolation=SERIALIZABLE", "mysql://a@localhost/db?loc=Local", "mysql://a@localhost/db?collation_connection=utf8mb4_0900_ai_ci"} {
		if _, err := Parse(raw); err == nil {
			t.Errorf("Parse(%q) accepted invalid URL", raw)
		}
	}
}
