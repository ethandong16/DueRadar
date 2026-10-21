"""Synthetic observation regressions; never contact a service or use live data."""
import datetime
import json
from pathlib import Path
import tempfile
import unittest

from observe import summarize


class ObservationTests(unittest.TestCase):
    def row(self, seconds=0, **changes):
        start = datetime.datetime(2026, 1, 1, tzinfo=datetime.timezone.utc)
        result = {
            "observed_at": (start + datetime.timedelta(seconds=seconds)).isoformat(),
            "pid": 10, "process_start_ticks": 100, "rss_kib": 1000,
            "cpu_seconds": seconds / 100,
            "accounts": [{"platform": "rain", "login_state": "logged_in", "sync_state": "success",
                          "last_success_at": start.isoformat(), "consecutive_failures": 0,
                          "needs_revalidation": False}],
        }
        result.update(changes)
        return result

    def report(self, rows):
        with tempfile.TemporaryDirectory() as directory:
            source = Path(directory) / "observation.jsonl"
            source.write_text("\n".join(json.dumps(row) for row in rows), encoding="utf-8")
            return summarize(source)

    def test_short_run_never_claims_day_coverage_by_sample_count(self):
        report = self.report([self.row(index) for index in range(97)])
        self.assertEqual(report["samples"], 97)
        self.assertFalse(report["covers_24_hours"])
        self.assertEqual(report["max_sample_gap_seconds"], 1)

    def test_failed_samples_do_not_count_as_a_new_process(self):
        failure = {"observed_at": self.row(900)["observed_at"], "error": "TimeoutExpired"}
        report = self.report([self.row(), failure, self.row(1800, rss_kib=1200)])
        self.assertEqual(report["process_identities"], 1)
        self.assertEqual(report["errors"], 1)
        self.assertEqual(report["rss_change_kib"], 200)
        self.assertEqual(report["max_sample_gap_seconds"], 900)

    def test_sync_stall_is_visible_even_when_process_stays_active(self):
        report = self.report([self.row(0), self.row(900), self.row(1800), self.row(2700)])
        rain = report["platform_sync"]["rain"]
        self.assertEqual(rain["success_timestamps"], 1)
        self.assertEqual(rain["max_success_age_seconds"], 2700)
        self.assertEqual(rain["stale_samples"], 1)
        self.assertEqual(rain["failed_samples"], 0)

    def test_sync_recovery_counts_failure_and_renewed_success(self):
        first, failed, recovered = self.row(), self.row(900), self.row(1800)
        failed["accounts"][0].update(sync_state="failed", consecutive_failures=1)
        recovered["accounts"][0]["last_success_at"] = recovered["observed_at"]
        report = self.report([first, failed, recovered])
        rain = report["platform_sync"]["rain"]
        self.assertEqual(rain["success_timestamps"], 2)
        self.assertEqual(rain["failed_samples"], 1)
        self.assertEqual(rain["max_success_age_seconds"], 900)
        self.assertEqual(rain["last_success_at"], recovered["observed_at"])

    def test_unbound_accounts_are_separate_from_missing_success(self):
        row = self.row()
        row["accounts"].append({"platform": "zhidao_shared", "login_state": "unbound", "sync_state": "idle"})
        row["accounts"].append({"platform": "zhidao_campus", "login_state": "expired", "sync_state": "failed"})
        report = self.report([row])["platform_sync"]
        self.assertEqual(report["zhidao_shared"]["unbound_samples"], 1)
        self.assertEqual(report["zhidao_shared"]["missing_success_samples"], 0)
        self.assertEqual(report["zhidao_campus"]["missing_success_samples"], 1)
        self.assertEqual(report["zhidao_campus"]["failed_samples"], 1)

    def test_24_hour_span_reports_gaps_and_restart_separately(self):
        end = self.row(86400, pid=11, process_start_ticks=200, cpu_seconds=1)
        report = self.report([self.row(), self.row(900), end])
        self.assertTrue(report["covers_24_hours"])
        self.assertEqual(report["max_sample_gap_seconds"], 85500)
        self.assertEqual(report["process_identities"], 2)
        self.assertIsNone(report["rss_change_kib"])

    def test_missing_or_malformed_success_is_counted_without_identity_data(self):
        row = self.row()
        row["accounts"][0]["last_success_at"] = "unexpected"
        report = self.report([row])
        self.assertEqual(report["platform_sync"]["rain"]["missing_success_samples"], 1)
        self.assertEqual(report["platform_sync"]["rain"]["success_timestamps"], 0)

    def test_go_timestamps_keep_fractional_seconds_and_utc_offset(self):
        for timestamp, expected_age in [
            ("2026-01-01T08:00:00.123456789+08:00", 1.876544),
            ("2026-01-01T00:00:00.1Z", 1.9),
            ("2026-01-01T00:00:00Z", 2.0),
        ]:
            with self.subTest(timestamp=timestamp):
                row = self.row(2)
                row["accounts"][0]["last_success_at"] = timestamp
                report = self.report([row])["platform_sync"]["rain"]
                self.assertEqual(report["missing_success_samples"], 0)
                self.assertEqual(report["success_timestamps"], 1)
                self.assertAlmostEqual(report["max_success_age_seconds"], expected_age)


if __name__ == "__main__":
    unittest.main()
