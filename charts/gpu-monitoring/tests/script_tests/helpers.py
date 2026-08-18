# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

import os
import subprocess
import tempfile
import unittest
from pathlib import Path

CHART_DIR = Path(__file__).resolve().parents[2]

MOCK_COMMAND = r"""#!/usr/bin/env python3
import os
import sys
from pathlib import Path

name = Path(sys.argv[0]).name
args = " ".join(sys.argv[1:])

if name == "nvidia-smi":
    if args == "nvlink --status":
        if os.environ.get("NVIDIA_SMI_NVLINK_STATUS_FAIL") == "1":
            print("nvlink status query failed", file=sys.stderr)
            sys.exit(1)
        if os.environ.get("NVIDIA_SMI_EMPTY_NVLINK_STATUS") == "1":
            sys.exit(0)
        print("Link 0: 50 GB/s")
    elif args in ("nvlink -s -i 0", "nvlink --id=0 --status"):
        if os.environ.get("NVIDIA_SMI_NVLINK_DETAIL_FAIL") == "1":
            print("nvlink detail query failed", file=sys.stderr)
            sys.exit(1)
        print("Link 0: 50 GB/s")
    elif args == "c2c --id=0 --status":
        if os.environ.get("NVIDIA_SMI_C2C_FAIL") == "1":
            print("c2c status query failed", file=sys.stderr)
            sys.exit(1)
        print("Link 0: 50 GB/s")
    elif args == "topo -m":
        print(" ".join(["NV18"] * 12))
    elif args == "--id=0 --query-gpu=driver_version --format=csv,noheader":
        print("580.126.09")
    elif args == "--query-gpu=clocks_event_reasons.active --format=csv,noheader,nounits":
        if os.environ.get("NVIDIA_SMI_THROTTLE_FAIL") == "1":
            print("active reason query failed", file=sys.stderr)
            sys.exit(1)
        print(os.environ.get("NVIDIA_SMI_THROTTLE_OUTPUT", "0x0000000000000000"))
    elif args == "-q":
        if os.environ.get("NVIDIA_SMI_QUERY_FAIL") == "1":
            print("device query failed", file=sys.stderr)
            sys.exit(1)
        print("==============NVSMI LOG==============")
        if os.environ.get("NVIDIA_SMI_NO_VBIOS") != "1":
            versions = os.environ.get(
                "NVIDIA_SMI_VBIOS_VERSIONS", "96.00.BC.00.02"
            ).split()
            for version in versions:
                print(f"    VBIOS Version                         : {version}")
    else:
        sys.exit(1)
elif name == "pgrep":
    sys.exit(0)
elif name == "dcgmi":
    if args == "health -c":
        sys.exit(0)
    if args == "health -t":
        print(os.environ.get("DCGMI_HEALTH_OUTPUT", "DCGM health check passed"))
        sys.exit(int(os.environ.get("DCGMI_HEALTH_EXIT_CODE", "0")))
    sys.exit(2)
elif name == "nsenter":
    Path(os.environ["NSENTER_ARGS_FILE"]).write_text(args + "\n")
elif name == "journalctl":
    print(os.environ.get("JOURNALCTL_OUTPUT", ""))
    sys.exit(int(os.environ.get("JOURNALCTL_EXIT_CODE", "0")))
elif name == "date":
    if args == "+%s" and os.environ.get("TEST_NOW"):
        print(os.environ["TEST_NOW"])
    else:
        os.execv("/bin/date", ["/bin/date", *sys.argv[1:]])
elif name == "ibstat":
    print("CA 'mlx5_0'\n  Port 1:\n    State: Active")
elif name == "timeout":
    os.execvp(sys.argv[2], sys.argv[2:])
else:
    sys.exit(127)
"""


class ScriptTestCase(unittest.TestCase):
    def setUp(self):
        self._temporary_directory = tempfile.TemporaryDirectory()
        self.test_root = Path(self._temporary_directory.name)
        self.mock_bin = self.test_root / "bin"
        self.mock_bin.mkdir()
        dispatcher = self.mock_bin / "mock-command"
        dispatcher.write_text(MOCK_COMMAND)
        dispatcher.chmod(0o755)
        for command in (
            "date",
            "dcgmi",
            "ibstat",
            "journalctl",
            "nsenter",
            "nvidia-smi",
            "pgrep",
            "timeout",
        ):
            (self.mock_bin / command).symlink_to(dispatcher)

    def tearDown(self):
        self._temporary_directory.cleanup()

    def run_script(self, script, *, expected=0, env=None, args=()):
        command_env = os.environ.copy()
        command_env["PATH"] = f"{self.mock_bin}{os.pathsep}{command_env['PATH']}"
        command_env.update(env or {})
        result = subprocess.run(
            ["bash", str(CHART_DIR / "scripts" / script), *args],
            check=False,
            capture_output=True,
            text=True,
            env=command_env,
        )
        output = result.stdout + result.stderr
        self.assertEqual(
            expected,
            result.returncode,
            f"{script} returned {result.returncode}, expected {expected}:\n{output}",
        )
        return output
