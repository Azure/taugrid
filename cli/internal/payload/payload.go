// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

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
	MaxDecodedBytes = 1024 * 1024

	MaxEnvEntryBytes = 64 * 1024

	maxArgStrlen = 131072

	envelopeVersion = 2

	AnnotationDigest = workloadmeta.AnnotationPayloadDigest

	InitContainerName = "tau-payload"

	EnvB64       = "TAU_PAYLOAD_B64"
	EnvDigest    = "TAU_PAYLOAD_DIGEST"
	EnvTargetDir = "TAU_PAYLOAD_TARGET_DIR"
)

type File struct {
	Name string
	Data []byte
}

type Payload struct {
	Files []File
}

type envelope struct {
	Version int            `json:"version"`
	Files   []envelopeFile `json:"files"`
}

type envelopeFile struct {
	Name string `json:"name"`
	Data string `json:"data"`
}

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

func (p Payload) DecodedSize() int {
	n := 0
	for _, f := range p.Files {
		n += len(f.Data)
	}
	return n
}

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

	if entry := len(EnvB64) + 1 + len(encoded); entry > MaxEnvEntryBytes {
		return "", "", sizeError("encoded payload environment entry", entry, MaxEnvEntryBytes)
	}
	return encoded, hex.EncodeToString(sum[:]), nil
}

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
