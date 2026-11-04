# Releasing Tau

Tau releases use canonical SemVer tags (`vX.Y.Z`). Pushing a tag does not
publish a GitHub Release. Maintainers dispatch the reviewed release workflow
from `main`, supplying an existing annotated product tag. The workflow actively
requests every coordinated image and chart build through the approved publisher,
independently verifies the public artifacts, and publishes the GitHub Release last.
SemVer build metadata (`+...`) is unsupported because container tags cannot
represent it without changing the product version.

The workflow revision, publisher control revision, and product source revision
are separate immutable commits. Recovery of an older unpublished product tag
uses current reviewed orchestration without moving or replacing that tag.

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

## Protected publisher setup

**Platform provisioning is required before this workflow can publish.** The
repository does not provision an identity bridge, register a coordinator, or
install protected-resource checks. Missing configuration fails closed; no
personal token, stored client secret, or existing webhook secret is a fallback.

Configure the `release-publication` environment with required reviewers and a
deployment branch policy restricted to the single `main` branch (not tags).
The job reads installed protection rules and fails closed if either required
reviewers or that exact branch policy is missing. Populate these environment
secrets through the approved platform setup. These are configuration bindings,
not client credentials; using environment secrets masks private endpoints and
identifiers in runner logs:

| Environment secret | Required binding |
|---|---|
| `RELEASE_BACKEND_URL` | Approved project API base URL |
| `RELEASE_BACKEND_PIPELINE_ID` | Registered single coordinated publisher |
| `RELEASE_BACKEND_CONTROL_REF` | Reviewed publisher control branch ref |
| `RELEASE_BACKEND_CONTROL_SHA` | Full immutable publisher control commit |
| `RELEASE_TENANT_ID` | Approved federated identity tenant |
| `RELEASE_CLIENT_ID` | Approved application identity |
| `RELEASE_OIDC_AUDIENCE` | Audience matching the configured federated trust |

Bind workload federation to this repository's protected environment subject and
the expected issuer and audience. The application identity needs only permission
to queue/read the approved coordinator and read its receipt artifact, plus
necessary repository access. It does not need registry publishing credentials.
The workflow exchanges its short-lived assertion for the documented
`https://app.vssps.visualstudio.com/.default` resource token, keeps both tokens
only in memory, and never forwards them to artifact download URLs.
See the [federated credential token protocol](https://learn.microsoft.com/entra/identity-platform/v2-oauth2-client-creds-grant-flow)
and [application identity authorization](https://learn.microsoft.com/azure/devops/integrate/get-started/authentication/service-principal-managed-identity).

The approved coordinator must enforce a shared protected exclusive lock across
coordinated and manual publication entry points, explicitly verify the installed
lock check, preserve approvals, reject unknown reads and overwrites, and reuse
existing artifacts only when source, platforms, and content match. Merely setting
YAML lock behavior is not proof that the protected check exists. Register and
review that backend contract before configuring its immutable control SHA.

The queue request contains `sourceRef`, `releaseTag` (without `v`),
`forcePublish=false`, and `orchestrationRef`. Repository resources must resolve
`self` to the configured publisher control SHA, `taugrid` to the product tag's
full commit, and `releaseTools` to the GitHub workflow's full commit. The workflow
checks all three returned revisions and every parameter; pipeline success alone
is insufficient.

The coordinator builds these four images for **both `linux/amd64` and
`linux/arm64`**: `tau`, `taugrid-portal`, `tau-core-controller`, and
`taugrid-metrics-collector`. It verifies their public availability, then builds
`tau-core-controller`, `taugrid-core`, and `taugrid` charts. All use the product
commit and coordinated version. The separately versioned GPU collector, Ray,
ADX, Kueue, KubeRay, and GPU monitoring artifacts are verified at their checked-in
pins, not rebuilt or silently upgraded.

Trusted chart packaging is:

```bash
python3 -m pip install -r scripts/ci/release-requirements.txt
python3 scripts/ci/release_artifacts.py package-charts \
  --source /path/to/immutable-product-checkout \
  --tag vX.Y.Z --output /path/to/empty-packages
```

Run this script from the trusted orchestration checkout, not the payload.
It packages only the three coordinated charts; fetches pinned upstream chart
artifacts from the public registry; applies the established Kueue CRD-retention
transformation; and normalizes archive timestamps, ownership, modes, and ordering
for byte-identical retries. It does not call payload-owned publishing scripts.
Python dependencies and Helm must be installed before the protected publisher
step; the GitHub verifier pins Helm 3.20.0.

The coordinator must emit a pipeline artifact named `release-receipt` containing
only `release-receipt.json`. Its schema is:

```json
{
  "schema": 1,
  "sourceCommit": "<full-product-sha>",
  "releaseTag": "X.Y.Z",
  "controlCommit": "<full-publisher-control-sha>",
  "pipelineId": 1,
  "runId": 1,
  "images": [
    {
      "name": "<required-image-name>",
      "reference": "<public-image-repository>:X.Y.Z",
      "digest": "sha256:<index-digest>",
      "platforms": ["linux/amd64", "linux/arm64"],
      "revision": "<full-product-sha>"
    }
  ],
  "charts": [
    {
      "name": "<required-chart-name>",
      "reference": "<public-chart-repository>:X.Y.Z",
      "digest": "sha256:<manifest-digest>",
      "contentSha256": "<64-lowercase-hex-chart-layer-hash>"
    }
  ]
}
```

The image and chart arrays must contain the exact required matrices, without
duplicates or omissions. GitHub independently checks public manifest/config
digests, both image platforms and OCI revision labels, chart layer hashes and
complete chart contents against trusted packaging of the product source, pinned
upstream chart contents, and independently pinned public runtime images. Unknown
or unavailable public artifacts stop publication. Public evidence excludes private
publisher identities; the raw receipt is not uploaded to GitHub.

## Publish after authorization

1. Create an annotated `vX.Y.Z` tag on the reviewed `main` source commit and
   push only that tag. Repository tag rules must prevent updates or deletion of
   release tags. Pushing the tag does not start **Release TauGrid**.
2. Verify protected publisher setup is complete, including the reviewed
   coordinator control SHA and cross-entrypoint lock checks.
3. Dispatch **Release TauGrid** from reviewed `main` and supply the product tag:

   ```bash
   gh workflow run release-tau.yaml \
     --repo Azure/taugrid \
     --ref main \
     -f tag=vX.Y.Z
   ```
4. The read-only validation job verifies that the tag is annotated, follows
   SemVer, points to `main`, has checked-in release notes, and has no published
   GitHub Release.
5. After release gates pass, the protected coordinator builds and verifies
   images first, then coordinated charts, and emits its immutable receipt.
   GitHub checks the run identity, receipt, and public artifacts independently.
   Missing setup, failed builds, wrong source, unknown status, or failed public
   checks block the GitHub Release.
6. The workflow appends GitHub-generated change attribution, new-contributor
   recognition, and the full changelog link to the checked-in curated notes.
   It then reruns all release gates, compares two independent builds, and
   transfers the validated assets to a separate write-authorized publish job.
7. The publish job creates a draft release, uploads the assets once, verifies
   every GitHub asset digest, and only then publishes the draft. A retry resumes
   a draft only when its metadata, release notes, and assets exactly match the
   rebuilt release. It never overwrites an existing release or asset.
   To resume an already queued matching coordinator run, configure its identifier
   in the optional environment secret `RELEASE_BACKEND_RESUME_RUN` and add
   `-f resume_publisher=true` to the dispatch. Never put private run identifiers
   into public workflow inputs. Clear the resume binding after recovery.
   The workflow must still resolve to the same reviewed orchestration commit. A failed or uncertain queue
   is never automatically repeated; inspect the protected backend first.
8. Before publication, a clean GitHub-hosted Windows runner verifies the two
   Windows executables and the `tau` release version. After publication,
   Ubuntu and macOS runners install the Python SDK wheel and exercise native
   CLI commands, while a Windows runner installs `tau` through `install.ps1`
   and exercises both Windows executables. A failure leaves the immutable
   published release unchanged when it occurs before publication.

Every tag must contain its own release notes. The GitHub Release cannot publish
until all required builds and independent public-artifact checks succeed.

## Verify

Confirm the post-publication Ubuntu and macOS jobs succeeded. They exercise the
published CLI installer, Python SDK wheel, and native version and help commands.
The earlier release jobs compare every GitHub asset digest with `SHA256SUMS`,
compare two SDK wheel builds, and inspect all cross-compiled binaries with
`go version -m`. Do not update downstream minimum-version requirements until
the full workflow succeeds.
