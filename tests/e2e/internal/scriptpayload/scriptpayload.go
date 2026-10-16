// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package scriptpayload

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/Azure/taugrid/tests/e2e/internal/taukeys"
)

const (
	MaxDecodedBytes = 64 * 1024

	envelopeVersion = 1

	AnnotationDigest = taukeys.AnnotationPayloadDigest

	InitContainerName = "tau-payload"

	EnvB64       = "TAU_PAYLOAD_B64"
	EnvDigest    = "TAU_PAYLOAD_DIGEST"
	EnvTargetDir = "TAU_PAYLOAD_TARGET_DIR"
)

type envelope struct {
	Version int            `json:"version"`
	Files   []envelopeFile `json:"files"`
}

type envelopeFile struct {
	Name string `json:"name"`
	Data string `json:"data"`
}

func Encode(files map[string][]byte) (encoded string, digest string, err error) {
	size := 0
	for _, data := range files {
		size += len(data)
	}
	if size > MaxDecodedBytes {
		return "", "", fmt.Errorf(
			"generated payload is %d bytes, which exceeds the embedded payload cap of %d bytes (%d KiB); "+
				"reduce the size of the generated script/requirements, or move large assets into a custom "+
				"Ray image or a mounted PVC instead of embedding them directly in the workload spec",
			size, MaxDecodedBytes, MaxDecodedBytes/1024,
		)
	}

	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)

	env := envelope{Version: envelopeVersion, Files: make([]envelopeFile, 0, len(names))}
	for _, name := range names {
		env.Files = append(env.Files, envelopeFile{
			Name: name,
			Data: base64.StdEncoding.EncodeToString(files[name]),
		})
	}
	raw, err := json.Marshal(env)
	if err != nil {
		return "", "", fmt.Errorf("marshal payload envelope: %w", err)
	}
	sum := sha256.Sum256(raw)
	return base64.StdEncoding.EncodeToString(raw), hex.EncodeToString(sum[:]), nil
}

func Decode(encoded, digest string) (map[string][]byte, error) {
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("decode payload: %w", err)
	}
	sum := sha256.Sum256(raw)
	got := hex.EncodeToString(sum[:])
	if got != digest {
		return nil, fmt.Errorf("payload integrity check failed: digest=%s want=%s", got, digest)
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
