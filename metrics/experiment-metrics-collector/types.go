// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package collector implements durable, restart-safe experiment metric collection.
package collector

import (
	"context"

	"github.com/Azure/taugrid/core/exptelemetry"
)

const (
	ChunkSchemaV1         = "tau.experiment.metric_chunk.v1"
	ChunkManifestSchemaV1 = "tau.experiment.metric_chunk_manifest.v1"
	CheckpointSchemaV1    = "tau.experiment.metrics_checkpoint.v1"
)

// MetricEventChunk is the immutable unit delivered to sinks.
type MetricEventChunk struct {
	SchemaVersion string
	Digest        string
	Path          string
	SourcePath    string
	SourceFileID  string
	StartOffset   int64
	EndOffset     int64
	Sequence      uint64
	Events        []exptelemetry.MetricEvent
	NDJSON        []byte
}

// DeliveryAck reports one sink delivery attempt.
type DeliveryAck struct {
	Samples  int
	Requests int
	Retries  int
	Metadata map[string]string
}

// Sink delivers immutable chunks. ConfigIdentity must change whenever delivery
// behavior or destination changes.
type Sink interface {
	Name() string
	ConfigIdentity() string
	Deliver(context.Context, MetricEventChunk) (DeliveryAck, error)
}
