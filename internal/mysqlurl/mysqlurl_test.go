package mysqlurl

import (
	"testing"
	"time"
)

func TestParse(t *testing.T) {
	cfg, err := Parse("mysql://test:password@localhost:3306/testdb?tls=true")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Net != "tcp" || cfg.Addr != "localhost:3306" || cfg.DBName != "testdb" || cfg.User != "test" || cfg.Passwd != "password" || !cfg.ParseTime || cfg.Loc != time.UTC || cfg.TLSConfig != "true" {
		t.Fatalf("unexpected config: %#v", cfg)
	}
	if cfg.Params["time_zone"] != "'+00:00'" || cfg.Params["transaction_isolation"] != "'READ-COMMITTED'" {
		t.Fatalf("incorrect session settings: %v", cfg.Params)
	}
}

func TestParseRejectsSessionOverrides(t *testing.T) {
	for _, raw := range []string{"postgres://a@localhost/db", "mysql://a@localhost/db?time_zone=UTC", "mysql://a@localhost/db?parseTime=false", "mysql://a@localhost/db?transaction_isolation=SERIALIZABLE", "mysql://a@localhost/db?loc=Local"} {
		if _, err := Parse(raw); err == nil {
			t.Errorf("Parse(%q) accepted invalid URL", raw)
		}
	}
}
