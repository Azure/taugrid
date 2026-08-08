// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package expstore

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

var allowedObservationTypes = map[string]bool{
	"observation":     true,
	"decision":        true,
	"blocker":         true,
	"next-experiment": true,
	"exclusion":       true,
}

var allowedObservationScopes = map[string]string{
	"experiment": "experiments",
	"run_group":  "run_groups",
	"run":        "runs",
	"artifact":   "artifacts",
	"event":      "events",
	"metric":     "",
}

// observationScopeNames returns the accepted scope names in a stable order so
// the validation error message is derived from allowedObservationScopes rather
// than duplicated as a literal that silently drifts when a scope is added or
// removed.
func observationScopeNames() []string {
	names := make([]string, 0, len(allowedObservationScopes))
	for name := range allowedObservationScopes {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func (s *Store) RecordObservation(ctx context.Context, opts RecordObservationOptions) (RecordObservationResult, error) {
	var result RecordObservationResult
	err := s.withWriteLock(ctx, func() error {
		var err error
		result, err = s.recordObservationLocked(ctx, opts)
		return err
	})
	return result, err
}

func (s *Store) recordObservationLocked(ctx context.Context, opts RecordObservationOptions) (RecordObservationResult, error) {
	nowTime := time.Now().UTC()
	now := nowTime.Format(time.RFC3339)
	obs := opts.Observation
	if opts.Command == "" {
		opts.Command = "exp observe"
	}
	if opts.IdempotencyKey != "" {
		obs.IdempotencyKey = opts.IdempotencyKey
	}
	if obs.Source == "" {
		obs.Source = "human"
	}
	if obs.Type == "" {
		obs.Type = "observation"
	}
	if obs.CreatedAt == "" {
		obs.CreatedAt = now
	}
	if obs.ObservationID == "" {
		obs.ObservationID = generatedObservationID(obs, nowTime.Format(time.RFC3339Nano))
	}
	if err := validateObservation(obs); err != nil {
		return RecordObservationResult{}, err
	}
	requestHash := opts.RequestHash
	if requestHash == "" {
		var err error
		requestHash, err = observationRequestHash(obs)
		if err != nil {
			return RecordObservationResult{}, err
		}
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return RecordObservationResult{}, err
	}
	defer tx.Rollback()

	if obs.IdempotencyKey != "" {
		existingHash, err := idempotencyHash(ctx, tx, obs.IdempotencyKey)
		if err != nil {
			return RecordObservationResult{}, err
		}
		if existingHash != "" {
			if existingHash != requestHash {
				return RecordObservationResult{}, fmt.Errorf("%w: idempotency key %q was used for a different request", ErrConflict, obs.IdempotencyKey)
			}
			existingID, err := observationIDForIdempotencyKey(ctx, tx, obs.IdempotencyKey)
			if err != nil {
				return RecordObservationResult{}, err
			}
			return RecordObservationResult{
				ObservationID:  existingID,
				ScopeType:      obs.ScopeType,
				ScopeID:        obs.ScopeID,
				Type:           obs.Type,
				Reused:         true,
				IdempotencyKey: obs.IdempotencyKey,
			}, nil
		}
	}
	if err := validateObservationScope(ctx, tx, obs.ScopeType, obs.ScopeID); err != nil {
		return RecordObservationResult{}, err
	}
	created, err := ensureObservationRecord(ctx, tx, obs)
	if err != nil {
		return RecordObservationResult{}, err
	}
	idempotencyCreated := false
	if obs.IdempotencyKey != "" {
		_, err := tx.ExecContext(ctx, `
INSERT INTO idempotency_keys(key, command, target_type, target_id, request_hash, created_at)
VALUES (?, ?, 'observation', ?, ?, ?)
`, obs.IdempotencyKey, opts.Command, obs.ObservationID, requestHash, now)
		if err != nil {
			return RecordObservationResult{}, fmt.Errorf("record idempotency key: %w", err)
		}
		idempotencyCreated = true
	}
	cleanupMirrors, err := s.appendObservationMirrors(obs, created, idempotencyCreated, opts.Command, requestHash, now)
	if err != nil {
		return RecordObservationResult{}, err
	}
	if err := tx.Commit(); err != nil {
		if cleanupErr := cleanupMirrors(); cleanupErr != nil {
			return RecordObservationResult{}, fmt.Errorf("commit experiment observation: %w; cleanup JSONL mirrors: %v", err, cleanupErr)
		}
		return RecordObservationResult{}, err
	}
	return RecordObservationResult{
		ObservationID:  obs.ObservationID,
		ScopeType:      obs.ScopeType,
		ScopeID:        obs.ScopeID,
		Type:           obs.Type,
		Created:        created,
		Reused:         !created && !idempotencyCreated,
		IdempotencyKey: obs.IdempotencyKey,
	}, nil
}

func validateObservation(obs ObservationRecord) error {
	if err := validateID("observation", obs.ObservationID); err != nil {
		return err
	}
	if obs.IdempotencyKey != "" {
		if err := validateID("idempotency key", obs.IdempotencyKey); err != nil {
			return err
		}
	}
	if obs.Author == "" {
		return fmt.Errorf("observation author is required")
	}
	if obs.Source == "" {
		return fmt.Errorf("observation source is required")
	}
	if !allowedObservationTypes[obs.Type] {
		return fmt.Errorf("observation type must be one of: observation, decision, blocker, next-experiment, exclusion")
	}
	if _, ok := allowedObservationScopes[obs.ScopeType]; !ok {
		return fmt.Errorf("observation scope type must be one of: %s", strings.Join(observationScopeNames(), ", "))
	}
	if obs.ScopeID == "" {
		return fmt.Errorf("observation scope id is required")
	}
	if obs.ScopeType != "metric" {
		if err := validateID("observation scope id", obs.ScopeID); err != nil {
			return err
		}
	}
	if obs.Text == "" {
		return fmt.Errorf("observation text is required")
	}
	if obs.CreatedAt == "" {
		return fmt.Errorf("observation created_at is required")
	}
	return nil
}

func validateObservationScope(ctx context.Context, tx *sql.Tx, scopeType, scopeID string) error {
	table, ok := allowedObservationScopes[scopeType]
	if !ok {
		return fmt.Errorf("unsupported observation scope %q", scopeType)
	}
	if scopeType == "metric" {
		return nil
	}
	idColumn, ok := map[string]string{
		"experiment": "experiment_id",
		"run_group":  "run_group_id",
		"run":        "run_id",
		"artifact":   "artifact_id",
		"event":      "event_id",
	}[scopeType]
	if !ok {
		// Fail closed. An unmapped scope previously produced "WHERE  = ?",
		// which surfaced to users as an opaque SQL syntax error.
		return fmt.Errorf("unsupported observation scope %q", scopeType)
	}
	var exists int
	if err := tx.QueryRowContext(ctx, fmt.Sprintf("SELECT count(*) FROM %s WHERE %s = ?", table, idColumn), scopeID).Scan(&exists); err != nil {
		return err
	}
	if exists == 0 {
		return fmt.Errorf("%w: observation scope %s:%s", ErrNotFound, scopeType, scopeID)
	}
	return nil
}

func ensureObservationRecord(ctx context.Context, tx *sql.Tx, obs ObservationRecord) (bool, error) {
	var existing ObservationRecord
	err := tx.QueryRowContext(ctx, `
SELECT observation_id, coalesce(idempotency_key, ''), author, source, type, scope_type, scope_id,
       text, coalesce(evidence, ''), created_at
FROM observations WHERE observation_id = ?`, obs.ObservationID).Scan(
		&existing.ObservationID,
		&existing.IdempotencyKey,
		&existing.Author,
		&existing.Source,
		&existing.Type,
		&existing.ScopeType,
		&existing.ScopeID,
		&existing.Text,
		&existing.Evidence,
		&existing.CreatedAt,
	)
	if err == nil {
		if !observationRecordEqual(existing, obs) {
			return false, fmt.Errorf("%w: observation %q already exists with different metadata", ErrConflict, obs.ObservationID)
		}
		return false, nil
	}
	if err != sql.ErrNoRows {
		return false, err
	}
	_, err = tx.ExecContext(ctx, `
INSERT INTO observations(observation_id, idempotency_key, author, source, type, scope_type, scope_id, text, evidence, created_at)
VALUES (?, nullif(?, ''), ?, ?, ?, ?, ?, ?, nullif(?, ''), ?)
`, obs.ObservationID, obs.IdempotencyKey, obs.Author, obs.Source, obs.Type, obs.ScopeType, obs.ScopeID, obs.Text, obs.Evidence, obs.CreatedAt)
	if err != nil {
		return false, err
	}
	return true, nil
}

func observationIDForIdempotencyKey(ctx context.Context, tx *sql.Tx, key string) (string, error) {
	var id string
	err := tx.QueryRowContext(ctx, "SELECT target_id FROM idempotency_keys WHERE key = ? AND target_type = 'observation'", key).Scan(&id)
	if err == sql.ErrNoRows {
		return "", fmt.Errorf("%w: observation idempotency key %q", ErrNotFound, key)
	}
	return id, err
}

func (s *Store) appendObservationMirrors(obs ObservationRecord, created, idempotencyCreated bool, command, requestHash, now string) (func() error, error) {
	var cleanups []func() error
	add := func(name string, record any) error {
		cleanup, err := s.appendJSONLWithRollback(name, record)
		if err != nil {
			if cleanupErr := cleanupJSONL(cleanups); cleanupErr != nil {
				return fmt.Errorf("%w; cleanup JSONL mirrors: %v", err, cleanupErr)
			}
			return err
		}
		cleanups = append(cleanups, cleanup)
		return nil
	}
	if created {
		if err := add("observations.jsonl", obs); err != nil {
			return nil, err
		}
	}
	if idempotencyCreated {
		if err := add("idempotency_keys.jsonl", map[string]string{
			"key":          obs.IdempotencyKey,
			"command":      command,
			"target_type":  "observation",
			"target_id":    obs.ObservationID,
			"request_hash": requestHash,
			"created_at":   now,
		}); err != nil {
			return nil, err
		}
	}
	return func() error {
		return cleanupJSONL(cleanups)
	}, nil
}

func observationRequestHash(obs ObservationRecord) (string, error) {
	payload := map[string]string{
		"author":     obs.Author,
		"source":     obs.Source,
		"type":       obs.Type,
		"scope_type": obs.ScopeType,
		"scope_id":   obs.ScopeID,
		"text":       obs.Text,
		"evidence":   obs.Evidence,
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

func generatedObservationID(obs ObservationRecord, now string) string {
	raw := []byte(obs.Author + "\x00" + obs.Source + "\x00" + obs.Type + "\x00" + obs.ScopeType + "\x00" + obs.ScopeID + "\x00" + obs.Text + "\x00" + obs.Evidence + "\x00" + now)
	sum := sha256.Sum256(raw)
	return "obs-" + hex.EncodeToString(sum[:])[:12]
}

func observationRecordEqual(a, b ObservationRecord) bool {
	return a.ObservationID == b.ObservationID &&
		a.IdempotencyKey == b.IdempotencyKey &&
		a.Author == b.Author &&
		a.Source == b.Source &&
		a.Type == b.Type &&
		a.ScopeType == b.ScopeType &&
		a.ScopeID == b.ScopeID &&
		a.Text == b.Text &&
		a.Evidence == b.Evidence &&
		a.CreatedAt == b.CreatedAt
}
