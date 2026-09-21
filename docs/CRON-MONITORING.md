# Standard cron job monitoring (`linux.cron.v1`)

`linux.cron.v1` proves the outcome and timeliness of individual cron jobs. It
complements `linux.systemd.v1`, whose `cron_daemon_state` only proves that the
scheduler daemon itself is active.

## Trust and privacy boundary

The signed `bob-icu-cron-run` binary executes the original command and writes
one small local JSON state file. The state contains only:

- stable job ID;
- last start, finish and successful-finish timestamps;
- last exit code and duration;
- whether a run is currently in progress.

It never stores command arguments, stdout, stderr, environment variables or
credentials. Original stdin/stdout/stderr behavior is preserved, so normal cron
logging or mail remains available.

State writes are atomic and synced. State reads reject symlinks, non-regular
files, files over 64 KiB, unknown JSON fields, wrong schemas and mismatched job
IDs. Invalid state is replaced instead of blocking the scheduled workload. A
per-job lock rejects overlapping executions with exit code `75`.

Monitoring I/O is deliberately fail-open for the workload: an unavailable or
damaged state path never prevents the original command from running. If the
command succeeds but its final monitoring state cannot be committed, the
wrapper returns `125` so cron logging or mail exposes the monitoring failure.
If the command itself fails, its original non-zero exit code wins.

## Install the writer

The release archive contains `bob-icu-cron-run`. Install it root-owned and not
writable by scheduled jobs:

```bash
install -m 0755 -o root -g root bob-icu-cron-run /usr/local/bin/bob-icu-cron-run
groupadd --system bob-icu-cron  # only when the group does not yet exist
install -d -m 2775 -o root -g bob-icu-cron /var/lib/bob-icu-cron
```

Root-run cron jobs need no additional group membership. For a non-root job, add
only that existing Unix account to `bob-icu-cron`, then prove its effective
group membership before changing the crontab. State files are mode `0644` and
contain no secrets; the BOB ICU client only needs read/traverse access and must
not be added to the writer group.

## Register the sensor

Enable the package and define each job in `/etc/bob-icu/client.json`:

```json
{
  "packages": ["linux.base.v1", "linux.systemd.v1", "linux.cron.v1"],
  "cron_state_directory": "/var/lib/bob-icu-cron",
  "cron_jobs": [
    {
      "job_id": "nightly_backup",
      "display_name": "Nächtliche Sicherung",
      "expected_interval_seconds": 86400,
      "grace_seconds": 1800,
      "max_runtime_seconds": 7200,
      "poll_interval_seconds": 60
    }
  ]
}
```

- `expected_interval_seconds` is the largest normal gap between successful
  runs, not a cron expression (60 seconds to 31 days).
- `grace_seconds` absorbs normal scheduling variation and may not exceed the
  expected interval. If omitted, the client uses 10%, bounded to 5–60 minutes.
- `max_runtime_seconds` marks a still-running job critical after the limit. If
  omitted, it defaults to the expected interval, capped at one hour.
- `poll_interval_seconds` controls local state reads and defaults to 60 seconds.
- At most 32 jobs fit in the package service, matching the server's default
  per-service sensor ceiling.

The package fails configuration validation when jobs are absent, duplicated,
unsafe or outside the supported bounds.

## Wrap the existing cron entry

Original:

```cron
0 3 * * * /usr/local/sbin/nightly-backup --quiet
```

Monitored:

```cron
0 3 * * * /usr/local/bin/bob-icu-cron-run -job nightly_backup -- /usr/local/sbin/nightly-backup --quiet
```

No shell is inserted by the wrapper. Pipelines, redirects or shell built-ins
must remain explicit:

```cron
0 4 * * * /usr/local/bin/bob-icu-cron-run -job report -- /bin/sh -lc '/opt/report | /opt/archive'
```

Do not put secrets directly in cron command lines. Preserve the job's existing
credential mechanism and environment.

## State interpretation

- no state / never successful: `warning`;
- running within its maximum runtime: `healthy` with a running message;
- non-zero latest exit code: `critical`;
- running beyond maximum runtime: `critical`;
- last success older than expected interval plus grace: `critical`;
- recent successful run: `healthy` with age and duration.

Wrapper exit codes `75` and `125` are reserved for overlap and monitoring
infrastructure failures. Every other command exit code is preserved.

A host clock error can distort age calculations; pair this package with
`linux.systemd.v1` so NTP status is visible independently.

## Safe migration and rollback

For every host, inventory exact crontab files and owners first. Propose each
line-level change, the new job ID and timing parameters, then obtain approval.
Back up the original crontab and BOB ICU configuration. Validate the new client
configuration under the `bob-icu-client` service identity before restarting
only that client. Run each wrapped command once with a harmless or explicitly
approved real execution path, verify the state and server reading, and ensure
the original exit code is preserved.

Rollback restores the original cron line and client configuration, then
restarts only `bob-icu-client.service`. State files can remain as historical
evidence or be moved to a protected quarantine; they are not executable.
