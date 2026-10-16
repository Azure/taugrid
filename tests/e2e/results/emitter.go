// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package results

import "context"

type ResultEmitter interface {
	Record(ctx context.Context, o Outcome) error
	Flush() error
}

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
