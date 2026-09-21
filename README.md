# BOB ICU Client

**BOB ICU – Infrastructure Care Unit**  
*Erkennt. Diagnostiziert. Heilt.*

`bob-icu-client` is the lightweight Linux patient agent for BOB ICU. It runs
configured sensors on its own scheduler, publishes their manifest and values to
the BOB ICU ingest API, and sends an independent heartbeat every 60 seconds.

## Design

- one statically linked Go binary
- internal interval scheduler; no cron dependency
- embedded, versioned standard packages with stable names and sensor IDs
- built-in host sensors and strictly defined external command sensors
- durable SQLite/WAL outbox for temporary network or server outages
- HTTPS plus HMAC-SHA256 request authentication
- client-defined services and sensors; no manual server-side catalog setup
- systemd readiness and watchdog support
- no inbound listener and no inbound firewall rule

The server remains authoritative for thresholds, state evaluation, alerting and
future repair actions. The client only collects and transports observations.

## Quick start

The supported installation path is documented in
[`docs/INSTALL-OPENCLAW.md`](docs/INSTALL-OPENCLAW.md). Sensor definitions and
the external-command contract are documented in
[`docs/SENSORS.md`](docs/SENSORS.md).

Linux patients should use the versioned standard packages documented in
[`docs/STANDARD-PACKAGES.md`](docs/STANDARD-PACKAGES.md). They provide one
consistent catalog across installations without downloading mutable sensor
definitions at runtime.

The optional, privilege-separated OpenClaw WhatsApp health integration is
documented in [`docs/OPENCLAW-WHATSAPP.md`](docs/OPENCLAW-WHATSAPP.md).

The privilege-separated integration for enrolling the BOB ICU monitoring VPS
itself is documented in
[`docs/MONITORING-SERVER.md`](docs/MONITORING-SERVER.md).

Release binaries are published for Linux `amd64` and `arm64`. Every release
contains SHA-256 checksums and an Ed25519 signature over the checksum file.

## Default limits

- 64 services per host
- 32 sensors per service
- 10,000 queued payloads
- 4 parallel sensor executions

The matching BOB ICU server accepts at most 64 registered hosts. These defaults
are deliberately conservative and configurable where appropriate.

## Development

```bash
go test -race ./...
go vet ./...
go build ./cmd/bob-icu-client
```

## License

MIT — see [`LICENSE`](LICENSE).
