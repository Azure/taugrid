// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package results

import "context"

// ResultEmitter records test outcomes to a backing store.
type ResultEmitter interface {
	// Record writes a single test outcome. Implementations must be safe for
	// concurrent use — multiple tests in the same package run in parallel.
	Record(ctx context.Context, o Outcome) error
	// Flush persists any buffered data. Called once per process in TestMain
	// before os.Exit.
	Flush() error
}

// Multi fans out Record/Flush calls to multiple emitters.
// This lets us emit JSONL today and add KustoSink (WI-03) with one line change.
type Multi struct {
	Emitters []ResultEmitter
}

func (m *Multi) Record(ctx context.Context, o Outcome) error {
	var firstErr error
	for _, e := range m.Emitters {
		if err := e.Record(ctx, o); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (m *Multi) Flush() error {
	var firstErr error
	for _, e := range m.Emitters {
		if err := e.Flush(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}
