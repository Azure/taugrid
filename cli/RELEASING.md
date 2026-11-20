# Releasing Tau

Tau releases use canonical SemVer tags (`vX.Y.Z`). Pushing a tag does not
publish a GitHub Release. Maintainers manually dispatch **Release TauGrid**
from the existing annotated tag ref. The complete release order is:

`release tag -> CLI/SDK release and installer checks -> image builds -> public image availability -> Helm publication -> Release ready`

A published GitHub Release means CLI and SDK assets are available, not that
the complete distribution is ready. **Release readiness** runs separately after
**Release TauGrid** succeeds; only its successful **Release ready** job proves
that all required public images and Helm charts have matching provenance.

## Distribution contract

Each release contains raw binaries and one checksum manifest. Linux binaries are
statically linked. Darwin binaries use CGO for macOS Keychain access and link
only to macOS system libraries and frameworks. Both Darwin architectures target
macOS 13.0 or newer. Windows amd64 binaries are native PE executables.

- `tau-{darwin,linux}-{amd64,arm64}` and `tau-windows-amd64.exe`
- `tau-gen-{darwin,linux}-{amd64,arm64}` and `tau-gen-windows-amd64.exe`
- `tau-<sdk-version>-py3-none-any.whl`
- `install.sh`
- `install.ps1`
- `LICENSE`
- `SHA256SUMS`

The Tau Python SDK keeps its own package version. The workflow builds its wheel
twice from the tagged source revision, compares the outputs, and publishes the
wheel with the CLI assets.

## Prepare

1. Land a release-preparation PR that updates install docs and
   `releases/vX.Y.Z.md`.
2. Choose a source commit on `main` and record its full SHA. The commit must
   contain every feature named in the release notes.
3. Build twice from a clean macOS checkout using the exact Go version in
   `go.mod`. Darwin authentication-cache support requires CGO; the release
   workflow pins an arm64 `macos-15` runner that can also cross-build Darwin
   amd64 and static Linux binaries:

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
     RELEASE_DIR=/tmp/tau-release-a \
     PYTHON_WHEEL="$WHEEL_A"
   make -C cli release-assets \
     VERSION="$VERSION" COMMIT="$COMMIT" DATE="$DATE" \
     RELEASE_DIR=/tmp/tau-release-b \
     PYTHON_WHEEL="$WHEEL_B"
   diff -u /tmp/tau-release-a/SHA256SUMS /tmp/tau-release-b/SHA256SUMS
   while read -r _ asset; do
     cmp "/tmp/tau-release-a/$asset" "/tmp/tau-release-b/$asset"
   done < /tmp/tau-release-a/SHA256SUMS
   ```

4. Run the Go and Python gates used by `.github/workflows/release-tau.yaml`.
5. Review the candidate manifest, release notes, source SHA, and all gate
   results. Release publication requires explicit human authorization.

## Publish after authorization

1. Create an annotated `vX.Y.Z` tag on the reviewed `main` source commit and
   push only that tag. Repository tag rules must prevent updates or deletion of
   release tags. Pushing the tag does not start **Release TauGrid**.
2. Dispatch **Release TauGrid** from the same tag ref and supply the matching
   tag. Use the same command to retry a matching draft:

   ```bash
   gh workflow run release-tau.yaml \
     --repo Azure/taugrid \
     --ref vX.Y.Z \
     -f tag=vX.Y.Z
   ```
3. The read-only validation job verifies that the tag is annotated, follows
   SemVer, points to `main`, has checked-in release notes, and has no published
   GitHub Release.
4. The workflow appends GitHub-generated change attribution, new-contributor
   recognition, and the full changelog link to the checked-in curated notes.
   It then reruns all release gates, compares two independent builds, and
   transfers the validated assets to a separate write-authorized publish job.
5. The publish job creates a draft release, uploads the assets once, verifies
   every GitHub asset digest, and only then publishes the draft. A retry resumes
   a draft only when its metadata, release notes, and assets exactly match the
   rebuilt release. It never overwrites an existing release or asset.
6. Before publication, a clean GitHub-hosted Windows runner verifies the two
   Windows executables and the `tau` release version. After publication,
   Ubuntu and macOS runners install the Python SDK wheel and exercise native
   CLI commands, while a Windows runner installs `tau` through `install.ps1`
   and exercises both Windows executables. A failure leaves the immutable
   published release unchanged when it occurs after publication.
7. The governed distribution coordinator requires the published stable release
   and a successful `release-tau.yaml` dispatch for the same annotated tag and
   full source SHA, including every installer smoke check. It builds `tau`,
   `taugrid-portal`, `tau-core-controller`, and `taugrid-metrics-collector`.
   Before publishing charts it verifies that the public image index digests
   match the publication output and carry the release source revision.
8. The coordinator packages and publishes the five first-party charts from
   that same source commit. The separate public **Release readiness** workflow
   waits up to four hours for the complete public distribution and exposes
   **Release ready** only after successful verification. A timeout or provenance
   mismatch is a failed readiness check, never a partial success.

Every tag must contain its own release notes and use a manual CLI release run
from the matching tag ref. The readiness workflow never modifies the release,
its notes, or registry state, and never invokes the distribution coordinator.

## Chart provenance and immutable reuse

The publisher requires `RELEASE_SOURCE_COMMIT=<full 40-character SHA>` and
`RELEASE_VERSION=X.Y.Z`, with checkout `HEAD` at that SHA and clean tracked chart
inputs. The publishing and readiness helpers use only Python's standard library.
Run both commands from the same source:

```bash
export RELEASE_SOURCE_COMMIT="$(git rev-parse HEAD)"
export RELEASE_VERSION=X.Y.Z
scripts/ci/publish-helm-charts.sh package /tmp/release-helm
scripts/ci/publish-helm-charts.sh publish /tmp/release-helm
```

The publication environment supplies its approved registry authentication and
destination. Packaging seals all five archives in `release-manifest.json` with
the full commit, release version, per-chart versions, source trees, and archive
SHA-256 hashes. Publication verifies that exact manifest and those exact bytes.
Both existing and newly published archives are pulled back and verified.

Each packaged top-level `Chart.yaml` carries
`org.opencontainers.image.revision=<full source SHA>` and
`org.taugrid.chart.source-tree=<git tree SHA for charts/NAME>`. The latter
identifies every tracked file in that chart directory, including templates,
values, tests, lock files and tracked dependency inputs; untracked generated
archives and packaging timestamps do not affect it.

`tau-core-controller`, `taugrid-core`, and `taugrid` must have both `version`
and `appVersion` equal to the release version, and their packaged revision must
equal the release commit. `gpu-monitoring` and `adx-mon` keep the versions in
the tagged `Chart.yaml`; they may reuse an immutable historical package only
when its source-tree annotation matches the tagged chart subtree exactly.
Their revision may therefore identify an older commit. A changed subtree
requires a new independent chart version.

An existing package without provenance, or with a conflicting source tree or
revision, fails closed. Never overwrite it or stamp old published packages
retroactively. Bootstrap the new flow with a fresh product release containing
these helpers, and bump any independent chart version whose existing package
lacks matching provenance. Releases published before this contract, including
`v0.4.8`, do not satisfy it.

Image publication passes
`DOCKER_BUILD_ARGS="--annotation index:org.opencontainers.image.revision=SHA --label org.opencontainers.image.revision=SHA"`
to each image's `docker-push` target. The public gate requires the exact source
revision on each image index and both `linux/amd64` and `linux/arm64` entries.
Existing image tags without that provenance are not silently reused.

## Read-only recovery

If publication finishes after the automatic readiness wait expires, manually
dispatch **Release readiness** with the stable tag:

```bash
gh workflow run release-ready.yaml --repo Azure/taugrid --ref main -f tag=vX.Y.Z
```

The verifier resolves the annotated tag through the public repository API,
matches its full commit to a successful CLI release workflow and a published
stable release, reads chart metadata from that commit, and checks the tag again
while waiting. Missing registry artifacts (`404`) are retried; authentication,
rate-limit, server and network errors fail explicitly instead of masquerading
as missing artifacts.

The legacy `.pipelines/publish-helm-charts.yml` has no branch trigger and no
publication credentials or tasks. It is a manual, read-only verifier requiring
the stable tag, full source SHA and version, with a 30-minute bound. It cannot
bypass the governed publisher. The same verifier can be run locally from a
checkout with full tag history and `origin/main`:

```bash
python3 scripts/ci/verify-release-readiness.py \
  --tag vX.Y.Z --commit FULL_SOURCE_SHA --version X.Y.Z --timeout 1800
```

Coordinators may add `--check-once` for a no-wait probe. Exit `0` means the
exact release is ready, `3` means only that one or more public MCR artifacts
still return `404`, `1` means verification failed, and `2` means invalid command
usage. Authentication, rate-limit, server, network, digest, workflow and
provenance failures return `1`, never `3`.

`GH_TOKEN` is optional for public reads outside GitHub Actions but recommended
to avoid anonymous API rate limits. The GitHub workflow uses only `contents:
read` and `actions: read`; its check belongs to the readiness workflow run,
and its summary identifies the verified release tag and source SHA.

## Verify

Confirm the post-publication Ubuntu and macOS jobs succeeded. They exercise the
published CLI installer, Python SDK wheel, and native version and help commands.
The earlier release jobs compare every GitHub asset digest with `SHA256SUMS`,
compare two SDK wheel builds, and inspect all cross-compiled binaries with
`go version -m`. Do not update downstream minimum-version requirements until
the full CLI workflow and the separate **Release ready** check both succeed.
