package dolt

import (
	"database/sql"
	"fmt"

	"github.com/go-sql-driver/mysql"
)

// BulkReadDB opens a short-lived read pool on the store's server for a
// caller that streams large text columns (brain recall pulls every matching
// comment body). It differs from the store's own pool in two ways: the
// client asks for MySQL protocol compression, which cuts a 24 MB comment
// fetch over the tailnet from about 3 s to about 1 s, and the pool allows
// maxConns concurrent connections so the caller can partition one large
// read into several streams. The driver only compresses when the server
// advertises CLIENT_COMPRESS; otherwise the packets stay plain, so the
// pool is always usable. The caller closes it.
func (s *DoltStore) BulkReadDB(maxConns int) (*sql.DB, error) {
	dsn, err := bulkReadDSN(s.connStr)
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, fmt.Errorf("failed to open bulk-read connection: %w", err)
	}
	if maxConns < 1 {
		maxConns = 1
	}
	db.SetMaxOpenConns(maxConns)
	db.SetMaxIdleConns(maxConns)
	return db, nil
}

// bulkReadDSN answers connStr with protocol compression requested and
// everything else (host, credentials, timeouts, TLS) unchanged.
func bulkReadDSN(connStr string) (string, error) {
	cfg, err := mysql.ParseDSN(connStr)
	if err != nil {
		return "", fmt.Errorf("failed to parse DSN for bulk-read connection: %w", err)
	}
	if err := cfg.Apply(mysql.EnableCompression(true)); err != nil {
		return "", fmt.Errorf("failed to enable compression for bulk-read connection: %w", err)
	}
	return cfg.FormatDSN(), nil
}
