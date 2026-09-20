# OpenClaw WhatsApp integration

The BOB ICU client must not receive an OpenClaw Gateway token or direct access
to WhatsApp credentials. This integration therefore uses a narrow, sanitized
status bridge:

1. a systemd oneshot runs `openclaw channels status --channel whatsapp --json`
   under the existing OpenClaw host account;
2. the bridge keeps only account identity and connection-health fields;
3. the resulting snapshot is world-readable but contains no chats, allowlists,
   credentials, tokens or message metadata;
4. command sensors read one configured account each and reject snapshots older
   than the configured maximum age.

The timer is independent of cron and does not restart or reconfigure the
OpenClaw Gateway. Account mappings remain local in
`/etc/bob-icu-openclaw/whatsapp-accounts.json`; do not commit real phone
numbers to this public repository.

Example sensor:

```json
{
  "sensor_id": "account_491234567890",
  "display_name": "+49 123 4567890",
  "value_type": "text",
  "interval_seconds": 60,
  "timeout_seconds": 10,
  "command": [
    "/usr/local/libexec/bob-icu/whatsapp-account-sensor",
    "/run/bob-icu-openclaw-status/whatsapp.json",
    "default",
    "120"
  ]
}
```

Possible values are `healthy`, `degraded`, `disconnected` and `disabled`.
Failure or a stale snapshot produces no fabricated reading; the server can
therefore mark the sensor stale using its expected interval.
