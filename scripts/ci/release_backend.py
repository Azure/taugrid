#!/usr/bin/env python3
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

import argparse
import json
import os
import re
import subprocess
import time
import urllib.error
import urllib.parse
import urllib.request
from pathlib import Path

from release_artifacts import (
    ArtifactNotFound,
    CHARTS,
    IMAGES,
    SHA,
    collect_chart,
    collect_image,
    collect_release,
    require,
    source_charts,
    version,
)


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
        raise ValueError(f"publisher API failed (HTTP {error.code})") from None
    except urllib.error.URLError:
        raise ValueError("publisher API request failed") from None


def required(name):
    value = os.environ.get(name, "")
    require(bool(value), f"missing protected release configuration: {name}")
    return value


def mask(value):
    if os.environ.get("GITHUB_ACTIONS") == "true":
        escaped = str(value).replace("%", "%25").replace("\r", "%0D").replace("\n", "%0A")
        print(f"::add-mask::{escaped}", flush=True)


class Backend:
    def __init__(self, source_commit, tag):
        require(SHA.fullmatch(source_commit) is not None,
                "release source must be a full immutable SHA")
        self.source_commit = source_commit
        self.tag = tag
        self.release_version = version(tag)
        config = json.loads(required("RELEASE_PUBLISHERS_CONFIG"))
        require(isinstance(config, dict), "invalid protected publisher configuration")
        self.base = config.get("url", "").rstrip("/")
        parsed = urllib.parse.urlsplit(self.base)
        require(parsed.scheme == "https" and parsed.netloc == "dev.azure.com" and
                re.fullmatch(r"/[A-Za-z0-9_-]+/[A-Za-z0-9_%.-]+", parsed.path) and
                not parsed.query and not parsed.fragment, "invalid protected publisher URL")
        publishers = config.get("publishers")
        require(isinstance(publishers, list), "protected publisher matrix is missing")
        self.publishers = {}
        for publisher in publishers:
            require(isinstance(publisher, dict), "invalid protected publisher entry")
            name = publisher.get("name")
            kind = publisher.get("kind")
            pipeline_id = publisher.get("pipelineId")
            require(isinstance(name, str) and name not in self.publishers,
                    "duplicate or invalid protected publisher name")
            require(kind in ("image", "charts"), "invalid protected publisher kind")
            require(type(pipeline_id) is int and pipeline_id > 0,
                    "invalid protected publisher pipeline ID")
            if kind == "image":
                require(name in IMAGES, "unsupported coordinated image publisher")
                control_commit = publisher.get("controlCommit", "")
                control_ref = publisher.get("controlRef", "")
                require(SHA.fullmatch(control_commit) is not None,
                        "publisher control SHA must be immutable")
                require(re.fullmatch(r"refs/heads/[A-Za-z0-9/_.-]+", control_ref) is not None,
                        "publisher control ref must be a branch ref")
            else:
                require(name == "charts", "unsupported coordinated chart publisher")
            self.publishers[name] = publisher
        require(set(self.publishers) == {*IMAGES, "charts"},
                "protected publisher matrix must contain every coordinated publisher")
        self.tenant = required("RELEASE_TENANT_ID")
        self.client = required("RELEASE_CLIENT_ID")
        for identifier in (self.tenant, self.client):
            require(re.fullmatch(r"[0-9a-fA-F-]{36}", identifier) is not None,
                    "invalid federated identity configuration")
        self.audience = required("RELEASE_OIDC_AUDIENCE")
        self.oidc_url = required("ACTIONS_ID_TOKEN_REQUEST_URL")
        self.oidc_token = required("ACTIONS_ID_TOKEN_REQUEST_TOKEN")
        require((urllib.parse.urlsplit(self.oidc_url).hostname or "").endswith(
            ".actions.githubusercontent.com"), "unexpected OIDC issuer endpoint")
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
            try:
                lifetime = int(response.get("expires_in", 0))
            except (TypeError, ValueError):
                lifetime = 0
            require(lifetime > 120, "federated token lifetime is too short")
            self.token = response["access_token"]
            mask(self.token)
            self.expires = time.monotonic() + lifetime - 120
        return self.token

    def api(self, path, body=None):
        data = json.dumps(body).encode() if body is not None else None
        return json.loads(request_bytes(
            f"{self.base}/_apis/{path}", token=self.access_token(), data=data))

    def parameters(self, publisher):
        if publisher["kind"] == "charts":
            return {}
        return {
            "sourceRef": self.source_commit,
            "releaseTag": self.release_version,
            "forcePublish": False,
        }

    def resources(self, publisher):
        if publisher["kind"] == "charts":
            return {"self": {
                "refName": f"refs/tags/{self.tag}",
                "version": self.source_commit,
            }}
        return {
            "self": {
                "refName": publisher["controlRef"],
                "version": publisher["controlCommit"],
            },
            "taugrid": {
                "refName": self.source_commit,
                "version": self.source_commit,
            },
        }

    def queue_body(self, publisher):
        body = {"resources": {"repositories": self.resources(publisher)}}
        parameters = self.parameters(publisher)
        if parameters:
            body["templateParameters"] = parameters
        return body

    def validate_run(self, publisher, run, run_id):
        require(type(run.get("id")) is int and run["id"] == run_id and
                run.get("pipeline", {}).get("id") == publisher["pipelineId"],
                "publisher run identity mismatch")
        repositories = run.get("resources", {}).get("repositories", {})
        for name, resource in self.resources(publisher).items():
            actual = repositories.get(name, {})
            require(actual.get("version") == resource["version"],
                    f"publisher {name} revision mismatch")
            if "refName" in actual:
                require(actual["refName"] == resource["refName"],
                        f"publisher {name} ref mismatch")
        expected = self.parameters(publisher)
        actual = run.get("templateParameters") or {}
        require(actual == expected, "publisher parameter mismatch")
        require(run.get("state") in ("inProgress", "completed"),
                "publisher state is unknown or canceled")

    def release_run(self, publisher, run):
        if run.get("pipeline", {}).get("id") != publisher["pipelineId"]:
            return False
        if publisher["kind"] == "charts":
            repository = run.get("resources", {}).get("repositories", {}).get("self", {})
            if "refName" in repository:
                return repository["refName"] == f"refs/tags/{self.tag}"
            return repository.get("version") == self.source_commit
        return (run.get("templateParameters") or {}).get("releaseTag") == self.release_version

    def recent_runs(self, publisher):
        response = self.api(
            f"pipelines/{publisher['pipelineId']}/runs?api-version=7.1&$top=50")
        summaries = response.get("value")
        require(isinstance(summaries, list), "publisher run list is invalid")
        runs = []
        for summary in summaries:
            run_id = summary.get("id")
            require(type(run_id) is int and run_id > 0, "publisher run list has invalid identity")
            run = self.api(
                f"pipelines/{publisher['pipelineId']}/runs/{run_id}?api-version=7.1")
            if self.release_run(publisher, run):
                runs.append(run)
        return runs

    def exact_runs(self, publisher):
        exact = []
        for run in self.recent_runs(publisher):
            try:
                self.validate_run(publisher, run, run["id"])
                exact.append(run)
            except ValueError:
                raise ValueError(
                    f"{publisher['name']} has a conflicting run for release {self.tag}") from None
        return exact

    def artifact_ready(self, publisher):
        if publisher["kind"] == "image":
            try:
                collect_image(publisher["name"], self.source_commit, self.release_version)
                return True
            except ArtifactNotFound:
                return False
        missing = False
        for name in CHARTS:
            try:
                collect_chart(name, self.release_version)
            except ArtifactNotFound:
                missing = True
        return not missing

    def queue_or_resume(self, publisher):
        if self.artifact_ready(publisher):
            return None
        exact = self.exact_runs(publisher)
        active = [run for run in exact if run["state"] == "inProgress"]
        succeeded = [run for run in exact if run["state"] == "completed" and
                     run.get("result") == "succeeded"]
        require(len(active) <= 1, "multiple matching publisher runs are active")
        if active:
            run_id = active[0]["id"]
            mask(run_id)
            return run_id
        require(not succeeded, "publisher succeeded but the public artifact is unavailable")
        before = {run["id"] for run in exact}
        try:
            queued = self.api(
                f"pipelines/{publisher['pipelineId']}/runs?api-version=7.1",
                self.queue_body(publisher))
            run_id = queued.get("id")
            require(type(run_id) is int and run_id > 0, "publisher did not return a run ID")
        except ValueError:
            recovered = [run for run in self.exact_runs(publisher) if run["id"] not in before]
            require(len(recovered) == 1,
                    "publisher queue result is uncertain; inspect protected runs before retry")
            run_id = recovered[0]["id"]
        mask(run_id)
        return run_id

    def wait(self, pending, timeout=10800):
        deadline = time.monotonic() + timeout
        remaining = dict(pending)
        while remaining:
            for name, run_id in list(remaining.items()):
                publisher = self.publishers[name]
                run = self.api(
                    f"pipelines/{publisher['pipelineId']}/runs/{run_id}?api-version=7.1")
                self.validate_run(publisher, run, run_id)
                if run["state"] == "completed":
                    require(run.get("result") == "succeeded",
                            f"{name} publisher run did not succeed")
                    del remaining[name]
            if remaining:
                require(time.monotonic() < deadline,
                        "publisher wait timed out; inspect protected runs before retry")
                time.sleep(30)

    def wait_artifacts(self, timeout=1200):
        deadline = time.monotonic() + timeout
        remaining = dict(self.publishers)
        while remaining:
            for name, publisher in list(remaining.items()):
                if self.artifact_ready(publisher):
                    del remaining[name]
            if remaining:
                require(time.monotonic() < deadline,
                        "publisher succeeded but public artifact propagation timed out")
                time.sleep(15)

    def run(self):
        pending = {}
        for name, publisher in self.publishers.items():
            run_id = self.queue_or_resume(publisher)
            if run_id is not None:
                pending[name] = run_id
        self.wait(pending)
        self.wait_artifacts()


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--source", type=Path, required=True)
    parser.add_argument("--source-commit", required=True)
    parser.add_argument("--tag", required=True)
    parser.add_argument("--output", type=Path, required=True)
    args = parser.parse_args()
    require(subprocess.check_output(
        ["git", "-C", str(args.source), "rev-parse", "HEAD"], text=True).strip() ==
        args.source_commit, "payload checkout does not match immutable release source")
    require(not subprocess.check_output(
        ["git", "-C", str(args.source), "status", "--porcelain", "--untracked-files=no"],
        text=True).strip(), "payload checkout has modified tracked content")
    source_charts(args.source, args.tag)
    backend = Backend(args.source_commit, args.tag)
    backend.run()
    evidence = collect_release(
        args.source.resolve(), args.tag, args.source_commit)
    args.output.write_text(json.dumps(evidence, indent=2) + "\n")


if __name__ == "__main__":
    main()
