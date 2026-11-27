# Releasing TauGrid

TauGrid uses one coordinated stable version for the `tau`, `taugrid-portal`,
`tau-core-controller`, and `taugrid-metrics-collector` images plus the
`taugrid`, `taugrid-core`, and `tau-core-controller` charts. The Python SDK and
the `gpu-monitoring` and `adx-mon` charts retain their independent versions.

The immutable annotated `vX.Y.Z` tag is the release source and version. Pushing
the tag does not publish anything. A maintainer manually starts **Complete
TauGrid Release** from protected control code after the source commit is
reviewed on `main`.

The protected coordinator owns mutation. The public **Release readiness**
workflow is read only. A release is complete only after the coordinator
publishes all artifacts and the public verifier confirms their provenance.

## Distribution contract

The GitHub Release contains:

- `tau-{darwin,linux}-{amd64,arm64}`
- `tau-windows-amd64.exe`
- `tau-gen-{darwin,linux}-{amd64,arm64}`
- `tau-gen-windows-amd64.exe`
- `tau-<sdk-version>-py3-none-any.whl`
- `install.sh`
- `install.ps1`
- `LICENSE`
- `SHA256SUMS`

Linux binaries are statically linked. Darwin binaries use CGO for macOS
Keychain access and target macOS 13.0 or newer. Windows binaries are native PE
executables. The Python SDK wheel is built twice and compared before
publication.

The four coordinated image indexes must contain `linux/amd64` and
`linux/arm64` manifests and carry
`org.opencontainers.image.revision=<full release SHA>`.

Every packaged chart carries:

- `org.opencontainers.image.revision=<full source SHA>`
- `org.taugrid.chart.source-tree=<git tree SHA for charts/NAME>`

The source-tree annotation covers every tracked file in that chart directory.
The three coordinated charts use `X.Y.Z` for both `version` and `appVersion`.
The independently versioned charts keep their tagged versions and may reuse an
existing immutable package only when its source-tree provenance matches.

## Prepare

1. Land release notes and every intended source change on `main`.
2. Record the full 40-character source SHA.
3. Run the repository build, test, lint, chart, installer, and release
   reproduction gates.
4. Create and push an annotated `vX.Y.Z` tag on that exact source SHA.
   Repository rules must prevent release-tag updates and deletion.
5. Review the tag, source SHA, release notes, expected image and chart names,
   CLI/SDK asset manifest, and protected coordinator inputs.

Do not create a release tag from an unreviewed branch. Do not repair a release
by moving its tag, overwriting an immutable artifact, or forcing publication.

## Protected publication

Manually queue **Complete TauGrid Release** from its protected default branch
with `tag=vX.Y.Z`. The coordinator must re-fetch both protected control and the
tag before publication and hold its exclusive release lock from publisher
discovery through final public verification.

The coordinator performs this order:

1. Resolve the annotated tag to one exact `main` SHA and derive `X.Y.Z`.
2. Build the allowlisted CLI, SDK, installer, license, checksum, image, and
   chart artifact set from that SHA.
3. Queue the four protected image publishers with the exact source identity,
   stable version, and `forcePublish=false`.
4. Queue the protected chart publisher with the interface below.
5. Run the public verifier in `--artifacts-only` mode and verify the image
   indexes and chart packages, including architectures, source revision, chart
   source trees, immutable reuse, and downloaded bytes.
6. Create or resume the matching GitHub draft, upload the exact verified asset
   allowlist, verify every public asset name, checksum, and byte, then publish
   the GitHub Release last.
7. Run the full read-only public readiness verifier against the published tag,
   SHA, version, images, charts, and GitHub assets.

A published GitHub Release is not sufficient by itself. **Release ready** is
the final public distribution result.

### Protected chart publisher interface

`.pipelines/publish-helm-charts.yml` accepts:

| Parameter | Required release value |
| --- | --- |
| `releaseTag` | Annotated `vX.Y.Z` tag |
| `releaseSourceCommit` | Exact 40-character source SHA |
| `releaseVersion` | Stable `X.Y.Z` version |

The coordinator pins repository resources independently:

- `self` is protected control at the exact current protected-branch SHA.
- `releaseSource` is the annotated tag at the exact `sourceRef` SHA.

The first publisher job has no publication credentials. It validates protected
control, the annotated tag, the release SHA, `main` ancestry, clean tracked
chart inputs, and chart metadata. It executes only protected control scripts.
Tag-selected chart scripts are never executed. Helm treats the release checkout
as data, and the job transfers only sealed packages plus
`release-manifest.json`.

The credentialed publisher job starts on a fresh worker, checks out only
protected control, downloads the sealed pipeline artifact, and revalidates the
manifest, package hashes, chart identities, versions, source trees, and embedded
revision annotations before registry access. Existing immutable tags are pulled
and verified before reuse. New packages are pulled back, verified, and compared
byte-for-byte after publication. On success it publishes the
`taugrid-chart-publication` pipeline artifact containing `publication.json`.
That document records schema version `1`, the release tag, version, source SHA,
registry, repository root, and the five chart names, versions, repositories,
digests, and immutable-reuse decisions.

The public readiness pipeline contains no registry credentials or mutation
tasks. Its `--artifacts-only` mode is the pre-release publication gate; the full
mode is the post-release completion gate. Neither mode can queue publishers,
repair artifacts, or replace a release.

## Local release checks

Build the CLI/SDK asset set twice from a clean checkout:

```bash
python3 -m venv /tmp/tau-release-venv
/tmp/tau-release-venv/bin/python -m pip install build==1.5.0

VERSION=vX.Y.Z
COMMIT="$(git rev-parse HEAD)"
DATE="$(git show -s --format=%cI HEAD)"
export SOURCE_DATE_EPOCH="$(git show -s --format=%ct HEAD)"
/tmp/tau-release-venv/bin/python -m build --wheel \
  --outdir /tmp/tau-python-wheel-a sdk/python/python
/tmp/tau-release-venv/bin/python -m build --wheel \
  --outdir /tmp/tau-python-wheel-b sdk/python/python
WHEEL_A="$(find /tmp/tau-python-wheel-a -maxdepth 1 -type f -name 'tau-*.whl')"
WHEEL_B="$(find /tmp/tau-python-wheel-b -maxdepth 1 -type f -name 'tau-*.whl')"
cmp "$WHEEL_A" "$WHEEL_B"

make -C cli release-assets \
  VERSION="$VERSION" COMMIT="$COMMIT" DATE="$DATE" \
  RELEASE_DIR=/tmp/tau-release-a PYTHON_WHEEL="$WHEEL_A"
make -C cli release-assets \
  VERSION="$VERSION" COMMIT="$COMMIT" DATE="$DATE" \
  RELEASE_DIR=/tmp/tau-release-b PYTHON_WHEEL="$WHEEL_B"
diff -u /tmp/tau-release-a/SHA256SUMS /tmp/tau-release-b/SHA256SUMS
while read -r _ asset; do
  cmp "/tmp/tau-release-a/$asset" "/tmp/tau-release-b/$asset"
done < /tmp/tau-release-a/SHA256SUMS
```

Package and verify charts from the tagged checkout with protected scripts:

```bash
export RELEASE_SOURCE_COMMIT="$(git rev-parse HEAD)"
export RELEASE_VERSION=X.Y.Z
scripts/ci/publish-helm-charts.sh package /tmp/release-helm
python3 scripts/ci/verify_protected_chart_packages.py \
  directory \
  --directory /tmp/release-helm \
  --commit "$RELEASE_SOURCE_COMMIT" \
  --version "$RELEASE_VERSION"
```

The `publish` command requires the approved protected registry environment. Do
not run it from an ordinary developer session.

## Recovery

Retries must use the same tag, source SHA, version, protected control revision,
publisher definitions, and exact asset allowlist. A retry may reuse only:

- an image index with the exact source revision and required architectures;
- a chart package with matching version, source tree, and revision provenance;
- a GitHub draft whose tag, target, notes, names, checksums, and bytes match the
  rebuilt candidate.

A published GitHub Release blocks publication retries. Public readiness can be
rerun safely because it is read only:

```bash
gh workflow run release-ready.yaml \
  --repo Azure/taugrid \
  --ref main \
  -f tag=vX.Y.Z
```

The verifier may also be run locally from a checkout with full tag history:

```bash
python3 scripts/ci/verify-release-readiness.py \
  --tag vX.Y.Z \
  --commit FULL_SOURCE_SHA \
  --version X.Y.Z \
  --timeout 1800
```

Exit `0` means the exact public distribution is ready. Exit `3` means one or
more expected public artifacts still return `404`. Verification, provenance,
authentication, rate-limit, server, and network failures return `1`; invalid
usage returns `2`.
