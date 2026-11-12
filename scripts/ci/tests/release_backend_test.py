#!/usr/bin/env python3
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

import contextlib
import io
import json
import os
import sys
import unittest
import urllib.parse
from pathlib import Path
from unittest.mock import patch

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
import release_backend as backend

SOURCE, CONTROL = "a" * 40, "b" * 40
PUBLISHERS = [
    {"name": name, "kind": "image", "pipelineId": index + 1,
     "controlRef": "refs/heads/main", "controlCommit": CONTROL}
    for index, name in enumerate(("tau", "taugrid-portal", "tau-core-controller",
                                  "taugrid-metrics-collector"))
] + [{"name": "charts", "kind": "charts", "pipelineId": 5}]
CONFIG = {
    "RELEASE_PUBLISHERS_CONFIG": json.dumps({
        "url": "https://dev.azure.com/example/project",
        "publishers": PUBLISHERS,
    }),
    "RELEASE_TENANT_ID": "00000000-0000-0000-0000-000000000001",
    "RELEASE_CLIENT_ID": "00000000-0000-0000-0000-000000000002",
    "RELEASE_OIDC_AUDIENCE": "api://AzureADTokenExchange",
    "ACTIONS_ID_TOKEN_REQUEST_URL":
        "https://example.actions.githubusercontent.com/token?request=1",
    "ACTIONS_ID_TOKEN_REQUEST_TOKEN": "runtime-token",
}


class ReleaseBackendTests(unittest.TestCase):
    def setUp(self):
        self.environment = patch.dict(os.environ, CONFIG, clear=True)
        self.environment.start()
        self.addCleanup(self.environment.stop)
        self.client = backend.Backend(SOURCE, "v1.2.3")
        self.image = self.client.publishers["tau"]
        self.chart = self.client.publishers["charts"]

    def run_fixture(self, publisher, run_id=20, state="completed", result="succeeded"):
        return {
            "id": run_id,
            "pipeline": {"id": publisher["pipelineId"]},
            "state": state,
            "result": result,
            "resources": {"repositories": self.client.resources(publisher)},
            "templateParameters": self.client.parameters(publisher),
        }

    def test_missing_configuration_fails_before_network(self):
        for name in CONFIG:
            with self.subTest(name=name), patch.dict(os.environ, {name: ""}), \
                    patch.object(backend, "request_bytes") as request:
                with self.assertRaises(ValueError):
                    backend.Backend(SOURCE, "v1.2.3")
                request.assert_not_called()

    def test_publisher_matrix_is_exact_and_immutable(self):
        for mutation in (
                PUBLISHERS[:-1],
                PUBLISHERS + [dict(PUBLISHERS[0])],
                [dict(entry, pipelineId=0) if entry["name"] == "tau" else entry
                 for entry in PUBLISHERS],
                [dict(entry, controlCommit="main") if entry["name"] == "tau" else entry
                 for entry in PUBLISHERS]):
            config = {"url": "https://dev.azure.com/example/project", "publishers": mutation}
            with self.subTest(mutation=mutation), patch.dict(
                    os.environ, {"RELEASE_PUBLISHERS_CONFIG": json.dumps(config)}):
                with self.assertRaises(ValueError):
                    backend.Backend(SOURCE, "v1.2.3")

    def test_image_queue_binds_control_source_and_release(self):
        self.assertEqual(self.client.queue_body(self.image), {
            "resources": {"repositories": {
                "self": {"refName": "refs/heads/main", "version": CONTROL},
                "taugrid": {"refName": SOURCE, "version": SOURCE},
            }},
            "templateParameters": {
                "sourceRef": SOURCE,
                "releaseTag": "1.2.3",
                "forcePublish": False,
            },
        })

    def test_chart_queue_binds_exact_tag_and_source(self):
        self.assertEqual(self.client.queue_body(self.chart), {
            "resources": {"repositories": {
                "self": {"refName": "refs/tags/v1.2.3", "version": SOURCE},
            }},
        })

    def test_chart_run_matches_exact_source_when_api_omits_ref_name(self):
        run = self.run_fixture(self.chart)
        del run["resources"]["repositories"]["self"]["refName"]
        self.assertTrue(self.client.release_run(self.chart, run))

    def test_chart_run_rejects_main_ref_at_release_source(self):
        run = self.run_fixture(self.chart)
        run["resources"]["repositories"]["self"]["refName"] = "refs/heads/main"
        self.assertFalse(self.client.release_run(self.chart, run))
        with self.assertRaisesRegex(ValueError, "ref mismatch"):
            self.client.validate_run(self.chart, run, run["id"])

    def test_existing_public_artifact_skips_queue(self):
        with patch.object(self.client, "artifact_ready", return_value=True), \
                patch.object(self.client, "api") as api:
            self.assertIsNone(self.client.queue_or_resume(self.image))
            api.assert_not_called()

    def test_active_exact_run_is_resumed(self):
        run = self.run_fixture(self.image, state="inProgress", result=None)
        with patch.object(self.client, "artifact_ready", return_value=False), \
                patch.object(self.client, "exact_runs", return_value=[run]), \
                patch.object(self.client, "api") as api:
            self.assertEqual(self.client.queue_or_resume(self.image), run["id"])
            api.assert_not_called()

    def test_failed_run_can_be_replaced_but_succeeded_missing_artifact_cannot(self):
        failed = self.run_fixture(self.image, result="failed")
        queued = {"id": 21}
        with patch.object(self.client, "artifact_ready", return_value=False), \
                patch.object(self.client, "exact_runs", return_value=[failed]), \
                patch.object(self.client, "api", return_value=queued):
            self.assertEqual(self.client.queue_or_resume(self.image), 21)
        succeeded = self.run_fixture(self.image)
        with patch.object(self.client, "artifact_ready", return_value=False), \
                patch.object(self.client, "exact_runs", return_value=[succeeded]), \
                patch.object(self.client, "api") as api:
            with self.assertRaisesRegex(ValueError, "succeeded"):
                self.client.queue_or_resume(self.image)
            api.assert_not_called()

    def test_uncertain_queue_recovers_only_one_new_exact_run(self):
        recovered = self.run_fixture(self.image, run_id=21, state="inProgress", result=None)
        with patch.object(self.client, "artifact_ready", return_value=False), \
                patch.object(self.client, "exact_runs",
                             side_effect=[[], [recovered]]), \
                patch.object(self.client, "api", side_effect=ValueError("uncertain")):
            self.assertEqual(self.client.queue_or_resume(self.image), 21)
        for runs in ([], [recovered, dict(recovered, id=22)]):
            with patch.object(self.client, "artifact_ready", return_value=False), \
                    patch.object(self.client, "exact_runs",
                                 side_effect=[[], runs]), \
                    patch.object(self.client, "api", side_effect=ValueError("uncertain")):
                with self.assertRaisesRegex(ValueError, "uncertain"):
                    self.client.queue_or_resume(self.image)

    def test_conflicting_release_run_is_rejected(self):
        run = self.run_fixture(self.image)
        run["resources"]["repositories"]["taugrid"]["version"] = "c" * 40
        with patch.object(self.client, "recent_runs", return_value=[run]):
            with self.assertRaisesRegex(ValueError, "conflicting"):
                self.client.exact_runs(self.image)

    def test_wait_requires_success_and_is_bounded(self):
        failed = self.run_fixture(self.image, result="failed")
        with patch.object(self.client, "api", return_value=failed):
            with self.assertRaisesRegex(ValueError, "did not succeed"):
                self.client.wait({"tau": failed["id"]})
        running = self.run_fixture(self.image, state="inProgress", result=None)
        with patch.object(self.client, "api", return_value=running), \
                patch.object(backend.time, "monotonic", side_effect=[0, 2]):
            with self.assertRaisesRegex(ValueError, "timed out"):
                self.client.wait({"tau": running["id"]}, timeout=1)

    def test_run_queues_all_missing_publishers_before_waiting(self):
        run_ids = iter(range(1, 6))
        with patch.object(self.client, "queue_or_resume",
                          side_effect=lambda publisher: next(run_ids)) as queue, \
                patch.object(self.client, "wait") as wait, \
                patch.object(self.client, "wait_artifacts") as artifacts:
            self.client.run()
            self.assertEqual(queue.call_count, 5)
            self.assertEqual(set(wait.call_args.args[0]), {*backend.IMAGES, "charts"})
            artifacts.assert_called_once_with()

    def test_oidc_exchange_is_short_lived_and_cached_only_in_memory(self):
        responses = [
            json.dumps({"value": "federated-assertion"}).encode(),
            json.dumps({"access_token": "ephemeral", "token_type": "Bearer",
                        "expires_in": 3600}).encode(),
        ]
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
        with patch.dict(os.environ, {"GITHUB_ACTIONS": "true"}), \
                contextlib.redirect_stdout(output):
            backend.mask("private%value\nnext\rline")
        self.assertEqual(output.getvalue(), "::add-mask::private%25value%0Anext%0Dline\n")


if __name__ == "__main__":
    unittest.main()
