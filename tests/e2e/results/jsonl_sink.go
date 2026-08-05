package results

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sync"
)

// JSONLSink writes one JSON object per line to a file.
// Safe for concurrent use via an internal mutex.
type JSONLSink struct {
	mu     sync.Mutex
	file   *os.File
	enc    *json.Encoder
	closed bool
}

// NewJSONLSink creates a JSONL sink that writes to path.
// The file is created if it does not exist, or appended to if it does.
// Append mode is required because `go test ./...` runs each package in a
// separate process, so multiple processes write to the same file sequentially.
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
