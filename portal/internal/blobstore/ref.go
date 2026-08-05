package blobstore

import (
	"encoding/json"
	"fmt"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Azure/taugrid/core/fileutil"
)

const (
	DurableRefSchemaVersion       = "tau.blobref.v2"
	legacyDurableRefSchemaVersion = "tau.blobref.v1"
)

type DurableRef struct {
	SchemaVersion string `json:"schema_version"`
	URI           string `json:"uri"`
	Digest        string `json:"digest"`
	SizeBytes     int64  `json:"size_bytes"`
	ContentType   string `json:"content_type"`
	UploadedAt    string `json:"uploaded_at"`
	Account       string `json:"account,omitempty"`
	Container     string `json:"container,omitempty"`
	BlobPath      string `json:"blob_path"`
}

type Partition struct {
	Account      string
	Container    string
	BaseURI      string
	Project      string
	ExperimentID string
	RunGroupID   string
	RunID        string
	ArtifactType string
	ArtifactName string
	Digest       string
	Rank         *int64
}

func NewDurableRef(partition Partition, sizeBytes int64, contentType string, uploadedAt time.Time) DurableRef {
	blobPath := BuildBlobPath(partition)
	baseURI := strings.TrimRight(partition.BaseURI, "/")
	uri := strings.TrimLeft(blobPath, "/")
	if baseURI != "" {
		uri = baseURI + "/" + uri
	}
	return DurableRef{
		SchemaVersion: DurableRefSchemaVersion,
		URI:           uri,
		Digest:        DigestWithAlgorithm(partition.Digest),
		SizeBytes:     sizeBytes,
		ContentType:   contentType,
		UploadedAt:    uploadedAt.UTC().Format(time.RFC3339),
		Account:       partition.Account,
		Container:     partition.Container,
		BlobPath:      blobPath,
	}
}

func BuildBlobPath(partition Partition) string {
	digest := strings.TrimPrefix(partition.Digest, "sha256:")
	prefix := digest
	if len(prefix) > 2 {
		prefix = prefix[:2]
	}
	if prefix == "" {
		prefix = "_"
	}
	name := fileutil.SafePathComponent(partition.ArtifactName)
	ext := filepath.Ext(name)
	base := strings.TrimSuffix(name, ext)
	if base == "" {
		base = "artifact"
	}
	digestComponent := fileutil.ShortDigest(digest, 16)
	if digestComponent == "" {
		digestComponent = "unknown"
	}
	fileName := digestComponent + "-" + base + ext
	parts := []string{
		"v2",
		"project=" + fileutil.SafePathComponent(partition.Project),
		"experiment=" + safeOrPlaceholder(partition.ExperimentID),
		"group=" + fileutil.SafePathComponent(partition.RunGroupID),
		"run=" + fileutil.SafePathComponent(partition.RunID),
		fileutil.SafePathComponent(partition.ArtifactType),
	}
	if partition.Rank != nil {
		parts = append(parts, "rank="+fileutil.SafePathComponent(strconv.FormatInt(*partition.Rank, 10)))
	}
	parts = append(parts, prefix, fileName)
	return strings.Join(parts, "/")
}

func DigestWithAlgorithm(digest string) string {
	digest = strings.TrimSpace(digest)
	if digest == "" || strings.Contains(digest, ":") {
		return digest
	}
	return "sha256:" + digest
}

func ParseDurableRef(raw string) (DurableRef, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return DurableRef{}, fmt.Errorf("durable ref is empty")
	}
	var ref DurableRef
	if err := json.Unmarshal([]byte(raw), &ref); err != nil {
		return DurableRef{}, err
	}
	if err := ref.Validate(); err != nil {
		if normalized, normalizeErr := normalizeRefFromURI(ref); normalizeErr == nil {
			return normalized, nil
		}
		return DurableRef{}, err
	}
	return ref, nil
}

func (r DurableRef) String() (string, error) {
	if err := r.Validate(); err != nil {
		return "", err
	}
	raw, err := json.Marshal(r)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

func (r DurableRef) Validate() error {
	if r.SchemaVersion != DurableRefSchemaVersion && r.SchemaVersion != legacyDurableRefSchemaVersion {
		return fmt.Errorf("unsupported durable ref schema_version %q", r.SchemaVersion)
	}
	if strings.TrimSpace(r.URI) == "" {
		return fmt.Errorf("durable ref uri is required")
	}
	if strings.TrimSpace(r.Digest) == "" {
		return fmt.Errorf("durable ref digest is required")
	}
	if r.SizeBytes < 0 {
		return fmt.Errorf("durable ref size_bytes must be non-negative")
	}
	if strings.TrimSpace(r.BlobPath) == "" {
		return fmt.Errorf("durable ref blob_path is required")
	}
	return nil
}

func normalizeRefFromURI(ref DurableRef) (DurableRef, error) {
	parsed, err := url.Parse(ref.URI)
	if err != nil {
		return DurableRef{}, err
	}
	switch parsed.Scheme {
	case "azblob", "https":
		return NormalizeAzureRef(ref)
	default:
		return DurableRef{}, fmt.Errorf("durable ref cannot be normalized from uri scheme %q", parsed.Scheme)
	}
}

func safeOrPlaceholder(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "_"
	}
	return fileutil.SafePathComponent(value)
}
