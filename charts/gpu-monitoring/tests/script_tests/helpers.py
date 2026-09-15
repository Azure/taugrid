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
    if args == "-L":
        if os.environ.get("NVIDIA_SMI_LIST_FAIL") == "1":
            print("Failed to initialize NVML", file=sys.stderr)
            sys.exit(1)
        print("GPU 0: NVIDIA GPU (UUID: GPU-REAL)")
    elif args == "nvlink --status":
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
    state_file = Path(os.environ["DCGMI_WATCH_STATE_FILE"])
    command_log = os.environ.get("DCGMI_COMMAND_LOG")
    if command_log:
        with Path(command_log).open("a") as log:
            log.write(args + "\n")
    if args == "health -s a":
        if os.environ.get("DCGMI_SET_FAIL") == "1":
            print("Error: Unable to set health watches.", file=sys.stderr)
            sys.exit(int(os.environ.get("DCGMI_SET_EXIT_CODE", "5")))
        state_file.write_text("a\n")
        print("Health monitor systems set successfully.")
        sys.exit(0)
    if args == "health -f":
        if os.environ.get("DCGMI_FETCH_FAIL") == "1":
            print("Error: Unable to get health watches.", file=sys.stderr)
            sys.exit(int(os.environ.get("DCGMI_FETCH_EXIT_CODE", "6")))
        enabled = state_file.exists()
        partial = os.environ.get("DCGMI_PARTIAL_WATCHES") == "1" or (
            enabled and state_file.read_text().strip() == "partial"
        )
        print("Health monitor systems report")
        for system in (
            "PCIe",
            "NVLINK",
            "Memory",
            "SM",
            "InfoROM",
            "Thermal",
            "Power",
            "Driver",
            "NvSwitch NF",
            "NvSwitch F",
        ):
            on = enabled and system != "SM" and not (partial and system == "Driver")
            print(f"| {system:<12} | {'On' if on else 'Off':<3} |")
        sys.exit(0)
    if args == "health -c":
        if not state_file.exists():
            print(
                "Error: Health watches not enabled. Please enable watches.",
                file=sys.stderr,
            )
            sys.exit(253)
        print(
            os.environ.get(
                "DCGMI_HEALTH_OUTPUT",
                "Health Monitor Report\n| Overall Health | Healthy |",
            )
        )
        if os.environ.get("DCGMI_PARTIAL_WATCHES_AFTER_CHECK") == "1":
            state_file.write_text("partial\n")
        sys.exit(int(os.environ.get("DCGMI_HEALTH_EXIT_CODE", "0")))
    if args == "health -t":
        print("PARSE ERROR: Argument: -t", file=sys.stderr)
        sys.exit(254)
    sys.exit(2)
elif name == "nsenter":
    Path(os.environ["NSENTER_ARGS_FILE"]).write_text(args + "\n")
elif name == "node-problem-detector":
    Path(os.environ["NPD_ARGS_FILE"]).write_text(args + "\n")
elif name == "sleep":
    sleep_log = os.environ.get("SLEEP_ARGS_FILE")
    if sleep_log:
        with Path(sleep_log).open("a") as log:
            log.write(args + "\n")
    state_file = Path(os.environ["DCGMI_WATCH_STATE_FILE"])
    if os.environ.get("DCGMI_DROP_WATCHES_DURING_SLEEP") == "1":
        state_file.unlink(missing_ok=True)
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
        self.dcgm_state_file = self.test_root / "dcgm-watches"
        self.dcgm_command_log = self.test_root / "dcgmi-commands"
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
            "node-problem-detector",
            "pgrep",
            "sleep",
            "timeout",
        ):
            (self.mock_bin / command).symlink_to(dispatcher)

    def tearDown(self):
        self._temporary_directory.cleanup()

    def run_script(self, script, *, expected=0, env=None, args=()):
        command_env = os.environ.copy()
        command_env["PATH"] = f"{self.mock_bin}{os.pathsep}{command_env['PATH']}"
        command_env["DCGMI_WATCH_STATE_FILE"] = str(self.dcgm_state_file)
        command_env["DCGMI_COMMAND_LOG"] = str(self.dcgm_command_log)
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
