// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package rdmavalidation

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/Azure/taugrid/core/exptelemetry"
)

const cursorVersion = 1

type Cursor struct {
	SortAt       time.Time
	ValidationID string
}

type cursorPayload struct {
	Version      int    `json:"v"`
	SortAt       string `json:"sort_at"`
	ValidationID string `json:"validation_id"`
}

func EncodeCursor(cursor Cursor) (string, error) {
	if cursor.SortAt.IsZero() {
		return "", fmt.Errorf("%w: sort timestamp is required", ErrInvalidCursor)
	}
	if err := exptelemetry.ValidateID("validation ID", cursor.ValidationID); err != nil {
		return "", fmt.Errorf("%w: %v", ErrInvalidCursor, err)
	}
	data, err := json.Marshal(cursorPayload{
		Version:      cursorVersion,
		SortAt:       cursor.SortAt.UTC().Format(time.RFC3339Nano),
		ValidationID: cursor.ValidationID,
	})
	if err != nil {
		return "", fmt.Errorf("encode RDMA validation cursor: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(data), nil
}

func DecodeCursor(value string) (Cursor, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return Cursor{}, nil
	}
	data, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return Cursor{}, fmt.Errorf("%w: decode", ErrInvalidCursor)
	}
	var payload cursorPayload
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&payload); err != nil {
		return Cursor{}, fmt.Errorf("%w: payload", ErrInvalidCursor)
	}
	if payload.Version != cursorVersion {
		return Cursor{}, fmt.Errorf("%w: unsupported version", ErrInvalidCursor)
	}
	sortAt, err := time.Parse(time.RFC3339Nano, payload.SortAt)
	if err != nil {
		return Cursor{}, fmt.Errorf("%w: timestamp", ErrInvalidCursor)
	}
	if err := exptelemetry.ValidateID("validation ID", payload.ValidationID); err != nil {
		return Cursor{}, fmt.Errorf("%w: validation ID", ErrInvalidCursor)
	}
	return Cursor{SortAt: sortAt.UTC(), ValidationID: payload.ValidationID}, nil
}
