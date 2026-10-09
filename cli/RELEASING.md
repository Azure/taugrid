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
- `<tau-or-tau-gen-binary>.spdx.json` for every binary above
- `<component>-image-<version>-linux-<arch>-sha256-<digest>.spdx.json` for the
  Linux amd64 and arm64 manifests of `tau`, `taugrid-portal`, and
  `tau-core-controller`
- `taugrid-release-sbom-index.json`
- `SHA256SUMS`

The Tau Python SDK keeps its own package version. The workflow builds its wheel
twice from the tagged source revision, compares the outputs, and publishes the
wheel with the CLI assets. SBOMs are generated only after the two binary builds
compare successfully, so volatile scanner metadata cannot affect the binary
reproducibility check. `SHA256SUMS` covers the SBOMs and release index as well
as the other release assets.

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
2. Run the public Azure DevOps image publication pipelines for the coordinated
   first-party release using the tag's full source commit SHA and release tag
   without the `v` prefix. The GitHub SBOM scope is exactly `tau`,
   `taugrid-portal`, and `tau-core-controller`; independently versioned
   monitoring images are outside this release index.
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
6. The workflow appends GitHub-generated change attribution, new-contributor
   recognition, and the full changelog link to the checked-in curated notes.
   It then reruns all release gates, compares two independent builds, and
   transfers the validated binaries to the SBOM job. That job reads chart and
   values metadata, waits for each required public MCR tag, resolves it to an
   immutable OCI image-index digest, resolves the Linux amd64 and arm64
   platform manifest digests, and runs the pinned Syft version separately
   against each exact platform image. It also generates an SPDX JSON SBOM for
   every release `tau` and `tau-gen` binary, builds and validates the release
   index, and regenerates `SHA256SUMS`.
7. The publish job creates a draft release, uploads the assets once, verifies
   every GitHub asset digest and the SBOM/index contract, and only then
   publishes the draft. Missing public images, tag-only image identities,
   malformed SPDX, missing or duplicate index entries, missing files, and
   checksum mismatches fail before publication. A retry resumes a draft only
   when its metadata, release notes, and assets exactly match the rebuilt
   release. It never overwrites an existing release or asset.
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
`go version -m`. They also validate that the SBOM index contains every
distributed CLI target and both Linux platforms for exactly the three
coordinated image components, and that each referenced SPDX document and
checksum is valid. Image SBOM generation requires anonymous visibility of the
published MCR tags; local tests use a controlled fake Syft and do not require
registry credentials. Do not update downstream minimum-version requirements
until the full workflow succeeds.
