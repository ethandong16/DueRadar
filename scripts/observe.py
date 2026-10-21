#!/usr/bin/env python3
"""Record bounded Linux service observations without task titles or credentials."""
import argparse
import collections
import datetime
import hashlib
import json
import os
from pathlib import Path
import subprocess
import time


def state_summary(path):
    with path.open(encoding="utf-8") as stream:
        state = json.load(stream)
    return {
        "tasks": dict(collections.Counter(task.get("platform", "unknown") for task in state.get("tasks", []))),
        "task_status": dict(collections.Counter(task.get("status", "unknown") for task in state.get("tasks", []))),
        "event_status": dict(collections.Counter(event.get("status", "unknown") for event in state.get("events", []))),
        "accounts": [{key: account.get(key) for key in ("platform", "login_state", "sync_state", "last_success_at", "consecutive_failures", "needs_revalidation")} for account in state.get("accounts", [])],
    }


def sample(service, data, binary):
    result = subprocess.run(["systemctl", "show", service, "--property=MainPID,ActiveState,NRestarts"], capture_output=True, text=True, timeout=10, check=True)
    fields = dict(line.split("=", 1) for line in result.stdout.splitlines() if "=" in line)
    row = {"observed_at": datetime.datetime.now(datetime.timezone.utc).isoformat(), "service": fields.get("ActiveState"), "pid": int(fields.get("MainPID", 0)), "restarts": int(fields.get("NRestarts", 0))}
    if row["service"] != "active" or row["pid"] <= 0:
        row["error"] = "service_inactive"
        return row
    proc = Path("/proc") / str(row["pid"])
    status = dict(line.split(":", 1) for line in (proc / "status").read_text().splitlines() if ":" in line)
    row.update({"rss_kib": int(status["VmRSS"].split()[0]), "hwm_kib": int(status["VmHWM"].split()[0]), "threads": int(status["Threads"])})
    ticks = (proc / "stat").read_text().rsplit(") ", 1)[1].split()
    row["cpu_seconds"] = (int(ticks[11]) + int(ticks[12])) / os.sysconf("SC_CLK_TCK")
    row["process_start_ticks"] = int(ticks[19])
    row["binary_sha256"] = hashlib.sha256(binary.read_bytes()).hexdigest()
    row.update(state_summary(data / "state.json"))
    return row


def summarize(path):
    rows = [json.loads(line) for line in path.read_text(encoding="utf-8").splitlines() if line.strip()]
    if not rows:
        return {"samples": 0, "complete": False}
    start = datetime.datetime.fromisoformat(rows[0]["observed_at"])
    end = datetime.datetime.fromisoformat(rows[-1]["observed_at"])
    return {
        "samples": len(rows), "duration_seconds": (end - start).total_seconds(),
        "errors": sum("error" in row for row in rows),
        "max_rss_kib": max((row.get("rss_kib", 0) for row in rows), default=0),
        "max_hwm_kib": max((row.get("hwm_kib", 0) for row in rows), default=0),
        "process_identities": len({(row.get("pid"), row.get("process_start_ticks")) for row in rows}),
        "last": rows[-1],
    }


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--service", default="dueradar.service")
    parser.add_argument("--data", type=Path, default=Path("/var/lib/dueradar"))
    parser.add_argument("--binary", type=Path, default=Path("/opt/dueradar/dueradar"))
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--samples", type=int, default=97)
    parser.add_argument("--interval", type=int, default=900)
    parser.add_argument("--summarize", action="store_true")
    args = parser.parse_args()
    if args.summarize:
        print(json.dumps(summarize(args.output), ensure_ascii=False, indent=2))
        return 0
    if args.samples < 1 or args.samples > 10000 or args.interval < 1:
        parser.error("samples must be 1..10000 and interval must be positive")
    args.output.parent.mkdir(parents=True, exist_ok=True)
    # Exclusive creation protects a previous observation from accidental reset.
    with args.output.open("x", encoding="utf-8") as stream:
        os.chmod(args.output, 0o600)
        for index in range(args.samples):
            started = time.monotonic()
            try:
                row = sample(args.service, args.data, args.binary)
            except Exception as error:
                # Keep diagnostics structural; exception details may contain paths or data.
                row = {"observed_at": datetime.datetime.now(datetime.timezone.utc).isoformat(), "error": type(error).__name__}
            stream.write(json.dumps(row, ensure_ascii=False) + "\n")
            stream.flush()
            os.fsync(stream.fileno())
            if index + 1 < args.samples:
                time.sleep(max(0, args.interval - (time.monotonic() - started)))
    report = summarize(args.output)
    report["complete"] = report["samples"] == args.samples
    print(json.dumps(report, ensure_ascii=False, indent=2))
    return 1 if report["errors"] else 0


if __name__ == "__main__":
    raise SystemExit(main())
