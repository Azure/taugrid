# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

import unittest

from helpers import ScriptTestCase


class InfinibandTests(ScriptTestCase):
    def create_fabric(self, name, device_prefix, rate, pkey):
        root = self.test_root / name
        devices = []
        for index in range(8):
            device = f"{device_prefix}{index}"
            port_root = root / "class" / "infiniband" / device / "ports" / "1"
            (port_root / "pkeys").mkdir(parents=True)
            (port_root / "state").write_text("4: ACTIVE\n")
            (port_root / "phys_state").write_text("5: LinkUp\n")
            (port_root / "rate").write_text(f"{rate} Gb/sec\n")
            (port_root / "pkeys" / "0").write_text(f"{pkey}\n")
            devices.append(f"{device}:1")
        return root, " ".join(devices)

    def assert_fabric_healthy(self, root, devices, rate, pkey):
        common = {"SYSFS_ROOT": str(root), "IB_DEVICES": devices}
        self.run_script(
            "check_ib.sh", env={**common, "EXPECTED_IB_GBPS": str(rate)}
        )
        self.run_script(
            "check_ib_pkeys.sh", env={**common, "EXPECTED_IB_PKEY": pkey}
        )

    def test_supported_fabrics_are_healthy(self):
        profiles = (
            ("flex-a100", "mlx5_", 200, "0x8003"),
            ("flex-h200", "mlx5_ib", 400, "0xffff"),
            ("east-h200", "mlx5_", 400, "0x8001"),
        )
        for profile in profiles:
            with self.subTest(profile=profile[0]):
                root, devices = self.create_fabric(*profile)
                self.assert_fabric_healthy(root, devices, profile[2], profile[3])

    def test_pkey_mismatch_does_not_affect_link_health(self):
        root, devices = self.create_fabric("east-h200", "mlx5_", 400, "0x8001")
        (root / "class/infiniband/mlx5_0/ports/1/pkeys/0").write_text("0x8003\n")
        common = {"SYSFS_ROOT": str(root), "IB_DEVICES": devices}
        self.run_script("check_ib.sh", env={**common, "EXPECTED_IB_GBPS": "400"})
        output = self.run_script(
            "check_ib_pkeys.sh",
            expected=1,
            env={**common, "EXPECTED_IB_PKEY": "0x8001"},
        )
        self.assertIn("mlx5_0:1 expected PKey 0x8001; observed 0x8003", output)

    def test_link_mismatch_does_not_affect_pkey_health(self):
        root, devices = self.create_fabric("flex-h200", "mlx5_ib", 400, "0xffff")
        port = root / "class/infiniband/mlx5_ib0/ports/1"
        (port / "state").write_text("2: DOWN\n")
        (port / "rate").write_text("200 Gb/sec\n")
        common = {"SYSFS_ROOT": str(root), "IB_DEVICES": devices}
        self.run_script(
            "check_ib_pkeys.sh",
            env={**common, "EXPECTED_IB_PKEY": "0xffff"},
        )
        output = self.run_script(
            "check_ib.sh",
            expected=1,
            env={**common, "EXPECTED_IB_GBPS": "400"},
        )
        self.assertIn(
            "mlx5_ib0:1 expected state=ACTIVE physical_state=LinkUp rate=400Gbps; "
            "observed state=DOWN physical_state=LinkUp rate=200Gbps",
            output,
        )

    def test_pkey_must_be_explicit(self):
        root, devices = self.create_fabric("flex-a100", "mlx5_", 200, "0x8003")
        output = self.run_script(
            "check_ib_pkeys.sh",
            expected=1,
            env={"SYSFS_ROOT": str(root), "IB_DEVICES": devices},
        )
        self.assertIn(
            "EXPECTED_IB_PKEY must be an explicit hexadecimal PKey", output
        )


if __name__ == "__main__":
    unittest.main()
