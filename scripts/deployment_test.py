"""Regression checks for the shipped service sandbox configuration."""
from pathlib import Path
import unittest


ROOT = Path(__file__).resolve().parents[1]


def unit_values():
    values = {}
    for line in (ROOT / "deploy" / "dueradar.service").read_text(encoding="utf-8").splitlines():
        line = line.strip()
        if not line or line.startswith("#") or "=" not in line:
            continue
        key, value = line.split("=", 1)
        values[key] = value
    return values


class DeploymentTests(unittest.TestCase):
    def test_service_uses_restrictive_sandbox(self):
        values = unit_values()
        for key in (
            "NoNewPrivileges",
            "PrivateTmp",
            "ProtectHome",
            "ProtectKernelTunables",
            "ProtectKernelModules",
            "ProtectKernelLogs",
            "ProtectControlGroups",
            "ProtectClock",
            "RestrictNamespaces",
            "RestrictRealtime",
            "LockPersonality",
            "RestrictSUIDSGID",
        ):
            self.assertEqual(values.get(key), "yes", key)
        self.assertEqual(values.get("ProtectSystem"), "strict")
        self.assertEqual(values.get("CapabilityBoundingSet"), "")
        self.assertEqual(values.get("RestrictAddressFamilies"), "AF_UNIX AF_INET AF_INET6")

    def test_service_keeps_the_data_directory_writable(self):
        values = unit_values()
        # ProtectSystem=strict only exempts paths managed by systemd. Losing
        # any of these would make the state/lock/credential directory unwritable
        # and the service fail at startup.
        self.assertEqual(values.get("DynamicUser"), "yes")
        self.assertEqual(values.get("StateDirectory"), "dueradar")
        self.assertEqual(values.get("WorkingDirectory"), "/var/lib/dueradar")
        self.assertEqual(values.get("Environment"), "DUERADAR_DATA_DIR=/var/lib/dueradar")


if __name__ == "__main__":
    unittest.main()
