# Releasing Tau

Tau releases use canonical SemVer tags (`vX.Y.Z`). Pushing a tag does not
publish a release. A maintainer manually dispatches one GitHub Actions workflow
from the existing tag ref. After approval, that workflow publishes the
coordinated images and charts through the existing protected publishers,
verifies their public contents, and publishes the CLI and SDK GitHub Release
last. Every executable checkout uses the exact tagged revision.

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

1. Choose a reviewed source commit on `main` and record its full SHA. No
   release-specific version edit or release-preparation PR is required.
2. Optionally include `releases/vX.Y.Z.md` in the tagged commit for curated
   opening notes. Without it, the workflow generates the release notes.
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

## Protected publication configuration

The `release-publication` environment must have required reviewers and a single
deployment policy allowing `v*` tags. Its short-lived workload identity must
have queue and read access only to the approved publication pipelines. Registry
credentials remain in those publishers and are never stored in GitHub.

The environment provides these secrets:

- `RELEASE_TENANT_ID`
- `RELEASE_CLIENT_ID`
- `RELEASE_OIDC_AUDIENCE`
- `RELEASE_PUBLISHERS_CONFIG`

`RELEASE_PUBLISHERS_CONFIG` contains the protected API URL and exactly five
publisher entries: the four coordinated images and the chart publisher. Image
entries include a pipeline ID plus an immutable control commit and branch ref.
The chart entry includes its pipeline ID. Provision and update this
configuration through the approved infrastructure owner; do not add personal
tokens, client secrets, or registry credentials.

## Publish after authorization

The annotated tag is the sole coordinated release version source. The checked-in
charts retain release-neutral metadata, first-party image defaults follow the
packaged chart `appVersion`, and release binaries derive their default chart
version from their embedded build version. No version-bump commit or release PR
is required. A non-empty `cli/releases/vX.Y.Z.md` may provide curated opening
notes; when absent, the workflow uses GitHub-generated notes.

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
3. The read-only validation job verifies that the dispatch ref and input identify
   the same annotated SemVer tag, that its commit is on `main`, and that no
   published GitHub Release exists.
4. The release gates and reproducible CLI and SDK builds run before publication.
   The protected publication job then waits for environment approval and uses
   short-lived workload identity to queue every approved image and chart
   publisher with the exact source SHA and release version.
5. Before queueing, the workflow independently verifies any existing public
   artifact. Exact artifacts are reused, an active matching run is resumed, and
   a terminal failed run may be replaced. A conflicting run, mismatched public
   artifact, unknown state, ambiguous queue result, or unverified publisher
   revision fails closed.
6. All publisher runs are queued before the workflow waits for terminal results.
   The workflow then verifies both required image architectures, source revision
   labels, coordinated chart contents, dependency pins, and public digests.
7. The workflow appends GitHub-generated change attribution, new-contributor
   recognition, and the full changelog link to the checked-in curated notes.
   The separate write-authorized publish job runs only after every coordinated
   public artifact has been verified.
8. The publish job creates a draft release, uploads the assets once, verifies
   every GitHub asset digest, and only then publishes the draft. A retry resumes
   a draft only when its metadata, release notes, and assets exactly match the
   rebuilt release. It never overwrites an existing release or asset.
9. Before publication, a clean GitHub-hosted Windows runner verifies the two
   Windows executables and the `tau` release version. After publication,
   Ubuntu and macOS runners install the Python SDK wheel and exercise native
   CLI commands, while a Windows runner installs `tau` through `install.ps1`
   and exercises both Windows executables. A failure leaves the immutable
   published release unchanged when it occurs before publication.

Every release uses one manual run from the matching tag ref.

## Verify

Confirm the coordinated artifact job, GitHub publication, and
post-publication smoke jobs succeeded. The smoke jobs exercise the published
CLI installer, Python SDK wheel, and native version and help commands. Earlier
jobs compare public image and chart contents, every GitHub asset digest with
`SHA256SUMS`, two SDK wheel builds, and all cross-compiled binaries with
`go version -m`. Do not update downstream minimum-version requirements until
the full workflow succeeds.
