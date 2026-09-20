# Monitoring the BOB ICU server

The BOB ICU VPS can be enrolled as a normal patient. This is useful for host
resources, receiver health, database integrity, public HTTPS/TLS and supporting
system services. It does **not** replace an external sentinel: if the whole VPS,
network path or receiver is unavailable, the co-located client cannot deliver
an alarm until connectivity returns.

The integration uses a narrow status bridge. The bridge runs as the existing
`monitoring-ingest` account, performs read-only checks and atomically publishes
only sanitized sensor values under `/run/bob-icu-monitoring-status/status.json`.
The unprivileged `bob-icu-client` account receives no server configuration,
client credentials, passkey data, capability grants or database read access.

Recommended first catalog:

- `Host`: uptime, one-minute load, available memory and root-disk use;
- `BOB ICU`: receiver service, SQLite quick-check, SQLite size, public HTTPS
  and certificate days remaining;
- `System services`: SSH and NTP synchronization;
- the independent client heartbeat every 60 seconds.

Install the two scripts under `/usr/local/libexec/bob-icu/`, install the service
and timer under `/etc/systemd/system/`, verify both units with
`systemd-analyze verify`, then enable the timer. Run the snapshot once before
starting the client and verify that it contains only the documented schema and
sensor objects. Snapshot age must be bounded by the sensor reader; stale or
missing snapshots fail closed and produce no fabricated reading.
