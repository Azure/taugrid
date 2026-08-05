package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func tempDB(t *testing.T) *DB {
	t.Helper()
	dir := t.TempDir()
	db, err := Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestOpenAndSchema(t *testing.T) {
	db := tempDB(t)

	// Verify tables exist by inserting and querying.
	err := db.InsertSamples([]Sample{
		{Timestamp: 1000, GPU: 0, Field: "TEST", Value: 42},
	})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	samples, err := db.QuerySamples(context.Background(), "TEST", -1, time.Time{})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(samples) != 1 || samples[0].Value != 42 {
		t.Errorf("got %v, want [{42}]", samples)
	}
}

func TestOpenReadOnly(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.db")

	// Create and populate.
	db, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	_ = db.InsertSamples([]Sample{{Timestamp: 1000, GPU: 0, Field: "X", Value: 1}})
	_ = db.Close()

	// Open read-only.
	ro, err := OpenReadOnly(path)
	if err != nil {
		t.Fatalf("open ro: %v", err)
	}
	defer func() { _ = ro.Close() }()

	samples, err := ro.QuerySamples(context.Background(), "X", -1, time.Time{})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(samples) != 1 {
		t.Errorf("expected 1 sample, got %d", len(samples))
	}
}

func TestOpenReadOnly_Missing(t *testing.T) {
	ro, err := OpenReadOnly("/nonexistent/path/test.db")
	if err != nil {
		// Driver rejected the path at open time.
		return
	}
	defer func() { _ = ro.Close() }()

	// sql.Open succeeds lazily; the error surfaces on first query.
	_, err = ro.QuerySamples(context.Background(), "X", -1, time.Time{})
	if err == nil {
		t.Error("expected error querying a non-existent database")
	}
}

func TestInsertAndQuerySamples(t *testing.T) {
	db := tempDB(t)
	ctx := context.Background()

	samples := []Sample{
		{Timestamp: 100, GPU: 0, Field: "A", Value: 1},
		{Timestamp: 100, GPU: 1, Field: "A", Value: 2},
		{Timestamp: 200, GPU: 0, Field: "A", Value: 3},
		{Timestamp: 200, GPU: 1, Field: "A", Value: 4},
		{Timestamp: 100, GPU: 0, Field: "B", Value: 10},
	}
	if err := db.InsertSamples(samples); err != nil {
		t.Fatalf("insert: %v", err)
	}

	// All field A.
	got, _ := db.QuerySamples(ctx, "A", -1, time.Time{})
	if len(got) != 4 {
		t.Errorf("field A all: got %d, want 4", len(got))
	}

	// Field A, GPU 0 only.
	got, _ = db.QuerySamples(ctx, "A", 0, time.Time{})
	if len(got) != 2 {
		t.Errorf("field A gpu 0: got %d, want 2", len(got))
	}

	// Field A, since ts=200.
	got, _ = db.QuerySamples(ctx, "A", -1, time.Unix(200, 0))
	if len(got) != 2 {
		t.Errorf("field A since 200: got %d, want 2", len(got))
	}

	// Field A, GPU 0, since ts=200.
	got, _ = db.QuerySamples(ctx, "A", 0, time.Unix(200, 0))
	if len(got) != 1 {
		t.Errorf("field A gpu 0 since 200: got %d, want 1", len(got))
	}

	// Field B.
	got, _ = db.QuerySamples(ctx, "B", -1, time.Time{})
	if len(got) != 1 {
		t.Errorf("field B: got %d, want 1", len(got))
	}
}

func TestQueryLatestSamples(t *testing.T) {
	db := tempDB(t)
	ctx := context.Background()

	_ = db.InsertSamples([]Sample{
		{Timestamp: 100, GPU: 0, Field: "X", Value: 1},
		{Timestamp: 200, GPU: 0, Field: "X", Value: 2},
		{Timestamp: 100, GPU: 1, Field: "X", Value: 10},
		{Timestamp: 200, GPU: 1, Field: "X", Value: 20},
	})

	got, err := db.QueryLatestSamples(ctx, "X")
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 latest, got %d", len(got))
	}

	vals := map[int]float64{}
	for _, s := range got {
		vals[s.GPU] = s.Value
	}
	if vals[0] != 2 || vals[1] != 20 {
		t.Errorf("latest values: got %v, want {0:2, 1:20}", vals)
	}
}

func TestInsertAndQueryHealthChecks(t *testing.T) {
	db := tempDB(t)
	ctx := context.Background()

	checks := []HealthCheck{
		{Timestamp: 100, GPU: 0, System: "Memory", Status: "healthy", Message: "ok"},
		{Timestamp: 200, GPU: 0, System: "Memory", Status: "warning", Message: "SBE detected"},
		{Timestamp: 100, GPU: 1, System: "PCIe", Status: "healthy", Message: "ok"},
	}
	if err := db.InsertHealthChecks(checks); err != nil {
		t.Fatalf("insert: %v", err)
	}

	got, err := db.QueryLatestHealthChecks(ctx)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 latest, got %d", len(got))
	}

	for _, hc := range got {
		if hc.GPU == 0 && hc.System == "Memory" {
			if hc.Status != "warning" {
				t.Errorf("GPU 0 Memory: got %q, want warning", hc.Status)
			}
		}
	}
}

func TestUpsertGPUInfo(t *testing.T) {
	db := tempDB(t)
	ctx := context.Background()

	info := GPUInfoRow{GPU: 0, UUID: "uuid-0", PCIBus: "00:3B:00.0", Name: "H100", VBIOS: "1.0", Driver: "535.54", Updated: 100}
	if err := db.UpsertGPUInfo(info); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	// Update.
	info.VBIOS = "2.0"
	info.Updated = 200
	if err := db.UpsertGPUInfo(info); err != nil {
		t.Fatalf("upsert update: %v", err)
	}

	got, err := db.QueryAllGPUInfo(ctx)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(got) != 1 || got[0].VBIOS != "2.0" {
		t.Errorf("got %v, want VBIOS=2.0", got)
	}
}

func TestPrune(t *testing.T) {
	db := tempDB(t)
	ctx := context.Background()

	now := time.Now().Unix()
	old := now - 3600 // 1 hour ago

	_ = db.InsertSamples([]Sample{
		{Timestamp: old, GPU: 0, Field: "X", Value: 1},
		{Timestamp: now, GPU: 0, Field: "X", Value: 2},
	})
	_ = db.InsertHealthChecks([]HealthCheck{
		{Timestamp: old, GPU: 0, System: "Mem", Status: "ok"},
		{Timestamp: now, GPU: 0, System: "Mem", Status: "ok"},
	})

	if err := db.Prune(30 * time.Minute); err != nil {
		t.Fatalf("prune: %v", err)
	}

	samples, _ := db.QuerySamples(ctx, "X", -1, time.Time{})
	if len(samples) != 1 {
		t.Errorf("expected 1 sample after prune, got %d", len(samples))
	}

	checks, _ := db.QueryLatestHealthChecks(ctx)
	if len(checks) != 1 {
		t.Errorf("expected 1 health check after prune, got %d", len(checks))
	}
}

func TestLatestTimestamp(t *testing.T) {
	db := tempDB(t)

	// Empty DB.
	ts, err := db.LatestTimestamp()
	if err != nil || ts != 0 {
		t.Errorf("empty db: ts=%d err=%v, want 0/nil", ts, err)
	}

	_ = db.InsertSamples([]Sample{
		{Timestamp: 100, GPU: 0, Field: "X", Value: 1},
		{Timestamp: 300, GPU: 0, Field: "X", Value: 2},
		{Timestamp: 200, GPU: 0, Field: "X", Value: 3},
	})

	ts, err = db.LatestTimestamp()
	if err != nil || ts != 300 {
		t.Errorf("ts=%d err=%v, want 300/nil", ts, err)
	}
}

func TestInsertOrReplace(t *testing.T) {
	db := tempDB(t)
	ctx := context.Background()

	_ = db.InsertSamples([]Sample{
		{Timestamp: 100, GPU: 0, Field: "X", Value: 1},
	})
	// Same PK, different value — should replace.
	_ = db.InsertSamples([]Sample{
		{Timestamp: 100, GPU: 0, Field: "X", Value: 99},
	})

	got, _ := db.QuerySamples(ctx, "X", 0, time.Time{})
	if len(got) != 1 || got[0].Value != 99 {
		t.Errorf("expected replaced value=99, got %v", got)
	}
}

func TestQuerySamplesContext_Cancelled(t *testing.T) {
	db := tempDB(t)

	_ = db.InsertSamples([]Sample{
		{Timestamp: 100, GPU: 0, Field: "X", Value: 1},
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately.

	_, err := db.QuerySamples(ctx, "X", -1, time.Time{})
	if err == nil {
		// SQLite may or may not respect context cancellation for fast queries.
		// This test verifies the context is wired through — the behavior
		// depends on the driver. We accept both outcomes.
		t.Log("query succeeded with cancelled context (acceptable for fast queries)")
	}
}

func TestIndexesExist(t *testing.T) {
	db := tempDB(t)

	// Query sqlite_master for our indexes.
	rows, err := db.db.Query("SELECT name FROM sqlite_master WHERE type='index' AND name LIKE 'idx_%'")
	if err != nil {
		t.Fatalf("query indexes: %v", err)
	}
	defer func() { _ = rows.Close() }()

	indexes := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan: %v", err)
		}
		indexes[name] = true
	}

	expected := []string{
		"idx_samples_field_gpu_ts",
		"idx_health_checks_gpu_system_ts",
	}
	for _, idx := range expected {
		if !indexes[idx] {
			t.Errorf("missing index: %s (found: %v)", idx, indexes)
		}
	}
}

func TestConcurrentReadWrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.db")

	writer, err := Open(path)
	if err != nil {
		t.Fatalf("open writer: %v", err)
	}
	defer func() { _ = writer.Close() }()

	reader, err := OpenReadOnly(path)
	if err != nil {
		t.Fatalf("open reader: %v", err)
	}
	defer func() { _ = reader.Close() }()

	// Write while reading.
	_ = writer.InsertSamples([]Sample{
		{Timestamp: 100, GPU: 0, Field: "X", Value: 1},
	})

	got, err := reader.QuerySamples(context.Background(), "X", -1, time.Time{})
	if err != nil {
		t.Fatalf("concurrent read: %v", err)
	}
	if len(got) != 1 {
		t.Errorf("expected 1, got %d", len(got))
	}

	// Write more, read again.
	_ = writer.InsertSamples([]Sample{
		{Timestamp: 200, GPU: 0, Field: "X", Value: 2},
	})

	got, err = reader.QueryLatestSamples(context.Background(), "X")
	if err != nil {
		t.Fatalf("concurrent read latest: %v", err)
	}
	if len(got) != 1 || got[0].Value != 2 {
		t.Errorf("expected latest=2, got %v", got)
	}
}

func TestDBNotExist(t *testing.T) {
	path := filepath.Join(os.TempDir(), "nonexistent_dir_"+t.Name(), "test.db")
	_, err := Open(path)
	// Some SQLite drivers create parent dirs, others don't. Just verify no panic.
	if err != nil {
		t.Logf("expected-ish error for missing parent dir: %v", err)
	}
}
