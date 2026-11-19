// Package mysqlurl converts a mysql URL to a driver configuration.
package mysqlurl

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"
)

// Parse requires a TCP URL and configures every connection for UTC, the
// store's READ COMMITTED isolation level and the binary text collation the
// schema uses (design §11.4), so a literal compared on a connection cannot pick
// the server's case-insensitive default. When HANDOFFKEEP_MYSQL_CA_PEM is set,
// the connection uses the pinned CA instead of the URL's tls= (see tls.go).
func Parse(raw string) (*mysql.Config, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, errors.New("malformed database URL")
	}
	if u.Scheme != "mysql" || u.Host == "" || u.User == nil || strings.TrimPrefix(u.Path, "/") == "" || strings.Contains(strings.TrimPrefix(u.Path, "/"), "/") {
		return nil, fmt.Errorf("invalid mysql URL")
	}
	pinned, err := pinnedTLS()
	if err != nil {
		return nil, err
	}
	cfg := mysql.NewConfig()
	cfg.Net = "tcp"
	cfg.Addr = u.Host
	cfg.User = u.User.Username()
	cfg.Passwd, _ = u.User.Password()
	cfg.DBName = strings.TrimPrefix(u.Path, "/")
	cfg.ParseTime = true
	cfg.Loc = time.UTC
	// PostgreSQL stores timestamps at microsecond precision and pgx truncates
	// to it; the MySQL driver otherwise sends nanoseconds and the server
	// rounds DATETIME(6) up, which can put a stored time a few hundred
	// nanoseconds after the Go time that wrote it (hk 5077).
	if err := cfg.Apply(mysql.TimeTruncate(time.Microsecond)); err != nil {
		return nil, err
	}
	// An UPDATE reports the rows it matched, as PostgreSQL does, not only the
	// rows whose values changed: store code decides "not found" from it.
	cfg.ClientFoundRows = true
	cfg.Params = make(map[string]string)
	for key, values := range u.Query() {
		if len(values) != 1 {
			return nil, fmt.Errorf("mysql URL parameter %q must occur once", key)
		}
		switch key {
		case "time_zone", "transaction_isolation", "collation_connection", "parseTime", "loc", "timeTruncate":
			return nil, fmt.Errorf("mysql URL parameter %q is controlled by the store", key)
		case "tls":
			cfg.TLSConfig = values[0]
		default:
			cfg.Params[key] = values[0]
		}
	}
	if pinned != nil {
		if cfg.TLSConfig != "" && cfg.TLSConfig != pinnedTLSName {
			return nil, fmt.Errorf("mysql URL tls=%q conflicts with %s; use tls=%s or omit tls", cfg.TLSConfig, caPEMEnv, pinnedTLSName)
		}
		if err := checkPinnedParams(cfg.Params); err != nil {
			return nil, err
		}
		cfg.TLSConfig = pinnedTLSName
	}
	cfg.Params["time_zone"] = "'+00:00'"
	cfg.Params["transaction_isolation"] = "'READ-COMMITTED'"
	cfg.Params["collation_connection"] = "'utf8mb4_0900_bin'"
	return cfg, nil
}
