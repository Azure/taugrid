# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

import unittest

from helpers import ScriptTestCase


class InfinibandFlapTests(ScriptTestCase):
    def test_unknown_samples_do_not_count_as_flaps(self):
        state = self.test_root / "unknown-state.txt"
        state.write_text(
            "7000 mlx5_0:1=up\n"
            "7600 mlx5_0:1=unknown\n"
            "8200 mlx5_0:1=error\n"
            "8800 mlx5_0:1=unknown\n"
            "9400 mlx5_0:1=up\n"
        )
        self.run_script(
            "check_ib_flaps.sh",
            env={
                "TEST_NOW": "10000",
                "IB_DEVICES": "mlx5_0:1",
                "IB_FLAP_THRESHOLD_SHORT": "1",
                "IB_FLAP_CHECK_WINDOW": "3600",
                "IB_FLAP_STATE_FILE": str(state),
            },
        )

    def test_retention_is_time_based_and_repeatable(self):
        state = self.test_root / "flap-state.txt"
        samples = [
            (6300, "up"),
            (6500, "up"),
            (7200, "down"),
            (7900, "up"),
            (8600, "down"),
            (9300, "up"),
            *[(timestamp, "up") for timestamp in range(9350, 9950, 50)],
        ]
        state.write_text(
            "".join(f"{timestamp} mlx5_0:1={status}\n" for timestamp, status in samples)
        )
        env = {
            "TEST_NOW": "10000",
            "IB_DEVICES": "mlx5_0:1",
            "IB_FLAP_THRESHOLD_SHORT": "2",
            "IB_FLAP_CHECK_WINDOW": "3600",
            "IB_FLAP_STATE_FILE": str(state),
        }

        first = self.run_script("check_ib_flaps.sh", expected=1, env=env)
        self.assertIn("2 ibstat state flaps", first)
        retained = state.read_text().splitlines()
        self.assertFalse(any(line.startswith("6300 ") for line in retained))
        self.assertTrue(any(line.startswith("6500 ") for line in retained))
        self.assertGreater(len(retained), 10)

        second = self.run_script("check_ib_flaps.sh", expected=1, env=env)
        self.assertIn("2 ibstat state flaps", second)


if __name__ == "__main__":
    unittest.main()
