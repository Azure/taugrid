#!/usr/bin/env python3
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

import copy
import contextlib
import io
import json
import os
import sys
import unittest
import urllib.parse
import zipfile
from pathlib import Path
from unittest.mock import patch

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
import release_backend as backend

SOURCE, CONTROL, TOOLS = "a" * 40, "b" * 40, "c" * 40
CONFIG = {
    "RELEASE_BACKEND_URL": "https://dev.azure.com/example/project",
    "RELEASE_BACKEND_PIPELINE_ID": "1",
    "RELEASE_BACKEND_CONTROL_SHA": CONTROL,
    "RELEASE_BACKEND_CONTROL_REF": "refs/heads/main",
    "RELEASE_TENANT_ID": "00000000-0000-0000-0000-000000000001",
    "RELEASE_CLIENT_ID": "00000000-0000-0000-0000-000000000002",
    "RELEASE_OIDC_AUDIENCE": "api://AzureADTokenExchange",
    "ACTIONS_ID_TOKEN_REQUEST_URL": "https://example.actions.githubusercontent.com/token?request=1",
    "ACTIONS_ID_TOKEN_REQUEST_TOKEN": "runtime-token",
}


class ReleaseBackendTests(unittest.TestCase):
    def setUp(self):
        self.environment = patch.dict(os.environ, CONFIG, clear=True)
        self.environment.start()
        self.addCleanup(self.environment.stop)
        self.client = backend.Backend(SOURCE, "v1.2.3", TOOLS)
        self.run = {"id": 2, "pipeline": {"id": 1}, "state": "completed", "result": "succeeded",
                    "resources": {"repositories": {
                        "self": {"version": CONTROL}, "taugrid": {"version": SOURCE},
                        "releaseTools": {"version": TOOLS}}},
                    "templateParameters": self.client.parameters()}

    def test_missing_configuration_fails_before_network(self):
        for name in CONFIG:
            with self.subTest(name=name), patch.dict(os.environ, {name: ""}), \
                    patch.object(backend, "request_bytes") as request:
                with self.assertRaises(ValueError):
                    backend.Backend(SOURCE, "v1.2.3", TOOLS)
                request.assert_not_called()

    def test_queue_binds_three_revisions_and_never_forces(self):
        with patch.object(self.client, "api", side_effect=[{"id": 2}, self.run]) as api:
            self.assertEqual(self.client.run(), 2)
        body = api.call_args_list[0].args[1]
        self.assertEqual(body["templateParameters"], {
            "sourceRef": SOURCE, "releaseTag": "1.2.3", "forcePublish": False,
            "orchestrationRef": TOOLS})
        self.assertEqual(body["resources"]["repositories"]["self"]["version"], CONTROL)
        self.assertEqual(body["resources"]["repositories"]["taugrid"]["version"], SOURCE)
        self.assertEqual(body["resources"]["repositories"]["releaseTools"]["version"], TOOLS)

    def test_resume_never_queues(self):
        with patch.object(self.client, "api", return_value=self.run) as api:
            self.assertEqual(self.client.run(2), 2)
            self.assertEqual(api.call_count, 1)
            self.assertEqual(len(api.call_args.args), 1)
        with patch.object(self.client, "api") as api:
            with self.assertRaises(ValueError):
                self.client.run(-1)
            api.assert_not_called()

    def test_unknown_failed_canceled_or_mismatched_run_never_succeeds(self):
        invalid_runs = []
        for state, result in (("completed", "failed"), ("completed", "canceled"),
                              ("completed", "unknown"), ("unknown", None), ("canceling", None)):
            invalid_runs.append(dict(self.run, state=state, result=result))
        for name in ("self", "taugrid", "releaseTools"):
            invalid = copy.deepcopy(self.run)
            invalid["resources"]["repositories"][name]["version"] = "d" * 40
            invalid_runs.append(invalid)
        for field, value in (("forcePublish", True), ("sourceRef", CONTROL),
                             ("releaseTag", "1.2.4"), ("orchestrationRef", SOURCE)):
            invalid = copy.deepcopy(self.run)
            invalid["templateParameters"][field] = value
            invalid_runs.append(invalid)
        invalid_runs.extend((dict(self.run, id=3), dict(self.run, pipeline={"id": 2})))
        for invalid in invalid_runs:
            with self.subTest(invalid=invalid), patch.object(self.client, "api", return_value=invalid):
                with self.assertRaises(ValueError):
                    self.client.run(2)

    def test_uncertain_queue_is_not_retried(self):
        with patch.object(self.client, "api", side_effect=ValueError("uncertain")) as api:
            with self.assertRaises(ValueError):
                self.client.run()
            self.assertEqual(api.call_count, 1)

    def test_wait_is_bounded(self):
        running = dict(self.run, state="inProgress", result=None)
        with patch.object(self.client, "api", return_value=running), \
                patch.object(backend.time, "monotonic", side_effect=[0, 2]):
            with self.assertRaisesRegex(ValueError, "timed out"):
                self.client.run(2, timeout=1)

    def test_oidc_exchange_is_short_lived_and_cached_only_in_memory(self):
        responses = [json.dumps({"value": "federated-assertion"}).encode(),
                     json.dumps({"access_token": "ephemeral", "token_type": "Bearer",
                                 "expires_in": 3600}).encode()]
        with patch.object(backend, "request_bytes", side_effect=responses) as request:
            self.assertEqual(self.client.access_token(), "ephemeral")
            self.assertEqual(self.client.access_token(), "ephemeral")
            self.assertEqual(request.call_count, 2)
            form = urllib.parse.parse_qs(request.call_args.kwargs["data"].decode())
            self.assertEqual(form["scope"], ["https://app.vssps.visualstudio.com/.default"])
            self.assertEqual(form["client_assertion"], ["federated-assertion"])
            self.assertNotIn("client_secret", form)
            self.assertNotIn("ephemeral", os.environ.values())

    def test_no_authenticated_redirects(self):
        self.assertIsNone(backend.NoRedirect().redirect_request(
            None, None, 302, "redirect", {}, "https://other.invalid/"))

    def test_dynamic_masks_escape_workflow_command_characters(self):
        output = io.StringIO()
        with patch.dict(os.environ, {"GITHUB_ACTIONS": "true"}), contextlib.redirect_stdout(output):
            backend.mask("private%value\nnext\rline")
        self.assertEqual(output.getvalue(), "::add-mask::private%25value%0Anext%0Dline\n")

    def test_receipt_download_has_no_authorization_and_rejects_extra_files(self):
        artifact = {"name": "release-receipt",
                    "signedContent": {"url": "https://example.invalid/signed?token=redacted"}}
        for names in (["release-receipt.json"], ["release-receipt.json", "extra.json"],
                      ["../release-receipt.json"]):
            stream = io.BytesIO()
            with zipfile.ZipFile(stream, "w") as archive:
                for name in names:
                    archive.writestr(name, '{"schema":1}')
            with patch.object(self.client, "api", return_value=artifact), \
                    patch.object(backend, "request_bytes", return_value=stream.getvalue()) as request:
                if len(names) == 1 and names[0] == "release-receipt.json":
                    self.assertEqual(self.client.receipt(2), {"schema": 1})
                else:
                    with self.assertRaises(ValueError):
                        self.client.receipt(2)
                self.assertNotIn("token", request.call_args.kwargs)


if __name__ == "__main__":
    unittest.main()
