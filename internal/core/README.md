# Reminder core

`Open(path)` returns an `*Engine`. The public API requested by the service is
implemented: `Snapshot`, `UpdateSettings`, `Sync`, `Fail`, `Pending`, `MarkSent`,
`MarkFailed`, `RequireRevalidation`, and `SetAccount`. `UpdateSettingsAt` is also
available for deterministic administration/tests. JSON field names and exact Go
types live in `model.go`; time fields on tasks are `StartAt` and `EndAt`.

- Platforms are `rain`, `zhidao_shared`, and `zhidao_campus`.
- Task kinds are `homework`, `exam`, `chapter`, and `video`; statuses are
  `pending`, `completed`, and `unknown`. Progress uses the 0–100 percentage scale.
- Accounts begin as `login_state=unbound`, `sync_state=idle`. A successful sync
  produces `logged_in` / `success`. Failures produce `sync_state=error`;
  expired credentials produce `login_state=expired`. Reopening a previously
  synchronized store produces `sync_state=revalidating` until a successful sync.
- Events use `new_task`, `time_changed`, `deadline`, `video_deadline`,
  `exam_start`, and `fault`; statuses are `pending`, `sending`, `sent`,
  `canceled`, and `failed`. Every event contains `Title` and `Body` for Bark.
- Settings contain only reminder/quiet configuration; Bark credentials belong
  in the service's separate notification configuration.

`Pending(now)` durably claims each returned event for two minutes. The caller
must send each claimed event and call `MarkSent` or `MarkFailed`; repeated calls
cannot immediately claim the same event. Transport should finish within the
lease (the service should use a much shorter request timeout). Delivery retries
wait 1, 2, 4, 8, and 16 minutes, then stop after six total attempts. Persisted
failure diagnostics are fixed messages rather than raw provider/transport
errors, which may contain private session or Bark keys.

All task reminders require a successfully verified platform snapshot no older
than 30 minutes. Any sync failure gates task delivery immediately. Reopening or
explicit recovery also gates delivery, then each platform's successful sync
collapses already-due valid events to the most recent per task. Future events
remain independently scheduled. Expired task deadlines, started exams, unknown
or completed tasks, removed offsets, and superseded timestamps do not send.

Quiet hours are interpreted in the configured timezone, including intervals
across midnight. Only configured exact 60-minute exam-start and exact 30-minute
deadline events bypass quiet hours, including retries delayed from those same
events. This applies to a custom 30-minute video reminder, but never creates one
under the default video settings. Other configured offsets and discovery/change
notifications do not gain urgency by entering those time windows.

`Sync` consumes a complete snapshot for one platform. Omitted unfinished tasks
remain in history with unknown status and stop generating reminders. A new
task produces one discovery event; past scheduled offsets are not backfilled
at first discovery or after settings changes. Completed history is retained.
Full snapshots must contain unique IDs per platform. Unknown timestamps must
be `nil`, never invented by an adapter.

State writes use a private temporary file, file sync, and atomic rename in the
same directory. The store is thread safe within a single process; run only one
process per data directory. Successful remote delivery followed by a process
crash before `MarkSent` can result in a retry: exactly-once notification delivery
requires idempotency support from the remote notification service.

Validation: `go test ./internal/core` covers defaults, midnight boundaries,
first-sync/persistent dedup, urgent exceptions, custom offsets, video schedules,
unknown/completed/omitted states, recovery collapse, expired exam-start events,
quiet/recovery overlap, timestamp changes, fault episodes, freshness, retries,
settings changes, snapshot isolation, failed commits, and concurrent snapshots.
