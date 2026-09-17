#!/usr/bin/env python3
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

from pathlib import Path

ROOT = Path(__file__).resolve().parents[3]
WORKFLOW = ROOT / ".github/workflows/taugrid-pr-images.yml"
content = WORKFLOW.read_text()

required = [
    "pull_request:",
    "github.event.pull_request.head.repo.full_name != github.repository",
    "github.event.pull_request.head.repo.full_name == github.repository",
    "ref: ${{ github.event.pull_request.head.sha }}",
    "persist-credentials: false",
    "id-token: write",
    "secrets.AZURE_E2E_CLIENT_ID",
    "secrets.AZURE_E2E_TENANT_ID",
    "secrets.AZURE_E2E_SUBSCRIPTION_ID",
    "steps.azure-config.outputs.available == 'true'",
    "Skipped the ACR build because the repository Azure OIDC secrets are not configured.",
    "azure/login@a641126d1b8aa4d1fa005f4f92df94a3a4c4c906",
    '--namespace "pr-${PR_NUMBER}"',
    "--output \"$OUTPUT_FILE\"",
    "all",
    "validate-acr-build-output.py",
    "actions/upload-artifact@043fb46d1a93c77aae656e7c1c64a875d1fc6a0a",
    "GITHUB_STEP_SUMMARY",
]
for value in required:
    if value not in content:
        raise SystemExit(f"{WORKFLOW} is missing required contract: {value}")

for forbidden in ["pull_request_target:", "docker/build-push-action", "tr start"]:
    if forbidden in content:
        raise SystemExit(f"{WORKFLOW} contains forbidden contract: {forbidden}")

expected_paths = [
    ".gitignore",
    "cli/**",
    "controllers/tau-core/**",
    "core/**",
    "images/tau/**",
    "images/tau-core-controller/**",
    "images/taugrid-portal/**",
    "portal/**",
    "scripts/acr-build-images.sh",
]
for path in expected_paths:
    if f"- {path}" not in content:
        raise SystemExit(f"{WORKFLOW} path filter is missing {path}")

print("Validated TauGrid PR image workflow safety and build contract.")
