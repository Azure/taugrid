// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package collector implements durable, restart-safe experiment metric collection.
package collector

import (
	"context"
	"time"

	"github.com/Azure/taugrid/core/exptelemetry"
)

const (
	ChunkSchemaV1         = "tau.experiment.metric_chunk.v1"
	ChunkManifestSchemaV1 = "tau.experiment.metric_chunk_manifest.v1"
	CheckpointSchemaV1    = "tau.experiment.metrics_checkpoint.v1"
	ReceiptSchemaV1       = "tau.experiment.metric_delivery.v1"
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

// DeliveryAck records a sink's durable acceptance of one immutable chunk.
type DeliveryAck struct {
	SchemaVersion  string            `json:"schema_version"`
	ChunkDigest    string            `json:"chunk_digest"`
	ConfigIdentity string            `json:"config_identity"`
	Sink           string            `json:"sink"`
	Samples        int               `json:"samples"`
	Requests       int               `json:"requests,omitempty"`
	Retries        int               `json:"retries,omitempty"`
	Metadata       map[string]string `json:"metadata,omitempty"`
	DeliveredAt    time.Time         `json:"delivered_at"`
}

// Sink delivers immutable chunks. ConfigIdentity must change whenever delivery
// behavior or destination changes.
type Sink interface {
	Name() string
	ConfigIdentity() string
	Deliver(context.Context, MetricEventChunk) (DeliveryAck, error)
}
