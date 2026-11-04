#!/usr/bin/env python3
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

import argparse
import io
import json
import os
import re
import subprocess
import time
import urllib.error
import urllib.parse
import urllib.request
import zipfile
from pathlib import Path

from release_artifacts import SHA, require, source_charts, verify_receipt, version


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, request, response, code, message, headers, url):
        return None


def request_bytes(url, *, token=None, data=None, content_type="application/json"):
    require(urllib.parse.urlsplit(url).scheme == "https", "HTTPS is required")
    headers = {"Accept": "application/json"}
    if token:
        headers["Authorization"] = f"Bearer {token}"
    if data is not None:
        headers["Content-Type"] = content_type
    request = urllib.request.Request(url, headers=headers, data=data)
    try:
        with urllib.request.build_opener(NoRedirect).open(request, timeout=60) as response:
            require(response.status == 200, "publisher API returned unexpected status")
            body = response.read(4 * 1024 * 1024 + 1)
            require(len(body) <= 4 * 1024 * 1024, "publisher response exceeds size limit")
            return body
    except urllib.error.HTTPError as error:
        raise ValueError(
            f"publisher API failed (HTTP {error.code}); do not blindly retry an uncertain queue") from None
    except urllib.error.URLError:
        raise ValueError("publisher API read or queue failed; do not blindly retry an uncertain queue") from None


def required(name):
    value = os.environ.get(name, "")
    require(bool(value), f"missing protected release configuration: {name}")
    return value


def mask(value):
    if os.environ.get("GITHUB_ACTIONS") == "true":
        escaped = str(value).replace("%", "%25").replace("\r", "%0D").replace("\n", "%0A")
        print(f"::add-mask::{escaped}", flush=True)


class Backend:
    def __init__(self, source_commit, tag, orchestration_commit):
        for commit in (source_commit, orchestration_commit):
            require(SHA.fullmatch(commit) is not None, "release source must be a full immutable SHA")
        self.source_commit = source_commit
        self.tag = tag
        self.orchestration_commit = orchestration_commit
        self.release_version = version(tag)
        self.base = required("RELEASE_BACKEND_URL").rstrip("/")
        parsed = urllib.parse.urlsplit(self.base)
        require(parsed.scheme == "https" and parsed.netloc == "dev.azure.com" and
                re.fullmatch(r"/[A-Za-z0-9_-]+/[A-Za-z0-9_%.-]+", parsed.path) and
                not parsed.query and not parsed.fragment, "invalid protected backend URL")
        self.pipeline_id = int(required("RELEASE_BACKEND_PIPELINE_ID"))
        require(self.pipeline_id > 0, "invalid publisher pipeline ID")
        self.control_commit = required("RELEASE_BACKEND_CONTROL_SHA")
        require(SHA.fullmatch(self.control_commit) is not None, "publisher control SHA must be immutable")
        self.control_ref = required("RELEASE_BACKEND_CONTROL_REF")
        require(re.fullmatch(r"refs/heads/[A-Za-z0-9/_.-]+", self.control_ref) is not None,
                "publisher control ref must be a branch ref")
        self.tenant = required("RELEASE_TENANT_ID")
        self.client = required("RELEASE_CLIENT_ID")
        for identifier in (self.tenant, self.client):
            require(re.fullmatch(r"[0-9a-fA-F-]{36}", identifier) is not None,
                    "invalid federated identity configuration")
        self.audience = required("RELEASE_OIDC_AUDIENCE")
        self.oidc_url = required("ACTIONS_ID_TOKEN_REQUEST_URL")
        self.oidc_token = required("ACTIONS_ID_TOKEN_REQUEST_TOKEN")
        require((urllib.parse.urlsplit(self.oidc_url).hostname or "").endswith(".actions.githubusercontent.com"),
                "unexpected OIDC issuer endpoint")
        self.token = None
        self.expires = 0

    def access_token(self):
        if time.monotonic() >= self.expires:
            separator = "&" if "?" in self.oidc_url else "?"
            assertion = json.loads(request_bytes(
                self.oidc_url + separator + urllib.parse.urlencode({"audience": self.audience}),
                token=self.oidc_token))["value"]
            require(isinstance(assertion, str) and assertion, "OIDC assertion is missing")
            mask(assertion)
            form = urllib.parse.urlencode({
                "client_id": self.client,
                "scope": "https://app.vssps.visualstudio.com/.default",
                "client_assertion_type": "urn:ietf:params:oauth:client-assertion-type:jwt-bearer",
                "client_assertion": assertion,
                "grant_type": "client_credentials",
            }).encode()
            response = json.loads(request_bytes(
                f"https://login.microsoftonline.com/{self.tenant}/oauth2/v2.0/token",
                data=form, content_type="application/x-www-form-urlencoded"))
            require(response.get("token_type", "").lower() == "bearer" and
                    isinstance(response.get("access_token"), str) and response["access_token"],
                    "federated exchange did not return a bearer token")
            lifetime = int(response["expires_in"])
            require(lifetime > 120, "federated token lifetime is too short")
            self.token = response["access_token"]
            mask(self.token)
            self.expires = time.monotonic() + lifetime - 120
        return self.token

    def api(self, path, body=None):
        data = json.dumps(body).encode() if body is not None else None
        return json.loads(request_bytes(f"{self.base}/_apis/{path}",
                                       token=self.access_token(), data=data))

    def parameters(self):
        return {"sourceRef": self.source_commit, "releaseTag": self.release_version,
                "forcePublish": False, "orchestrationRef": self.orchestration_commit}

    def validate_run(self, run, run_id):
        require(type(run.get("id")) is int and run["id"] == run_id and
                run.get("pipeline", {}).get("id") == self.pipeline_id,
                "publisher run identity mismatch")
        repositories = run.get("resources", {}).get("repositories", {})
        for name, commit in (("self", self.control_commit), ("taugrid", self.source_commit),
                             ("releaseTools", self.orchestration_commit)):
            require(repositories.get(name, {}).get("version") == commit,
                    f"publisher {name} revision mismatch")
        require(run.get("templateParameters") == self.parameters(), "publisher parameter mismatch")
        require(run.get("state") in ("inProgress", "completed"), "publisher state is unknown or canceled")

    def run(self, resume_id=None, timeout=10800):
        require(resume_id is None or (type(resume_id) is int and resume_id > 0),
                "resume run identity must be a positive integer")
        if resume_id is None:
            body = {"resources": {"repositories": {
                "self": {"refName": self.control_ref, "version": self.control_commit},
                "taugrid": {"refName": f"refs/tags/{self.tag}", "version": self.source_commit},
                "releaseTools": {"refName": "refs/heads/main", "version": self.orchestration_commit},
            }}, "templateParameters": self.parameters()}
            run = self.api(f"pipelines/{self.pipeline_id}/runs?api-version=7.1", body)
            run_id = run.get("id")
            require(type(run_id) is int and run_id > 0, "publisher did not return a run ID")
        else:
            run_id = resume_id
        mask(run_id)
        deadline = time.monotonic() + timeout
        while True:
            run = self.api(f"pipelines/{self.pipeline_id}/runs/{run_id}?api-version=7.1")
            self.validate_run(run, run_id)
            if run["state"] == "completed":
                require(run.get("result") == "succeeded", "approved publisher run did not succeed")
                return run_id
            require(time.monotonic() < deadline, "publisher wait timed out; inspect protected backend before retry")
            time.sleep(30)

    def receipt(self, run_id):
        artifact = self.api(
            f"pipelines/{self.pipeline_id}/runs/{run_id}/artifacts"
            "?artifactName=release-receipt&$expand=signedContent&api-version=7.1")
        require(artifact.get("name") == "release-receipt", "publisher receipt artifact missing")
        url = artifact.get("signedContent", {}).get("url", "")
        require(bool(url), "publisher receipt download URL missing")
        mask(url)
        archive = request_bytes(url)
        with zipfile.ZipFile(io.BytesIO(archive)) as files:
            members = [entry for entry in files.infolist() if not entry.is_dir()]
            require(len(members) == 1 and members[0].filename in
                    ("release-receipt.json", "release-receipt/release-receipt.json") and
                    members[0].file_size <= 1024 * 1024, "invalid publisher receipt archive")
            return json.loads(files.read(members[0]))


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--source", type=Path, required=True)
    parser.add_argument("--source-commit", required=True)
    parser.add_argument("--orchestration-commit", required=True)
    parser.add_argument("--tag", required=True)
    parser.add_argument("--resume-run", type=int)
    parser.add_argument("--output", type=Path, required=True)
    args = parser.parse_args()
    require(subprocess.check_output(
        ["git", "-C", str(args.source), "rev-parse", "HEAD"], text=True).strip() ==
        args.source_commit, "payload checkout does not match immutable release source")
    require(not subprocess.check_output(
        ["git", "-C", str(args.source), "status", "--porcelain", "--untracked-files=no"],
        text=True).strip(), "payload checkout has modified tracked content")
    source_charts(args.source, args.tag)
    backend = Backend(args.source_commit, args.tag, args.orchestration_commit)
    run_id = backend.run(args.resume_run)
    evidence = verify_receipt(backend.receipt(run_id), args.source.resolve(), args.tag,
                              args.source_commit, backend.control_commit, backend.pipeline_id, run_id)
    args.output.write_text(json.dumps(evidence, indent=2) + "\n")


if __name__ == "__main__":
    main()
