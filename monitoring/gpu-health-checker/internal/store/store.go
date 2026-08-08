// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package store manages the SQLite database used for communication between
// the collector daemon and reader processes.
package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

const schema = `
CREATE TABLE IF NOT EXISTS samples (
    ts       INTEGER NOT NULL,
    gpu      INTEGER NOT NULL,
    field    TEXT    NOT NULL,
    value    REAL    NOT NULL,
    PRIMARY KEY (ts, gpu, field)
) WITHOUT ROWID;

CREATE TABLE IF NOT EXISTS health_checks (
    ts       INTEGER NOT NULL,
    gpu      INTEGER NOT NULL,
    system   TEXT    NOT NULL,
    status   TEXT    NOT NULL,
    message  TEXT,
    PRIMARY KEY (ts, gpu, system)
) WITHOUT ROWID;

CREATE TABLE IF NOT EXISTS gpu_info (
    gpu      INTEGER PRIMARY KEY,
    uuid     TEXT,
    pci_bus  TEXT,
    name     TEXT,
    vbios    TEXT,
    driver   TEXT,
    updated  INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_samples_field_gpu_ts ON samples(field, gpu, ts);
CREATE INDEX IF NOT EXISTS idx_health_checks_gpu_system_ts ON health_checks(gpu, system, ts);
`

// DB wraps a SQLite database connection with typed operations for the
// gpu-health-checker schema.
type DB struct {
	db *sql.DB
}

// Open opens (or creates) the SQLite database at the given path with WAL mode.
func Open(path string) (*DB, error) {
	db, err := sql.Open("sqlite3", path+"?_journal_mode=WAL&_busy_timeout=5000&_synchronous=NORMAL")
	if err != nil {
		return nil, fmt.Errorf("open sqlite %s: %w", path, err)
	}
	if _, err := db.Exec(schema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("create schema: %w", err)
	}
	return &DB{db: db}, nil
}

// OpenReadOnly opens the SQLite database in read-only mode for reader processes.
func OpenReadOnly(path string) (*DB, error) {
	db, err := sql.Open("sqlite3", path+"?mode=ro&_busy_timeout=5000")
	if err != nil {
		return nil, fmt.Errorf("open sqlite (ro) %s: %w", path, err)
	}
	return &DB{db: db}, nil
}

// Close closes the database connection.
func (d *DB) Close() error {
	return d.db.Close()
}

// Sample represents a single DCGM field sample.
type Sample struct {
	Timestamp int64
	GPU       int
	Field     string
	Value     float64
}

// HealthCheck represents a DCGM health diagnostic result.
type HealthCheck struct {
	Timestamp int64
	GPU       int
	System    string
	Status    string
	Message   string
}

// GPUInfoRow represents a row in the gpu_info table.
type GPUInfoRow struct {
	GPU     int
	UUID    string
	PCIBus  string
	Name    string
	VBIOS   string
	Driver  string
	Updated int64
}

// InsertSamples inserts a batch of samples in a single transaction.
func (d *DB) InsertSamples(samples []Sample) error {
	tx, err := d.db.Begin()
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	stmt, err := tx.Prepare("INSERT OR REPLACE INTO samples (ts, gpu, field, value) VALUES (?, ?, ?, ?)")
	if err != nil {
		return fmt.Errorf("prepare insert: %w", err)
	}
	defer func() { _ = stmt.Close() }()

	for _, s := range samples {
		if _, err := stmt.Exec(s.Timestamp, s.GPU, s.Field, s.Value); err != nil {
			return fmt.Errorf("insert sample (gpu=%d field=%s): %w", s.GPU, s.Field, err)
		}
	}
	return tx.Commit()
}

// InsertHealthChecks inserts a batch of health check results.
func (d *DB) InsertHealthChecks(checks []HealthCheck) error {
	tx, err := d.db.Begin()
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	stmt, err := tx.Prepare("INSERT OR REPLACE INTO health_checks (ts, gpu, system, status, message) VALUES (?, ?, ?, ?, ?)")
	if err != nil {
		return fmt.Errorf("prepare insert: %w", err)
	}
	defer func() { _ = stmt.Close() }()

	for _, c := range checks {
		if _, err := stmt.Exec(c.Timestamp, c.GPU, c.System, c.Status, c.Message); err != nil {
			return fmt.Errorf("insert health check (gpu=%d system=%s): %w", c.GPU, c.System, err)
		}
	}
	return tx.Commit()
}

// UpsertGPUInfo inserts or updates GPU static info.
func (d *DB) UpsertGPUInfo(info GPUInfoRow) error {
	_, err := d.db.Exec(`INSERT OR REPLACE INTO gpu_info (gpu, uuid, pci_bus, name, vbios, driver, updated)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		info.GPU, info.UUID, info.PCIBus, info.Name, info.VBIOS, info.Driver, info.Updated)
	if err != nil {
		return fmt.Errorf("upsert gpu_info (gpu=%d): %w", info.GPU, err)
	}
	return nil
}

// Prune deletes samples and health_checks older than the given retention duration.
func (d *DB) Prune(retention time.Duration) error {
	cutoff := time.Now().Unix() - int64(retention.Seconds())
	if _, err := d.db.Exec("DELETE FROM samples WHERE ts < ?", cutoff); err != nil {
		return fmt.Errorf("prune samples: %w", err)
	}
	if _, err := d.db.Exec("DELETE FROM health_checks WHERE ts < ?", cutoff); err != nil {
		return fmt.Errorf("prune health_checks: %w", err)
	}
	return nil
}

// QuerySamples returns samples matching the given field name, optionally filtered
// by GPU index and time window. If gpu < 0, all GPUs are returned. If since is
// zero, no time filter is applied.
func (d *DB) QuerySamples(ctx context.Context, field string, gpu int, since time.Time) ([]Sample, error) {
	var rows *sql.Rows
	var err error

	switch {
	case gpu >= 0 && !since.IsZero():
		rows, err = d.db.QueryContext(ctx,
			"SELECT ts, gpu, field, value FROM samples WHERE field = ? AND gpu = ? AND ts >= ? ORDER BY ts ASC",
			field, gpu, since.Unix())
	case gpu >= 0:
		rows, err = d.db.QueryContext(ctx,
			"SELECT ts, gpu, field, value FROM samples WHERE field = ? AND gpu = ? ORDER BY ts ASC",
			field, gpu)
	case !since.IsZero():
		rows, err = d.db.QueryContext(ctx,
			"SELECT ts, gpu, field, value FROM samples WHERE field = ? AND ts >= ? ORDER BY ts ASC",
			field, since.Unix())
	default:
		rows, err = d.db.QueryContext(ctx,
			"SELECT ts, gpu, field, value FROM samples WHERE field = ? ORDER BY ts ASC",
			field)
	}
	if err != nil {
		return nil, fmt.Errorf("query samples (field=%s): %w", field, err)
	}
	defer func() { _ = rows.Close() }()

	var result []Sample
	for rows.Next() {
		var s Sample
		if err := rows.Scan(&s.Timestamp, &s.GPU, &s.Field, &s.Value); err != nil {
			return nil, fmt.Errorf("scan sample: %w", err)
		}
		result = append(result, s)
	}
	return result, rows.Err()
}

// QueryLatestSamples returns the most recent sample for each GPU for the given field.
func (d *DB) QueryLatestSamples(ctx context.Context, field string) ([]Sample, error) {
	rows, err := d.db.QueryContext(ctx, `
		SELECT s.ts, s.gpu, s.field, s.value
		FROM samples s
		INNER JOIN (
			SELECT gpu, MAX(ts) AS max_ts FROM samples WHERE field = ? GROUP BY gpu
		) latest ON s.gpu = latest.gpu AND s.ts = latest.max_ts AND s.field = ?`,
		field, field)
	if err != nil {
		return nil, fmt.Errorf("query latest samples (field=%s): %w", field, err)
	}
	defer func() { _ = rows.Close() }()

	var result []Sample
	for rows.Next() {
		var s Sample
		if err := rows.Scan(&s.Timestamp, &s.GPU, &s.Field, &s.Value); err != nil {
			return nil, fmt.Errorf("scan sample: %w", err)
		}
		result = append(result, s)
	}
	return result, rows.Err()
}

// QueryLatestHealthChecks returns the most recent health check for each GPU and system.
func (d *DB) QueryLatestHealthChecks(ctx context.Context) ([]HealthCheck, error) {
	rows, err := d.db.QueryContext(ctx, `
		SELECT h.ts, h.gpu, h.system, h.status, h.message
		FROM health_checks h
		INNER JOIN (
			SELECT gpu, system, MAX(ts) AS max_ts FROM health_checks GROUP BY gpu, system
		) latest ON h.gpu = latest.gpu AND h.system = latest.system AND h.ts = latest.max_ts`)
	if err != nil {
		return nil, fmt.Errorf("query latest health checks: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var result []HealthCheck
	for rows.Next() {
		var c HealthCheck
		if err := rows.Scan(&c.Timestamp, &c.GPU, &c.System, &c.Status, &c.Message); err != nil {
			return nil, fmt.Errorf("scan health check: %w", err)
		}
		result = append(result, c)
	}
	return result, rows.Err()
}

// QueryAllGPUInfo returns all rows from the gpu_info table.
func (d *DB) QueryAllGPUInfo(ctx context.Context) ([]GPUInfoRow, error) {
	rows, err := d.db.QueryContext(ctx, "SELECT gpu, uuid, pci_bus, name, vbios, driver, updated FROM gpu_info ORDER BY gpu")
	if err != nil {
		return nil, fmt.Errorf("query gpu_info: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var result []GPUInfoRow
	for rows.Next() {
		var g GPUInfoRow
		if err := rows.Scan(&g.GPU, &g.UUID, &g.PCIBus, &g.Name, &g.VBIOS, &g.Driver, &g.Updated); err != nil {
			return nil, fmt.Errorf("scan gpu_info: %w", err)
		}
		result = append(result, g)
	}
	return result, rows.Err()
}

// LatestTimestamp returns the timestamp of the newest sample in the database.
// Returns 0 if the database is empty.
func (d *DB) LatestTimestamp() (int64, error) {
	var ts sql.NullInt64
	if err := d.db.QueryRow("SELECT MAX(ts) FROM samples").Scan(&ts); err != nil {
		return 0, fmt.Errorf("query max ts: %w", err)
	}
	if !ts.Valid {
		return 0, nil
	}
	return ts.Int64, nil
}
