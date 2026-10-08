#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

set -euo pipefail

REPO_ROOT="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../../.." && pwd)"
readonly REPO_ROOT
readonly SBOM_TOOL="${REPO_ROOT}/scripts/ci/release_sbom.py"
readonly WORK_DIR="${REPO_ROOT}/.release-sbom-test-${PPID}"
readonly RELEASE_DIR="${WORK_DIR}/release"

cleanup() {
  rm -rf "${WORK_DIR}"
}
trap cleanup EXIT

fail() {
  echo "release SBOM test failed: $*" >&2
  exit 1
}

expect_failure() {
  local expected="$1"
  shift
  local output
  if output="$("$@" 2>&1)"; then
    fail "command unexpectedly succeeded: $*"
  fi
  grep -Fq "${expected}" <<<"${output}" ||
    fail "failure did not contain '${expected}': ${output}"
}

expect_generate_failure() {
  local expected="$1"
  local images="$2"
  expect_failure "${expected}" \
    python3 "${SBOM_TOOL}" generate \
      --syft "${WORK_DIR}/fake-syft" \
      --release-dir "${RELEASE_DIR}" \
      --images "${images}" \
      --release-tag v0.4.3 \
      --source-commit 0123456789abcdef0123456789abcdef01234567 \
      --generated-at 2026-10-08T20:00:00Z
}

mkdir -p "${RELEASE_DIR}"

python3 "${SBOM_TOOL}" image-specs \
  --repo-root "${REPO_ROOT}" \
  --output "${WORK_DIR}/tagged-images.json"
python3 - "${WORK_DIR}/tagged-images.json" <<'PY'
import json
import sys

images = json.load(open(sys.argv[1], encoding="utf-8"))
assert [item["component"] for item in images] == [
    "tau",
    "taugrid-portal",
    "tau-core-controller",
]
assert all(item["chartVersion"] == "0.4.3" for item in images)
assert all(item["tagReference"].endswith(":0.4.3") for item in images)
PY

python3 - "${SBOM_TOOL}" <<'PY'
import importlib.util
import email.message
import json
import sys
import urllib.error

spec = importlib.util.spec_from_file_location("release_sbom", sys.argv[1])
module = importlib.util.module_from_spec(spec)
spec.loader.exec_module(module)

requests = []


class UrlResponse:
    def __init__(self, body):
        self.body = body
        self.headers = {}

    def __enter__(self):
        return self

    def __exit__(self, *_):
        return False

    def read(self, *_):
        return self.body


def fake_urlopen(request, timeout):
    requests.append(request)
    if len(requests) == 1:
        headers = email.message.Message()
        headers["WWW-Authenticate"] = (
            'Bearer realm="https://auth.example/token",'
            'service="registry.example",scope="repository:example/image:pull"'
        )
        raise urllib.error.HTTPError(
            request.full_url, 401, "Unauthorized", headers, None
        )
    if len(requests) == 2:
        return UrlResponse(b'{"token":"registry-token"}')
    return UrlResponse(b'{"schemaVersion":2}')


module.urllib.request.urlopen = fake_urlopen
with module.registry_request(
    "https://registry.example/v2/example/image/manifests/1.0.0",
    {"Accept": "application/vnd.oci.image.index.v1+json"},
) as response:
    assert response.read() == b'{"schemaVersion":2}'
assert requests[2].get_header("Authorization") == "Bearer registry-token"

manifest = {
    "schemaVersion": 2,
    "manifests": [
        {
            "digest": "sha256:" + "1" * 64,
            "platform": {"os": "linux", "architecture": "amd64"},
        },
        {
            "digest": "sha256:" + "2" * 64,
            "platform": {"os": "linux", "architecture": "arm64"},
        },
        {
            "digest": "sha256:" + "3" * 64,
            "platform": {"os": "unknown", "architecture": "unknown"},
        },
    ],
}


class Response:
    headers = {"Docker-Content-Digest": "sha256:" + "a" * 64}

    def __enter__(self):
        return self

    def __exit__(self, *_):
        return False

    def read(self):
        return json.dumps(manifest).encode()


module.registry_request = lambda *_: Response()
resolved = module.resolve_image("mcr.microsoft.com/example/image:1.0.0")
assert resolved["imageIndex"].endswith("@sha256:" + "a" * 64)
assert [item["platform"] for item in resolved["platforms"]] == [
    "linux/amd64",
    "linux/arm64",
]
assert resolved["platforms"][0]["platformDigest"] == "sha256:" + "1" * 64
assert resolved["platforms"][1]["platformDigest"] == "sha256:" + "2" * 64
PY

cat > "${WORK_DIR}/resolved-images.json" <<'JSON'
[
  {
    "component": "tau",
    "chart": "taugrid-core",
    "chartVersion": "0.4.3",
    "componentVersion": "0.4.3",
    "tagReference": "mcr.microsoft.com/aks/ai-runtime/tau:0.4.3",
    "imageIndex": "mcr.microsoft.com/aks/ai-runtime/tau@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
    "imageIndexDigest": "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
    "platforms": [
      {
        "platform": "linux/amd64",
        "platformDigest": "sha256:a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1",
        "platformImage": "mcr.microsoft.com/aks/ai-runtime/tau@sha256:a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1"
      },
      {
        "platform": "linux/arm64",
        "platformDigest": "sha256:a2a2a2a2a2a2a2a2a2a2a2a2a2a2a2a2a2a2a2a2a2a2a2a2a2a2a2a2a2a2a2a2",
        "platformImage": "mcr.microsoft.com/aks/ai-runtime/tau@sha256:a2a2a2a2a2a2a2a2a2a2a2a2a2a2a2a2a2a2a2a2a2a2a2a2a2a2a2a2a2a2a2a2"
      }
    ]
  },
  {
    "component": "taugrid-portal",
    "chart": "taugrid-core",
    "chartVersion": "0.4.3",
    "componentVersion": "0.4.3",
    "tagReference": "mcr.microsoft.com/aks/ai-runtime/taugrid-portal:0.4.3",
    "imageIndex": "mcr.microsoft.com/aks/ai-runtime/taugrid-portal@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
    "imageIndexDigest": "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
    "platforms": [
      {
        "platform": "linux/amd64",
        "platformDigest": "sha256:b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1",
        "platformImage": "mcr.microsoft.com/aks/ai-runtime/taugrid-portal@sha256:b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1"
      },
      {
        "platform": "linux/arm64",
        "platformDigest": "sha256:b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2",
        "platformImage": "mcr.microsoft.com/aks/ai-runtime/taugrid-portal@sha256:b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2"
      }
    ]
  },
  {
    "component": "tau-core-controller",
    "chart": "tau-core-controller",
    "chartVersion": "0.4.3",
    "componentVersion": "0.4.3",
    "tagReference": "mcr.microsoft.com/aks/ai-runtime/tau-core-controller:0.4.3",
    "imageIndex": "mcr.microsoft.com/aks/ai-runtime/tau-core-controller@sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
    "imageIndexDigest": "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
    "platforms": [
      {
        "platform": "linux/amd64",
        "platformDigest": "sha256:c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1",
        "platformImage": "mcr.microsoft.com/aks/ai-runtime/tau-core-controller@sha256:c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1"
      },
      {
        "platform": "linux/arm64",
        "platformDigest": "sha256:c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2",
        "platformImage": "mcr.microsoft.com/aks/ai-runtime/tau-core-controller@sha256:c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2"
      }
    ]
  }
]
JSON

cat > "${WORK_DIR}/fake-syft" <<'PY'
#!/usr/bin/env python3
import json
import pathlib
import sys

source = sys.argv[2]
source_name = sys.argv[sys.argv.index("--source-name") + 1]
output = next(value for value in sys.argv if value.startswith("spdx-json="))
path = pathlib.Path(output.split("=", 1)[1])
document = {
    "SPDXID": "SPDXRef-DOCUMENT",
    "spdxVersion": "SPDX-2.3",
    "dataLicense": "CC0-1.0",
    "name": source_name,
    "comment": " ".join(sys.argv),
    "documentNamespace": "https://example.invalid/volatile",
    "creationInfo": {
        "created": "2099-01-01T00:00:00Z",
        "creators": ["Tool: fake-syft"],
    },
    "packages": [
        {
            "SPDXID": "SPDXRef-Package",
            "name": source_name,
            "downloadLocation": "NOASSERTION",
        }
    ],
    "relationships": [
        {
            "spdxElementId": "SPDXRef-DOCUMENT",
            "relationshipType": "DESCRIBES",
            "relatedSpdxElement": "SPDXRef-Package",
        }
    ],
}
path.write_text(json.dumps(document), encoding="utf-8")
PY
chmod 0755 "${WORK_DIR}/fake-syft"

for asset in \
  tau-darwin-amd64 tau-darwin-arm64 tau-linux-amd64 tau-linux-arm64 \
  tau-windows-amd64.exe tau-gen-darwin-amd64 tau-gen-darwin-arm64 \
  tau-gen-linux-amd64 tau-gen-linux-arm64 tau-gen-windows-amd64.exe; do
  printf 'representative binary %s\n' "${asset}" > "${RELEASE_DIR}/${asset}"
done
printf 'wheel\n' > "${RELEASE_DIR}/tau-0.4.3-py3-none-any.whl"
printf 'license\n' > "${RELEASE_DIR}/LICENSE"
printf 'installer\n' > "${RELEASE_DIR}/install.sh"
printf 'installer\n' > "${RELEASE_DIR}/install.ps1"

python3 "${SBOM_TOOL}" generate \
  --syft "${WORK_DIR}/fake-syft" \
  --release-dir "${RELEASE_DIR}" \
  --images "${WORK_DIR}/resolved-images.json" \
  --release-tag v0.4.3 \
  --source-commit 0123456789abcdef0123456789abcdef01234567 \
  --generated-at 2026-10-08T20:00:00Z
python3 "${SBOM_TOOL}" validate --release-dir "${RELEASE_DIR}"

python3 - "${RELEASE_DIR}" <<'PY'
import json
import pathlib
import sys

release_dir = pathlib.Path(sys.argv[1])
index = json.loads((release_dir / "taugrid-release-sbom-index.json").read_text())
assert index["schemaVersion"] == "2.0"
assert len(index["cli"]) == 10
assert len(index["images"]) == 6
assert {entry["component"] for entry in index["images"]} == {
    "tau",
    "taugrid-portal",
    "tau-core-controller",
}
assert {
    (entry["component"], entry["platform"]) for entry in index["images"]
} == {
    (component, platform)
    for component in ("tau", "taugrid-portal", "tau-core-controller")
    for platform in ("linux/amd64", "linux/arm64")
}
assert all("@sha256:" in entry["imageIndex"] for entry in index["images"])
assert all("@sha256:" in entry["platformImage"] for entry in index["images"])
checksums = (release_dir / "SHA256SUMS").read_text()
assert "taugrid-release-sbom-index.json" in checksums
assert all(entry["sbom"] in checksums for entry in index["cli"] + index["images"])
for entry in index["cli"] + index["images"]:
    document = json.loads((release_dir / entry["sbom"]).read_text())
    assert document["creationInfo"]["created"] == "2026-10-08T20:00:00Z"
    assert document["documentNamespace"].startswith(
        "https://github.com/Azure/taugrid/sbom/v0.4.3/"
    )
for entry in index["images"]:
    document = json.loads((release_dir / entry["sbom"]).read_text())
    assert document["name"] == (
        f"{entry['component']}-image-{entry['platform'].replace('/', '-')}"
    )
    assert f"--platform {entry['platform']}" in document["comment"]
PY

cp "${RELEASE_DIR}/taugrid-release-sbom-index.json" "${WORK_DIR}/index.good"
cp "${RELEASE_DIR}/SHA256SUMS" "${WORK_DIR}/checksums.good"
cp "${RELEASE_DIR}/tau-linux-amd64.spdx.json" "${WORK_DIR}/sbom.good"

python3 - "${RELEASE_DIR}/taugrid-release-sbom-index.json" <<'PY'
import json
import pathlib
import sys

path = pathlib.Path(sys.argv[1])
index = json.loads(path.read_text())
index["cli"].append(dict(index["cli"][0]))
path.write_text(json.dumps(index))
PY
expect_failure "duplicate CLI entry" \
  python3 "${SBOM_TOOL}" validate --release-dir "${RELEASE_DIR}"
cp "${WORK_DIR}/index.good" "${RELEASE_DIR}/taugrid-release-sbom-index.json"

python3 - "${RELEASE_DIR}/taugrid-release-sbom-index.json" <<'PY'
import json
import pathlib
import sys

path = pathlib.Path(sys.argv[1])
index = json.loads(path.read_text())
index["images"].append(dict(index["images"][0]))
path.write_text(json.dumps(index))
PY
expect_failure "duplicate image entry" \
  python3 "${SBOM_TOOL}" validate --release-dir "${RELEASE_DIR}"
cp "${WORK_DIR}/index.good" "${RELEASE_DIR}/taugrid-release-sbom-index.json"

python3 - "${RELEASE_DIR}/taugrid-release-sbom-index.json" <<'PY'
import json
import pathlib
import sys

path = pathlib.Path(sys.argv[1])
index = json.loads(path.read_text())
index["images"].pop()
path.write_text(json.dumps(index))
PY
expect_failure "image platform coverage must be exactly" \
  python3 "${SBOM_TOOL}" validate --release-dir "${RELEASE_DIR}"
cp "${WORK_DIR}/index.good" "${RELEASE_DIR}/taugrid-release-sbom-index.json"

python3 - "${RELEASE_DIR}/taugrid-release-sbom-index.json" <<'PY'
import json
import pathlib
import sys

path = pathlib.Path(sys.argv[1])
index = json.loads(path.read_text())
first, second = index["cli"][:2]
first["sbom"], second["sbom"] = second["sbom"], first["sbom"]
first["sbomSha256"], second["sbomSha256"] = (
    second["sbomSha256"],
    first["sbomSha256"],
)
path.write_text(json.dumps(index))
PY
expect_failure "unexpected SBOM filename" \
  python3 "${SBOM_TOOL}" validate --release-dir "${RELEASE_DIR}"
cp "${WORK_DIR}/index.good" "${RELEASE_DIR}/taugrid-release-sbom-index.json"

python3 - "${RELEASE_DIR}/taugrid-release-sbom-index.json" <<'PY'
import json
import pathlib
import sys

path = pathlib.Path(sys.argv[1])
index = json.loads(path.read_text())
first, second = index["images"][:2]
first["sbom"], second["sbom"] = second["sbom"], first["sbom"]
first["sbomSha256"], second["sbomSha256"] = (
    second["sbomSha256"],
    first["sbomSha256"],
)
path.write_text(json.dumps(index))
PY
expect_failure "unexpected SBOM filename" \
  python3 "${SBOM_TOOL}" validate --release-dir "${RELEASE_DIR}"
cp "${WORK_DIR}/index.good" "${RELEASE_DIR}/taugrid-release-sbom-index.json"

printf '\ncorrupt\n' >> "${RELEASE_DIR}/tau-linux-amd64.spdx.json"
expect_failure "CLI SBOM digest mismatch" \
  python3 "${SBOM_TOOL}" validate --release-dir "${RELEASE_DIR}"
cp "${WORK_DIR}/checksums.good" "${RELEASE_DIR}/SHA256SUMS"
cp "${WORK_DIR}/sbom.good" "${RELEASE_DIR}/tau-linux-amd64.spdx.json"
python3 - "${RELEASE_DIR}/taugrid-release-sbom-index.json" "${RELEASE_DIR}/tau-linux-amd64.spdx.json" <<'PY'
import json
import pathlib
import sys

index = json.loads(pathlib.Path(sys.argv[1]).read_text())
entry = next(item for item in index["cli"] if item["asset"] == "tau-linux-amd64")
path = pathlib.Path(sys.argv[2])
document = json.loads(path.read_text())
document["dataLicense"] = "NOASSERTION"
path.write_text(json.dumps(document))
import hashlib
entry["sbomSha256"] = hashlib.sha256(path.read_bytes()).hexdigest()
pathlib.Path(sys.argv[1]).write_text(json.dumps(index, indent=2, sort_keys=True) + "\n")
PY
expect_failure "invalid dataLicense" \
  python3 "${SBOM_TOOL}" validate --release-dir "${RELEASE_DIR}"
cp "${WORK_DIR}/index.good" "${RELEASE_DIR}/taugrid-release-sbom-index.json"
cp "${WORK_DIR}/sbom.good" "${RELEASE_DIR}/tau-linux-amd64.spdx.json"

rm -rf "${RELEASE_DIR}"
mkdir -p "${RELEASE_DIR}"
cp "${WORK_DIR}/resolved-images.json" "${WORK_DIR}/tag-only-images.json"
python3 - "${WORK_DIR}/tag-only-images.json" <<'PY'
import json
import pathlib
import sys

path = pathlib.Path(sys.argv[1])
images = json.loads(path.read_text())
images[0]["imageIndex"] = images[0]["tagReference"]
path.write_text(json.dumps(images))
PY
expect_generate_failure \
  "image index reference and digest do not match" \
  "${WORK_DIR}/tag-only-images.json"

cp "${WORK_DIR}/resolved-images.json" "${WORK_DIR}/missing-platform.json"
python3 - "${WORK_DIR}/missing-platform.json" <<'PY'
import json
import pathlib
import sys

path = pathlib.Path(sys.argv[1])
images = json.loads(path.read_text())
images[0]["platforms"].pop()
path.write_text(json.dumps(images))
PY
expect_generate_failure "platform coverage must be exactly" \
  "${WORK_DIR}/missing-platform.json"

cp "${WORK_DIR}/resolved-images.json" "${WORK_DIR}/duplicate-platform.json"
python3 - "${WORK_DIR}/duplicate-platform.json" <<'PY'
import json
import pathlib
import sys

path = pathlib.Path(sys.argv[1])
images = json.loads(path.read_text())
images[0]["platforms"][1] = dict(images[0]["platforms"][0])
path.write_text(json.dumps(images))
PY
expect_generate_failure "duplicate platform linux/amd64" \
  "${WORK_DIR}/duplicate-platform.json"

cp "${WORK_DIR}/resolved-images.json" "${WORK_DIR}/mismatched-platform.json"
python3 - "${WORK_DIR}/mismatched-platform.json" <<'PY'
import json
import pathlib
import sys

path = pathlib.Path(sys.argv[1])
images = json.loads(path.read_text())
images[0]["platforms"][0]["platformImage"] = images[1]["platforms"][0]["platformImage"]
path.write_text(json.dumps(images))
PY
expect_generate_failure "reference and digest do not match" \
  "${WORK_DIR}/mismatched-platform.json"

echo "Release SBOM tests passed"
