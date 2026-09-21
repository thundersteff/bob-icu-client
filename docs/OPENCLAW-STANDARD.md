# OpenClaw standard package

`openclaw.standard.v1` monitors Core, LLM, channels, automations and memory.
The read-only bridge runs as the authorized OpenClaw host account and writes a
sanitized snapshot. BOB ICU receives no Gateway/model/channel credentials,
prompts, answers, chats, allowlists, session identifiers or job payloads.

```json
{
  "packages": ["openclaw.standard.v1"],
  "openclaw_accounts": [
    {"channel":"whatsapp", "account_id":"default", "display_name":"WhatsApp · Hauptkonto"}
  ],
  "openclaw_models": [
    {"model_id":"provider/primary-model", "display_name":"Primäres LLM", "active_probe":true}
  ]
}
```

Install the helpers and systemd units from `integrations/openclaw`. Configure
`/etc/bob-icu-openclaw/model-probe.env` (mode `0600`) with `OPENCLAW_AGENT` and
`OPENCLAW_MODEL`. The status snapshot runs every minute. The minimal real model
probe runs every 15 minutes and consumes provider quota. After 20 minutes
without a fresh result it becomes warning.

Channel sensors prove OpenClaw's local linked/running/connected state. True
end-to-end delivery requires an independent off-host sentinel.
