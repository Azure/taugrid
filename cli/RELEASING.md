# Releasing Tau

Tau releases use canonical SemVer tags (`vX.Y.Z`). Pushing a tag automatically
starts the GitHub Actions release workflow; maintainers can also dispatch the
workflow manually with an existing tag.

## Distribution contract

Each release contains raw binaries and one checksum manifest. Linux binaries are
statically linked. Darwin binaries use CGO for macOS Keychain access and link
only to macOS system libraries and frameworks. Both Darwin architectures target
macOS 12.0 or newer.

- `tau-{darwin,linux}-{amd64,arm64}`
- `tau-gen-{darwin,linux}-{amd64,arm64}`
- `install.sh`
- `LICENSE`
- `SHA256SUMS`

The Tau Python SDK is tested from the same tagged source revision for CLI
compatibility, but it keeps its own package version. This workflow does not
publish Python wheels.

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
   cd cli
   VERSION=vX.Y.Z
   COMMIT="$(git rev-parse HEAD)"
   DATE="$(git show -s --format=%cI HEAD)"
   make release-assets VERSION="$VERSION" COMMIT="$COMMIT" DATE="$DATE" \
     RELEASE_DIR=/tmp/tau-release-a
   make release-assets VERSION="$VERSION" COMMIT="$COMMIT" DATE="$DATE" \
     RELEASE_DIR=/tmp/tau-release-b
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
   release tags; the workflow revalidates the remote tag before publication.
2. The tag push starts **Release tau CLI** automatically. To retry an existing
   tag, dispatch the workflow from `main` and supply the tag.
3. The read-only validation job verifies that the tag is annotated, follows
   SemVer, points to `main`, has checked-in release notes, and has no existing
   GitHub Release.
4. The workflow reruns all release gates, compares two independent builds, and
   transfers the validated assets to a separate write-authorized publish job.
5. The publish job creates a draft release, uploads the assets once, verifies
   every GitHub asset digest, and only then publishes the draft. It never
   overwrites an existing release or asset.
6. After publication, clean GitHub-hosted Ubuntu and macOS runners install the
   tagged Python SDK for compatibility, install `tau` through the published
   `install.sh`, download `tau-gen` from the same release, and exercise native
   help and version commands. A failure leaves the immutable published release
   unchanged and fails the workflow for explicit follow-up.

For an existing tag that predates its checked-in release notes, a maintainer can
explicitly allow the manual workflow to use the reviewed notes from `main`:

```bash
gh workflow run release-tau.yaml \
  --repo Azure/taugrid \
  --ref main \
  -f tag=vX.Y.Z \
  -f allow_main_release_notes=true
```

This is a recovery path. New tags must contain their own release notes.

## Verify

Confirm the post-publication Ubuntu and macOS jobs succeeded. They exercise the
published GitHub Release download and Python SDK compatibility paths plus native
version and help commands. The earlier release jobs compare every GitHub asset
digest with `SHA256SUMS` and inspect all cross-compiled binaries with
`go version -m`. Do not update downstream minimum-version requirements until
the full workflow succeeds.
