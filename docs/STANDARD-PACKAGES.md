# Versioned standard packages

Standard packages give every BOB ICU patient the same service IDs, sensor IDs,
display names, units and collection intervals. Package definitions are embedded
in the signed client binary. A patient never downloads mutable executable sensor
code at runtime.

Enable packages in `client.json`:

```json
{
  "packages": [
    "linux.base.v1",
    "linux.systemd.v1"
  ],
  "services": []
}
```

Explicit `services` may be added for patient-specific monitoring. They must not
reuse a service ID owned by a package. Unknown or duplicate package IDs and
service conflicts fail closed during configuration validation.

List the packages embedded in an installed binary:

```bash
bob-icu-client -list-packages
```

Package IDs are immutable contracts. A materially changed catalog is released
under a new ID such as `linux.base.v2`; an existing `v1` patient is never
silently reinterpreted.

## `linux.base.v1`

### Service `linux_system` — Linux · System

| Sensor ID | Display name | Type / unit | Interval |
| --- | --- | --- | --- |
| `uptime_seconds` | Betriebszeit | number / seconds | 60 s |
| `cpu_used_percent` | CPU-Auslastung | number / percent | 60 s |
| `load_1` | Systemlast · 1 Minute | number / load | 60 s |
| `load_5` | Systemlast · 5 Minuten | number / load | 60 s |
| `load_15` | Systemlast · 15 Minuten | number / load | 60 s |
| `memory_used_percent` | Arbeitsspeicher belegt | number / percent | 60 s |
| `memory_available_bytes` | Arbeitsspeicher verfügbar | number / bytes | 60 s |
| `swap_used_percent` | Swap belegt | number / percent | 60 s |

### Service `linux_storage` — Linux · Speicher

| Sensor ID | Display name | Type / unit | Interval |
| --- | --- | --- | --- |
| `root_used_percent` | Root-Dateisystem belegt | number / percent | 300 s |
| `root_free_bytes` | Root-Dateisystem frei | number / bytes | 300 s |
| `root_inode_used_percent` | Root-Inodes belegt | number / percent | 300 s |
| `root_mount_state` | Root-Dateisystem Zustand | text | 300 s |

## `linux.systemd.v1`

### Service `linux_services` — Linux · Dienste

| Sensor ID | Display name | Type / unit | Interval |
| --- | --- | --- | --- |
| `systemd_state` | systemd-Zustand | text | 60 s |
| `systemd_failed_units_count` | Fehlerhafte systemd-Units | number / count | 60 s |
| `cron_daemon_state` | Cron-Dienst | text | 300 s |
| `time_sync_state` | Zeitsynchronisation | text | 300 s |
| `reboot_required_state` | Neustartbedarf | text | 3600 s |

## Cron scope

`cron_daemon_state` proves whether the system's `cron.service` or
`crond.service` is active. It cannot prove that every scheduled command ran
successfully. Critical jobs need an application-specific last-success sensor or
heartbeat. That separate convention will be introduced as its own versioned
package instead of overloading the daemon check.

## Compatibility

The four legacy `host.*` built-ins remain available for existing installations.
They are not part of the standard package contract and should not be used for
new patients.
