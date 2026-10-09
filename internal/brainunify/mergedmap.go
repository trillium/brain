package brainunify

import (
	"context"
	"fmt"
)

// LoadMergedMapping reads, from a merged database a running server hosts, which
// database each store was merged from (brain_stores.store -> source_database).
func LoadMergedMapping(ctx context.Context, host string, port int, database string) (map[string]string, error) {
	conn, err := OpenSource(host, port)
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close() }()
	rows, err := conn.query(ctx, fmt.Sprintf("select `store`, `source_database` from `%s`.`brain_stores`", database))
	if err != nil {
		return nil, fmt.Errorf("reading which database each store was merged from: %w", err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var store, db string
		if err := rows.Scan(&store, &db); err != nil {
			return nil, err
		}
		out[store] = db
	}
	return out, rows.Err()
}

// ApplyMergedMapping makes the registry say where each store's own database is,
// from the merged database's record, and marks the merged database as never a
// source. Use it whenever the merged database is hosted on the sources' server.
func (r *Registry) ApplyMergedMapping(database string, mapping map[string]string) {
	r.MergedDatabase = database
	r.SourceDatabases = mapping
}
