# Releasing Tau

Tau releases use canonical SemVer tags (`vX.Y.Z`) and a manually dispatched
GitHub Actions workflow. Pushing a tag does not publish a release.

## Distribution contract

Each release contains raw binaries and one checksum manifest. Linux binaries are
statically linked. Darwin binaries use CGO for macOS Keychain access and link
only to macOS system libraries and frameworks. Both Darwin architectures target
macOS 12.0 or newer.

- `tau-{darwin,linux}-{amd64,arm64}`
- `tau-gen-{darwin,linux}-{amd64,arm64}`
- `SHA256SUMS`

The Tau Python SDK is installed from the same tagged source revision. GitHub
Releases do not contain Python wheels.

## Prepare

1. Land a release-preparation PR that updates the SDK version, install docs, and
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
   push only that tag.
2. Manually dispatch **Release tau CLI** with the existing tag.
3. The read-only validation job verifies that the tag is annotated, follows
   SemVer, points to `main`, has checked-in release notes, and has no existing
   GitHub Release.
4. The workflow reruns all release gates, compares two independent builds, and
   transfers the validated assets to a separate write-authorized publish job.
5. The publish job creates a draft release, uploads the assets once, verifies
   every GitHub asset digest, and only then publishes the draft. It never
   overwrites an existing release or asset.
6. After publication, clean GitHub-hosted Ubuntu and macOS runners install the
   tagged Python SDK, bootstrap `tau` through the published GitHub Release,
   download `tau-gen` from the same release, and exercise native version and
   help commands. A failure leaves the immutable published release unchanged and
   fails the workflow for explicit follow-up.

## Verify

Confirm the post-publication Ubuntu and macOS jobs succeeded. They exercise the
published GitHub Release download and Python SDK bootstrap paths plus native
version and help commands. The earlier release jobs compare every GitHub asset
digest with `SHA256SUMS` and inspect all cross-compiled binaries with
`go version -m`. Do not update downstream minimum-version requirements until
the full workflow succeeds.
