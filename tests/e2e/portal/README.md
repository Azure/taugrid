# Portal Historical Range Acceptance

These additive tests reuse the existing Go E2E module and its CI entry point.
Existing tests, assertions, and live-cluster opt-in rules are unchanged.

```bash
cd tests/e2e
AI_RUNTIME_E2E=0 go test -count=1 -v ./portal
AI_RUNTIME_E2E=0 go test -count=1 ./...
```

Requirements: the repository Go toolchain, the Portal module's dependencies,
and Python 3 with its standard-library SQLite support. No new test framework,
package installation, cloud credentials, or cluster is required. Missing
prerequisites fail the tests instead of silently skipping acceptance.

## Test Strategy

The tests build the current Portal source, create temporary stores via the
public CLI, import JSONL through the actual importer, and start the resulting
Portal process on a loopback-only ephemeral port. Assertions use real HTTP
responses and explicitly specified expected run IDs and statuses. The process
serves its embedded frontend assets; this is API E2E, not browser E2E.

The fixed-time fixture adjusts only timestamps in each temporary SQLite store,
before the server starts. Runs and metric files are created by the real importer.
The fixture asserts that no lifecycle events were generated. The metric-only
test then imports a new file while the server is running. It checks both custom
and relative ranges without rewriting the original run creation timestamp.
Temporary data and subprocesses are cleaned up using Go test cleanup callbacks.
An invalid explicit kubeconfig and a filtered process environment prevent use
of ambient cluster and Azure configuration.

## Behavior Matrix

| Contract | Independent expectation | Current evidence |
| --- | --- | --- |
| Local import | One succeeded run; retrying the same import does not duplicate it | `TestPortalHistoricalImport` |
| Workspace isolation | Another workspace is rejected with 403; its records are absent from the selected workspace | Import and boundary tests |
| Range before limit | Out-of-range newer rows cannot displace the in-range endpoint | Boundary test, limits 1, 2, 3 |
| Expanding page size | Ordered IDs are `end`, `middle`, `start`; truncation clears at exactly 3 | Boundary test; not cursor pagination |
| Whole-second endpoints | Both start and end records included; adjacent outside records excluded | Boundary test |
| Timezone equivalence | UTC and equivalent +02:00 bounds return identical results | Boundary test |
| Terminal lifecycle | Middle record remains failed; succeeded filter retains only endpoints | Boundary test; not snapshot liveness |
| Subsecond endpoints | .100 through .900 includes start/middle/end but excludes .099 and .901 | PASS after timestamp comparison fix; original assertion unchanged |
| Metric-only activity | Old active run enters the current range after a new import, with its creation time unchanged | `TestPortalHistoricalMetricOnlyActivity` |
| Error recovery | Invalid input returns 400; subsequent valid and empty searches remain correct | Import and boundary tests |
| Frontend range/lifecycle/retry behavior | Existing component assertions remain unchanged; search-only rows retain authoritative liveness | 52 frontend tests; added CI execution |
| Browser state and rendering | Independent rail/header/status totals across range switches | Live Playwright PASS at 1440x1000 and 390x844; two populated metric charts |
| Kusto remote-write | Actual import, collector ingestion, native ADX queries, lifecycle, range and subsecond membership | `TestPortalLiveADXBrowser` PASS |
| Kusto projection/auto | Same contract on the appropriate real data source | Not exercised by the remote-write live deployment |
| Cost buckets | Independent hourly sums; aligned end excluded; partial hours rejected | Real ADX/API PASS; cost-specific UI interaction not covered |
| Late import of old samples | Registration time, not sample wall time, defines local import activity | Run membership and experiment-discovery regressions |

## Regression Fixed on 2026-09-21

```bash
cd tests/e2e
AI_RUNTIME_E2E=0 go test -count=1 -v ./portal \
  -run 'TestPortalHistoricalBoundaries/positive_subsecond_interval'
```

For start `2026-09-17T10:00:00.100Z` and end
`2026-09-17T10:00:00.900Z`, the independently specified expected IDs are:

```text
sub-end, sub-middle, sub-start
```

The original result also contained `sub-after`, timestamp
`2026-09-17T10:00:00.901Z`. Local search compares RFC3339 timestamps as SQLite
TEXT. After API normalization, `.901Z` sorts before `.9Z`, although it denotes
a later instant. Timestamp text ordering also affects result ordering.

The regression was neither skipped nor weakened. Local run search now uses the
SQLite `tau_timestamp` collation for time predicates and ordering, parsing
RFC3339Nano instants instead of comparing variable-width text or reducing
precision to SQLite milliseconds. Valid times sort after invalid legacy values;
invalid values retain deterministic lexical ordering. No stored data is rewritten.
All original Portal offline E2E assertions now pass.

All four new first-party source files include the repository copyright and MIT
headers; the repository license-header check passes.

## Live ADX and Browser Acceptance

This optional test requires both `AI_RUNTIME_E2E=1` and `TAU_PORTAL_LIVE_E2E=1`.
The generic cluster gate alone skips it. Once both are enabled, missing
configuration fails rather than skipping. It builds the current Go
source with its embedded frontend and never proxies to an older deployed Portal.
Rebuild the frontend before running; CI's existing asset freshness check remains.

```bash
npm --prefix portal/frontend run build
cd tests/e2e
AI_RUNTIME_E2E=1 \
TAU_PORTAL_LIVE_E2E=1 \
TAU_PORTAL_ADX_ENDPOINT=https://YOUR-CLUSTER.kusto.windows.net \
TAU_PORTAL_ADX_DATABASE=Metrics \
TAU_PORTAL_WORKSPACE=YOUR-WORKSPACE \
TAU_PORTAL_CLUSTER=YOUR-CLUSTER-NAME \
TAU_PORTAL_REMOTE_WRITE_URL=http://127.0.0.1:23100/receive \
TAU_PORTAL_PLAYWRIGHT_MODULE=/absolute/path/to/playwright-core/index.mjs \
TAU_PORTAL_CHROMIUM=/absolute/path/to/chromium \
TAU_PORTAL_EVIDENCE_DIR=/absolute/path/to/new-evidence-directory \
go test -count=1 -timeout 12m -run TestPortalLiveADXBrowser -v ./portal
```

Use an authorized test environment, an authenticated Azure CLI with ADX read
access, an existing collector endpoint (optionally a local port-forward), and
existing Playwright/Chromium installations. The test installs nothing. It writes
24 scalar samples plus a terminal status marker under a unique `pr297-e2e-*`
project. Local processes and temporary stores are removed; isolated telemetry
remains subject to the ADX retention policy. It does not delete table data,
change schema, provision infrastructure, or modify the deployed Portal.

Data visibility has a 240-second deadline. Expected day/week membership and
lifecycle counts come from the fixture, not snapshot-derived expectations.
Raw ADX queries independently confirm scalar counts and hourly costs. The live
cost check requires actual schema-v4 data, not an empty-table pass.

The 2026-09-21 final run passed in 141.88 seconds using Playwright 1.57.0 and
Chromium, with current frontend assets rebuilt before compilation. Evidence is
in `/tmp/taugrid-pr297-live-evidence-final`: fixture identity and binary SHA256,
import results, raw ADX samples, API snapshots, cost expectations/results,
desktop/mobile screenshots, Playwright `trace.zip`, and `checks.json`.
This verifies that earlier local build against real ADX, not a deployment rollout
or the later R8-R11 changes.

## R8-R11 Local Validation

The later remediation was validated offline: portable Go acceptance, native
query fakes for both ingestion formats, QueryCommand/file/in-memory tests, and
HTTP auto-source success/failure/local-precedence checks. Latest evidence is
queried only after historical membership and limits, using exact identities and
200-run batches. Empty successful evidence is unknown; failed/cancelled ADX
completion frames are errors. No real ADX scan-cost or retention claim follows
from the offline query contracts.

Frontend tests run in UTC, Asia/Shanghai, Asia/Kolkata and America/New_York;
the New York process executes the DST gap/fold case. Untouched custom bounds
retain exact text, while edited bounds use millisecond precision. Cost Custom
defaults to the previous complete UTC hour and rejects nonaligned drafts before
fetching; direct backend 400 responses remain recoverable.

Existing Playwright/Chromium also checked the rebuilt embedded assets against a
loopback fixture at 1440x1000/New York and 390x844/Kolkata. Failed Kusto authority,
retained pagination/hidden selection, timezone-only Apply with no extra request,
exact URL bounds, Cost defaults/validation, runtime errors and horizontal overflow
were checked. Temporary screenshots and checks are in
`/tmp/taugrid-r8-r11-browser`; the temporary harness is
`/tmp/taugrid-r8-r11-browser.mjs`. These are local evidence, not durable CI or
live-cloud acceptance. No packages, credentials or cloud changes were required.

## CI and Remaining Gates

The existing Portable end-to-end tests job discovers this package through
`go test -count=1 ./...`. The Portal job also runs `npm test --prefix frontend`,
in addition to its existing build and embedded-asset freshness checks.

The live test is opt-in and is not run against shared cloud infrastructure on
every PR. The offline job reports it as skipped, never as a live pass. Projection
ingestion, auto-source merging, browser failure injection, large-run pagination,
and cost-specific UI recovery were not covered by that earlier live run. Later
offline coverage above is not a substitute for live ADX performance validation.
No full-platform or GPU E2E pass is implied by the Portal live result.