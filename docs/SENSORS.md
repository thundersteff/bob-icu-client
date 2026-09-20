# Sensor configuration

The client publishes its complete configured catalog on startup. New services
and sensors are therefore defined on the client and appear on the server
without a separate catalog step.

Identifiers must match `[a-z0-9][a-z0-9._-]{0,62}`. A host may define at most
64 services and each service at most 32 sensors. Intervals are 10–86,400
seconds. A timeout must be shorter than the interval.

## Built-in sensors

| Name | Value | Unit recommendation |
| --- | --- | --- |
| `host.uptime_seconds` | Linux uptime | `seconds` |
| `host.load1` | one-minute load average | `load` |
| `host.memory_available_percent` | available memory | `percent` |
| `host.root_disk_used_percent` | used space on `/` | `percent` |

Example:

```json
{
  "sensor_id": "memory_available_percent",
  "display_name": "Available memory",
  "value_type": "number",
  "unit": "percent",
  "interval_seconds": 60,
  "timeout_seconds": 5,
  "builtin": "host.memory_available_percent"
}
```

## External command contract

Use a JSON array, not a shell command string. The client invokes the executable
directly without a shell and supplies only a minimal `PATH` and `LANG`.

```json
{
  "sensor_id": "example_state",
  "display_name": "Example state",
  "value_type": "text",
  "interval_seconds": 60,
  "timeout_seconds": 10,
  "command": ["/usr/local/libexec/bob-icu/example-sensor", "--json"]
}
```

The command must exit with status `0` and write exactly one JSON object to
stdout:

```json
{"value":"healthy","message":"all checks passed"}
```

For `value_type: "number"`, `value` must be a finite JSON number. For
`value_type: "text"`, it must be a JSON string. Unknown fields, trailing data,
output over 64 KiB, a non-zero exit status and timeouts are rejected.

Sensor failures do not fabricate readings. They are logged locally and counted
in the heartbeat message; the server can independently identify stale sensors
from their expected interval.
