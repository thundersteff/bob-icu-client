# Installation playbook for OpenClaw systems

This document is intended to be handed to an authorized OpenClaw agent. It is
an operational guide, not blanket authorization: the agent must still show the
operator the exact host changes and obtain approval before installation.

## Safety boundary

The client:

- creates one system account, one configuration directory, one state directory
  and one systemd unit;
- opens no inbound port and needs only outbound HTTPS to the configured server;
- does not modify or restart OpenClaw;
- never places the shared secret in Git, logs or command output.

## 1. Inspect and select a release

Repository: <https://github.com/thundersteff/bob-icu-client>

Use the latest non-prerelease GitHub Release unless the operator requested a
specific version. Select the asset matching `uname -m`:

- `x86_64` → `bob-icu-client_VERSION_linux_amd64.tar.gz`
- `aarch64`/`arm64` → `bob-icu-client_VERSION_linux_arm64.tar.gz`

Download the archive, `SHA256SUMS`, `SHA256SUMS.sig` and
`bob-icu-release-signing-public.pem` from the same release into a fresh
temporary directory.

## 2. Verify before extracting

```bash
openssl pkeyutl -verify -pubin \
  -inkey bob-icu-release-signing-public.pem \
  -sigfile SHA256SUMS.sig -rawin -in SHA256SUMS
sha256sum --check --ignore-missing SHA256SUMS
```

Both commands must succeed. Stop on any mismatch. Verify the archive before
extracting it and inspect the archive member names with `tar -tzf`.

## 3. Prepare an exact change proposal

Show the operator this intended delta, adapted to the host:

- system user/group `bob-icu-client` without login shell;
- `/usr/local/bin/bob-icu-client`;
- `/etc/bob-icu/client.json` (`0640`, root:client group);
- `/etc/bob-icu/client.secret` (`0640`, root:client group);
- `/var/lib/bob-icu-client` (`0750`, client user/group);
- `/etc/systemd/system/bob-icu-client.service`;
- enable and start only `bob-icu-client.service`;
- no inbound firewall rule and no OpenClaw restart.

The operator must provide or approve the source ID, ingest origin, credential
handoff and initial sensor catalog. For Linux patients, start from the embedded
`linux.base.v1` and `linux.systemd.v1` packages instead of recreating their
sensors manually; see [`STANDARD-PACKAGES.md`](STANDARD-PACKAGES.md).

## 4. Install after approval

Create the unprivileged account without populating its state directory from
`/etc/skel` (for example with `useradd --system --no-create-home`). Then create
the directories and files with explicit ownership and modes. Copy
`deploy/bob-icu-client.service` from the verified release. Put the secret alone
on one line in `client.secret`; do not interpolate it into the JSON
configuration.

Before enabling the service:

```bash
sudo -u bob-icu-client /usr/local/bin/bob-icu-client \
  -config /etc/bob-icu/client.json -check-config
systemd-analyze verify /etc/systemd/system/bob-icu-client.service
```

Then run `systemctl daemon-reload`, enable/start the unit and verify:

```bash
systemctl is-enabled bob-icu-client.service
systemctl is-active bob-icu-client.service
systemctl status --no-pager bob-icu-client.service
journalctl -u bob-icu-client.service --since=-5min --no-pager
```

Never print the secret. A successful client start is not sufficient proof;
verify on the BOB ICU server that the manifest, heartbeat and readings arrived.
The configuration check must run as the service account: it opens and migrates
the SQLite database and deliberately sets its mode to `0600`.

## 5. Upgrade and rollback

For an upgrade, verify the new release first, stop only this unit, atomically
replace the binary, start it and prove delivery. Preserve the previous binary
until verification succeeds.

Rollback is limited to this client: restore the previous binary/configuration,
run `daemon-reload` if the unit changed, and restart only
`bob-icu-client.service`. Preserve the SQLite state database unless it is the
demonstrated cause of failure.

For full removal, stop/disable the unit and move the unit, binary,
configuration and state directory to a recoverable quarantine location before
`daemon-reload`. No OpenClaw component needs to be changed.
