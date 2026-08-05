// Package scriptpayload is a small, test-only mirror of the wire format
// produced by cli/internal/payload (envelope version 1,
// established by PR1 for issues #869/#871). It exists solely so the
// tests/e2e Go module -- a SEPARATE module from cli, and
// therefore unable to import an `internal` package outside its own module
// tree -- can embed manager-routed RayJob fixtures (tests/e2e/stack/fixtures)
// with the same self-contained, head-only payload delivery mechanism that
// Tau's renderer uses, instead of the ConfigMap-based script delivery that
// MultiKueue does not mirror to worker clusters.
//
// Duplication boundary: this package independently re-implements Encode,
// Decode, and InitContainerScript against the SAME wire contract as
// cli/internal/payload (JSON envelope {"version":1,"files":
// [{"name":...,"data":<base64>}]}, canonically sorted by file name, SHA-256
// digest over the pre-outer-base64 JSON bytes, MaxDecodedBytes = 64 KiB). It
// is intentionally NOT compared against payload.go's source text -- that
// would be a brittle assertion that breaks on any refactor of the real
// implementation even when behavior is unchanged. Instead, correctness is
// pinned with behavioral golden vectors in scriptpayload_test.go: fixed
// inputs whose exact encoded envelope and digest were generated once from
// the real cli/internal/payload package and hardcoded here, so
// a future drift in either implementation's wire format is caught by a
// failing test rather than a silent divergence.
//
// If the envelope format in cli/internal/payload ever changes
// (envelope version bump, field rename, different digest scheme), this
// package and its golden vectors must be updated in lockstep, and the
// InitContainerScript constant below must be kept byte-for-byte identical to
// payload.InitContainerScript, since it is embedded directly into the static
// YAML fixtures as the runtime decoder -- there is no Go rendering step for
// these three fixtures to keep it in sync automatically.
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
	// MaxDecodedBytes mirrors payload.MaxDecodedBytes: the maximum total size,
	// in bytes, of all file contents in a payload before encoding.
	MaxDecodedBytes = 64 * 1024

	// envelopeVersion mirrors payload.envelopeVersion.
	envelopeVersion = 1

	// AnnotationDigest mirrors payload.AnnotationDigest.
	AnnotationDigest = taukeys.AnnotationPayloadDigest

	// InitContainerName mirrors payload.InitContainerName.
	InitContainerName = "tau-payload"

	// EnvB64 mirrors payload.EnvB64.
	EnvB64 = "TAU_PAYLOAD_B64"
	// EnvDigest mirrors payload.EnvDigest.
	EnvDigest = "TAU_PAYLOAD_DIGEST"
	// EnvTargetDir mirrors payload.EnvTargetDir.
	EnvTargetDir = "TAU_PAYLOAD_TARGET_DIR"
)

// envelope is the deterministic wire format produced by Encode. Field order
// and file order are both fixed so that encoding the same inputs always
// produces byte-identical output, matching payload.envelope.
type envelope struct {
	Version int            `json:"version"`
	Files   []envelopeFile `json:"files"`
}

type envelopeFile struct {
	Name string `json:"name"`
	Data string `json:"data"` // base64-encoded raw file bytes
}

// Encode canonically serializes files (keyed by file name), enforcing
// MaxDecodedBytes, and returns the base64-encoded envelope plus its SHA-256
// digest (hex). Mirrors payload.Encode(payload.New(files)).
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

// Decode reverses Encode, verifying the digest before returning file
// contents keyed by name. Mirrors payload.Decode.
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

// InitContainerScript MUST stay byte-for-byte identical to
// cli/internal/payload.InitContainerScript. It is embedded
// directly as a literal YAML block scalar in each of the three converted
// RayJob fixtures (see tests/e2e/stack/fixtures/inference-rayjob.yaml,
// inference-rayjob-gpu.yaml, and fineweb-rayjob-16xh200-ib.yaml), so it IS
// the runtime decoder for those fixtures -- not a copy used only in tests.
// TestFixturesEmbedIdenticalInitContainerScript in scriptpayload_test.go
// pins all three fixtures to this exact constant.
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
