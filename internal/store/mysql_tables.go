package store

import "context"

// mysqlTableExists reports whether the schema has a table. The tasks area uses
// it where it would read or write a table of an area that is not ported yet, so
// that "the area has no records" is decided by the schema, not assumed. A table
// never disappears while a store is open, so only a positive answer is cached.
func (s *Store) mysqlTableExists(ctx context.Context, q rowQuerier, name string) (bool, error) {
	if _, ok := s.tablesSeen.Load(name); ok {
		return true, nil
	}
	var n int
	if err := q.QueryRow(ctx, `SELECT COUNT(*) FROM information_schema.tables WHERE table_schema=DATABASE() AND table_name=?`, name).Scan(&n); err != nil {
		return false, err
	}
	if n == 0 {
		return false, nil
	}
	s.tablesSeen.Store(name, true)
	return true, nil
}
