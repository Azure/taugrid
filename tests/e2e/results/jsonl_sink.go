// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package results

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sync"
)

type JSONLSink struct {
	mu     sync.Mutex
	file   *os.File
	enc    *json.Encoder
	closed bool
}

func NewJSONLSink(path string) (*JSONLSink, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fmt.Errorf("results: creating JSONL file %s: %w", path, err)
	}
	return &JSONLSink{
		file: f,
		enc:  json.NewEncoder(f),
	}, nil
}

func (s *JSONLSink) Record(_ context.Context, o Outcome) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return fmt.Errorf("results: sink already closed")
	}
	return s.enc.Encode(o)
}

func (s *JSONLSink) Flush() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	return s.file.Close()
}
