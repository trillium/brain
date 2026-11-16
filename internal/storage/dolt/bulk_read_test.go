package dolt

import (
	"strings"
	"testing"

	"github.com/go-sql-driver/mysql"
)

// The DSN handed to the driver is a serialized protocol artifact; its
// compress parameter is the whole point of the bulk-read pool.
func TestBulkReadDSNRequestsCompression(t *testing.T) {
	in := "brain_app:secret@tcp(100.64.0.1:3307)/brain_unified?readTimeout=30s&tls=false"
	got, err := bulkReadDSN(in)
	if err != nil {
		t.Fatalf("bulkReadDSN: %v", err)
	}
	cfg, err := mysql.ParseDSN(got)
	if err != nil {
		t.Fatalf("driver rejects the bulk-read DSN %q: %v", got, err)
	}
	if cfg.User != "brain_app" || cfg.Passwd != "secret" || cfg.Addr != "100.64.0.1:3307" || cfg.DBName != "brain_unified" {
		t.Fatalf("bulk-read DSN changed the connection target: %q", got)
	}
	if cfg.ReadTimeout.String() != "30s" {
		t.Fatalf("bulk-read DSN dropped the read timeout: %q", got)
	}
	if !strings.Contains(got, "compress=true") {
		t.Fatalf("bulk-read DSN does not request compression: %q", got)
	}

	// A plain DSN stays plain, so the parameter is the caller's choice, not
	// something the driver adds on its own.
	if strings.Contains(in, "compress") {
		t.Fatalf("test input already carries compress: %q", in)
	}
}

func TestBulkReadDSNRejectsGarbage(t *testing.T) {
	if _, err := bulkReadDSN("not a dsn ?? ("); err == nil {
		t.Fatal("expected an error for an unparsable DSN")
	}
}

func TestBulkReadDBSizesThePool(t *testing.T) {
	s := &DoltStore{connStr: unroutableConnStr}
	db, err := s.BulkReadDB(6)
	if err != nil {
		t.Fatalf("BulkReadDB: %v", err)
	}
	defer db.Close()
	if got := db.Stats().MaxOpenConnections; got != 6 {
		t.Fatalf("MaxOpenConnections = %d, want 6", got)
	}

	one, err := s.BulkReadDB(0)
	if err != nil {
		t.Fatalf("BulkReadDB(0): %v", err)
	}
	defer one.Close()
	if got := one.Stats().MaxOpenConnections; got != 1 {
		t.Fatalf("MaxOpenConnections for a zero request = %d, want 1", got)
	}
}
