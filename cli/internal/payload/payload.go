// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package payload provides a shared helper for embedding small, deterministic,
// non-secret generated payloads (e.g. a researcher's driver script) directly
// in a Kubernetes workload spec instead of a per-run ConfigMap.
//
// MultiKueue mirrors the workload object (Job/RayJob) to a worker cluster, but
// it does not mirror auxiliary objects such as a generated script ConfigMap.
// Embedding the payload in the workload spec itself makes the workload
// self-contained: nothing else needs to be pre-provisioned or copied to the
// worker for the payload to be available.
//
// Payloads are bounded by two limits. The binding one is MaxEnvEntryBytes: the
// encoded envelope travels as a single TAU_PAYLOAD_B64=<...> environment entry.
// Tau keeps that entry at or below 64 KiB to limit workload metadata
// amplification, well under Linux's MAX_ARG_STRLEN (128 KiB) execve(2) limit.
// MaxDecodedBytes is a much looser sanity ceiling on the sum of raw file bytes.
// Encoding is deterministic: files are canonically ordered by name,
// serialized with a fixed field layout, and gzip-compressed with a fixed level
// and no header metadata, so identical inputs always produce a byte-identical
// envelope and digest. A SHA-256 digest, computed over the transported
// (pre-base64) envelope bytes, lets both the render-time caller and a runtime
// initContainer verify the payload was not corrupted or truncated in transit.
package payload

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"sort"

	"github.com/Azure/taugrid/core/workloadmeta"
)

const (
	// MaxDecodedBytes is a sanity ceiling on the total size, in bytes, of all
	// file contents in a Payload before encoding. It is deliberately NOT the
	// binding constraint: since envelope v2 the transported bytes are
	// gzip-compressed, so the limit that actually matters is
	// MaxEnvEntryBytes, enforced on the encoded result. This ceiling only
	// exists to reject obviously-wrong input (a whole dataset passed as a
	// "script") before spending CPU compressing it.
	MaxDecodedBytes = 1024 * 1024

	// MaxEnvEntryBytes is the binding constraint: the maximum size of the
	// whole "TAU_PAYLOAD_B64=<encoded>" environment entry handed to the
	// tau-payload initContainer. Tau caps embedded entries at 64 KiB to bound
	// Kueue Workload and Pod metadata amplification, while remaining well
	// below Linux's MAX_ARG_STRLEN (131072 bytes) execve(2) limit.
	// See TestEncodedEnvValueStaysUnderMaxArgStrlen.
	MaxEnvEntryBytes = 64 * 1024

	// maxArgStrlen is Linux's MAX_ARG_STRLEN: the kernel's per-argument and
	// per-environment-entry size limit enforced by execve(2).
	maxArgStrlen = 131072

	// envelopeVersion identifies the wire format produced by Encode.
	envelopeVersion = 2

	// AnnotationDigest is stamped on rendered workloads carrying an embedded
	// payload so operators can confirm, via `kubectl describe`/`get -o yaml`,
	// which payload a running workload was rendered with.
	AnnotationDigest = workloadmeta.AnnotationPayloadDigest

	// InitContainerName is the conventional name for the initContainer that
	// decodes, verifies, and writes an embedded payload at runtime.
	InitContainerName = "tau-payload"

	// EnvB64 names the env var carrying the base64-encoded payload envelope
	// passed to the tau-payload initContainer.
	EnvB64 = "TAU_PAYLOAD_B64"
	// EnvDigest names the env var carrying the expected SHA-256 digest (hex)
	// of the payload envelope, used for runtime integrity verification.
	EnvDigest = "TAU_PAYLOAD_DIGEST"
	// EnvTargetDir names the env var carrying the directory the
	// tau-payload initContainer writes decoded files into.
	EnvTargetDir = "TAU_PAYLOAD_TARGET_DIR"
)

// File is a single named file embedded in a Payload.
type File struct {
	Name string
	Data []byte
}

// Payload is a canonically ordered set of generated, non-secret files.
type Payload struct {
	Files []File
}

// envelope is the deterministic wire format produced by Encode. Field order
// and file order are both fixed so that encoding the same inputs always
// produces byte-identical output.
type envelope struct {
	Version int            `json:"version"`
	Files   []envelopeFile `json:"files"`
}

type envelopeFile struct {
	Name string `json:"name"`
	Data string `json:"data"` // base64-encoded raw file bytes
}

// New builds a Payload from a map of file name to contents. Files are
// canonically sorted by name so the resulting encoding is deterministic
// regardless of map iteration order.
func New(files map[string][]byte) Payload {
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	p := Payload{Files: make([]File, 0, len(names))}
	for _, name := range names {
		p.Files = append(p.Files, File{Name: name, Data: files[name]})
	}
	return p
}

// DecodedSize returns the total size, in bytes, of all file contents before
// encoding.
func (p Payload) DecodedSize() int {
	n := 0
	for _, f := range p.Files {
		n += len(f.Data)
	}
	return n
}

// Encode canonically serializes p, enforcing both MaxDecodedBytes and
// MaxEnvEntryBytes, and returns the base64-encoded envelope plus its SHA-256
// digest (hex). The JSON envelope is gzip-compressed before base64 so that a
// researcher's driver script — which is highly compressible text — gets several
// times more usable room under the kernel's per-environment-entry limit than
// the raw wire format allowed. The digest is computed over the transported
// (pre-base64, post-gzip) bytes so runtime verification only needs to hash what
// it received; it does not require re-encoding or decompressing first.
//
// Both size errors are actionable: they name the limit, the actual size, the
// overage, and the concrete remedies.
func Encode(p Payload) (encoded string, digest string, err error) {
	size := p.DecodedSize()
	if size > MaxDecodedBytes {
		return "", "", sizeError("generated payload", size, MaxDecodedBytes)
	}

	env := envelope{Version: envelopeVersion, Files: make([]envelopeFile, 0, len(p.Files))}
	for _, f := range p.Files {
		env.Files = append(env.Files, envelopeFile{
			Name: f.Name,
			Data: base64.StdEncoding.EncodeToString(f.Data),
		})
	}
	raw, err := json.Marshal(env)
	if err != nil {
		return "", "", fmt.Errorf("marshal payload envelope: %w", err)
	}
	compressed, err := compress(raw)
	if err != nil {
		return "", "", err
	}
	sum := sha256.Sum256(compressed)
	encoded = base64.StdEncoding.EncodeToString(compressed)

	// The binding constraint. Checked on the real encoded bytes so that
	// incompressible input is rejected here rather than producing a pod whose
	// initContainer cannot exec.
	if entry := len(EnvB64) + 1 + len(encoded); entry > MaxEnvEntryBytes {
		return "", "", sizeError("encoded payload environment entry", entry, MaxEnvEntryBytes)
	}
	return encoded, hex.EncodeToString(sum[:]), nil
}

// sizeError renders a limit breach with everything a researcher needs to act:
// what was measured, the ceiling, how far over it is, and what to do about it.
func sizeError(what string, actual, limit int) error {
	return fmt.Errorf(
		"%s is %d bytes, which exceeds the limit of %d bytes (%d KiB) by %d bytes; "+
			"payloads are gzip-compressed and embedded directly in the workload spec so the run "+
			"stays self-contained, and a single environment entry cannot exceed the kernel's "+
			"MAX_ARG_STRLEN (%d bytes). Remedies: split rarely-changing code out of the entrypoint "+
			"and bake it into a custom Ray image, or mount large assets from a PVC, instead of "+
			"embedding them in the workload spec",
		what, actual, limit, limit/1024, actual-limit, maxArgStrlen,
	)
}

// compress gzips raw deterministically: a fixed compression level, and no
// header name or modification time, so identical inputs always produce
// byte-identical output (and therefore a stable digest and a stable rendered
// workload spec).
func compress(raw []byte) ([]byte, error) {
	var buf bytes.Buffer
	w, err := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	if err != nil {
		return nil, fmt.Errorf("init payload compressor: %w", err)
	}
	if _, err := w.Write(raw); err != nil {
		return nil, fmt.Errorf("compress payload envelope: %w", err)
	}
	if err := w.Close(); err != nil {
		return nil, fmt.Errorf("compress payload envelope: %w", err)
	}
	return buf.Bytes(), nil
}

// Decode reverses Encode, verifying the digest before returning file
// contents keyed by name. It mirrors the runtime integrity check performed
// by the tau-payload initContainer (see InitContainerScript) and is used by
// tests to assert the two implementations agree.
func Decode(encoded, digest string) (map[string][]byte, error) {
	transported, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("decode payload: %w", err)
	}
	sum := sha256.Sum256(transported)
	got := hex.EncodeToString(sum[:])
	if got != digest {
		return nil, fmt.Errorf("payload integrity check failed: digest=%s want=%s", got, digest)
	}
	r, err := gzip.NewReader(bytes.NewReader(transported))
	if err != nil {
		return nil, fmt.Errorf("decompress payload envelope: %w", err)
	}
	defer r.Close()
	raw, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("decompress payload envelope: %w", err)
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("unmarshal payload envelope: %w", err)
	}
	out := make(map[string][]byte, len(env.Files))
	for _, f := range env.Files {
		data, err := base64.StdEncoding.DecodeString(f.Data)
		if err != nil {
			return nil, fmt.Errorf("decode payload file %q: %w", f.Name, err)
		}
		out[f.Name] = data
	}
	return out, nil
}

// InitContainerScript is a small, dependency-free Python script that decodes
// and verifies a payload embedded via the EnvB64/EnvDigest environment
// variables and writes its files under the directory named by EnvTargetDir.
// It only depends on the Python 3 standard library, which every Tau-managed
// Ray image (GPU and CPU) already ships, so the initContainer can reuse the
// workload's own image rather than pulling an additional one.
//
// The script intentionally mirrors Decode's verification logic (base64
// decode, SHA-256 digest compare, gzip decompression, JSON envelope parse) so
// the two stay in lockstep; render_test.go and payload_test.go both assert this.
const InitContainerScript = `import base64
import gzip
import hashlib
import json
import os
import sys

raw_b64 = os.environ["` + EnvB64 + `"]
want_digest = os.environ["` + EnvDigest + `"]
target_dir = os.environ["` + EnvTargetDir + `"]

raw = base64.b64decode(raw_b64)
got_digest = hashlib.sha256(raw).hexdigest()
if got_digest != want_digest:
    sys.stderr.write(
        "tau-payload: integrity check failed: digest=%s want=%s\n" % (got_digest, want_digest)
    )
    sys.exit(1)

raw = gzip.decompress(raw)

envelope = json.loads(raw)
files = envelope.get("files", [])
os.makedirs(target_dir, exist_ok=True)
for f in files:
    name = f["name"]
    if not name or name in (".", "..") or "/" in name or "\\" in name:
        sys.stderr.write("tau-payload: rejecting unsafe file name %r\n" % (name,))
        sys.exit(1)
    data = base64.b64decode(f["data"])
    path = os.path.join(target_dir, name)
    with open(path, "wb") as fh:
        fh.write(data)
    os.chmod(path, 0o644)

sys.stderr.write("tau-payload: wrote %d file(s) to %s\n" % (len(files), target_dir))
`
