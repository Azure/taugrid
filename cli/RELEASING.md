# Releasing Tau

Tau releases use canonical SemVer tags (`vX.Y.Z`). Pushing a tag does not
publish a GitHub Release. Maintainers publish and verify the coordinated Azure
DevOps images and charts first, then manually dispatch the GitHub Actions
release workflow from the existing tag ref.

## Distribution contract

Each release contains raw binaries and one checksum manifest. Linux binaries are
statically linked. Darwin binaries use CGO for macOS Keychain access and link
only to macOS system libraries and frameworks. Both Darwin architectures target
macOS 12.0 or newer. Windows amd64 binaries are native PE executables.

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
2. Run the public Azure DevOps image publication pipelines for every coordinated
   first-party image using the tag's full source commit SHA and release tag
   without the `v` prefix. For TauGrid 0.4.3, the required images are `tau`,
   `taugrid-portal`, `tau-core-controller`, and
   `taugrid-metrics-collector`.
3. Verify the expected immutable image tags and coordinated chart versions are
   available from MCR. Do not dispatch the GitHub Release while any ADO artifact
   is missing or still publishing. This ordering is an operator gate: the
   GitHub workflow has no credentials for, and does not infer readiness from,
   the external ADO pipelines.
4. Dispatch **Release TauGrid** from the same tag ref and supply the matching
   tag. Use the same command to retry a matching draft:

   ```bash
   gh workflow run release-tau.yaml \
     --repo Azure/taugrid \
     --ref vX.Y.Z \
     -f tag=vX.Y.Z
   ```
5. The read-only validation job verifies that the tag is annotated, follows
   SemVer, points to `main`, has checked-in release notes, and has no published
   GitHub Release.
6. The workflow reruns all release gates, compares two independent builds, and
   transfers the validated assets to a separate write-authorized publish job.
7. The publish job creates a draft release, uploads the assets once, verifies
   every GitHub asset digest, and only then publishes the draft. A retry resumes
   a draft only when its metadata, release notes, and assets exactly match the
   rebuilt release. It never overwrites an existing release or asset.
8. Before publication, a clean GitHub-hosted Windows runner verifies the two
   Windows executables and the `tau` release version. After publication,
   Ubuntu and macOS runners install the Python SDK wheel and exercise native
   CLI commands, while a Windows runner installs `tau` through `install.ps1`
   and exercises both Windows executables. A failure leaves the immutable
   published release unchanged when it occurs before publication.

Every tag must contain its own release notes, complete the external ADO artifact
gate, and use a manual run from the matching tag ref.

## Verify

Confirm the post-publication Ubuntu and macOS jobs succeeded. They exercise the
published CLI installer, Python SDK wheel, and native version and help commands.
The earlier release jobs compare every GitHub asset digest with `SHA256SUMS`,
compare two SDK wheel builds, and inspect all cross-compiled binaries with
`go version -m`. Do not update downstream minimum-version requirements until
the full workflow succeeds.
