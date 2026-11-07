# Elastic Agent

This app runs [Elastic Agent](https://www.elastic.co/docs/reference/fleet) on
your Home Assistant host and sends its logs to Elasticsearch. You manage the
agent from Fleet in Kibana. The app only enrolls it.

The app reads the host's systemd journal. On Home Assistant OS every container
logs there, so one agent can collect:

- Home Assistant Core logs, including startup and crashes
- Supervisor logs
- Logs from every other app
- Host logs (network, Docker, the OS itself)

It complements the [Elasticsearch integration](https://github.com/legrego/homeassistant-elasticsearch),
which publishes entity state changes. The integration can't see logs from
before it loads, from crashes, or from outside Core. This app can.

This is a community project, not an official Elastic product.

## Requirements

- Home Assistant OS or a Supervised install. Apps need the Supervisor. On
  Container or Core installs, run Elastic Agent yourself instead.
- A 64-bit host (`amd64` or `aarch64`). Elastic doesn't publish 32-bit images.
- An Elastic deployment with Fleet. Fleet Server must be the same version as
  the bundled agent (9.5.6) or newer. Serverless projects always meet this. On
  Elastic Cloud Hosted or a self-managed stack, check your version.
- Room for the image (about 300 MB download, 1 GB on disk) and for the agent's
  memory use, which is usually a few hundred MB. On a Raspberry Pi with 2 GB of
  RAM, keep an eye on memory after you start it.

## Set up Fleet

Do these steps in Kibana before you start the app.

### 1. Create an agent policy

Go to **Fleet > Agent policies > Create agent policy**. Name it something like
`Home Assistant`, and clear **Collect system logs and metrics**. The System
integration would report on the app's container, not on your host.

### 2. Add the Core logs integration

Add the **Custom Journald logs** integration to the policy with these settings:

| Setting | Value |
|---|---|
| Integration name | `haos-core` |
| Include matches | `CONTAINER_NAME=homeassistant` |
| Dataset name (under Advanced options) | `haos.core` |
| Parsers (under Advanced options) | see below |

Core's Docker container writes one journal entry per line of output, so a
traceback arrives as many entries. This parser joins every line that doesn't
start with a timestamp onto the line before it:

```yaml
- multiline:
    type: pattern
    pattern: '^(\x1b\[[0-9;]*m)?\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2}'
    negate: true
    match: after
```

The parser only works when the input reads Core alone, which is why Core gets
its own integration.

### 3. Add the logs integration for everything else

Add **Custom Journald logs** to the same policy a second time:

| Setting | Value |
|---|---|
| Integration name | `haos-system` |
| Include matches | leave empty |
| Dataset name (under Advanced options) | `haos.system` |
| Processors (under Advanced options) | see below |

The processor drops Core's lines, which the first integration already sends:

```yaml
- drop_event:
    when:
      equals:
        container.name: homeassistant
```

To collect less, add include matches instead. Use `CONTAINER_NAME=hassio_supervisor`
for the Supervisor, `CONTAINER_NAME=addon_<repository>_<slug>` for one app (for
example `addon_core_ssh`), or `_SYSTEMD_UNIT=<unit>` for a host service.

On the first start the integrations begin at the end of the journal. To also
send the logs already in it, set **Seek** to `head` before you start the app.

### 4. Add the ingest pipeline for Core logs

Core writes lines like:

```text
2026-11-07 17:30:03.789 ERROR (MainThread) [custom_components.elasticsearch.es_publish] Error publishing documents
```

The pipeline in [`pipelines/logs-haos.core@custom.json`](https://github.com/legrego/homeassistant-elastic-agent-app/blob/main/pipelines/logs-haos.core@custom.json)
removes the colour codes and turns those lines into fields:

| Field | Example |
|---|---|
| `log.level` | `error` |
| `log.logger` | `custom_components.elasticsearch.es_publish` |
| `process.thread.name` | `MainThread` |
| `homeassistant.integration` | `elasticsearch` |
| `message` | `Error publishing documents` |
| `error.type`, `error.message`, `error.stack_trace` | from a traceback, when there is one |

In **Dev Tools**, run `PUT _ingest/pipeline/logs-haos.core@custom` with the
file's contents as the body. Fleet runs a pipeline with this name for every
document in the `haos.core` dataset. The event time stays the journal's time,
which is more precise than the time in the line.

### 5. Copy the enrollment details

Go to **Fleet > Enrollment tokens** and copy the secret for your policy. Find
the Fleet Server URL under **Fleet > Settings > Fleet Server hosts**.

## Configuration

| Option | Required | Description |
|---|---|---|
| `fleet_url` | Yes | Fleet Server URL, for example `https://fleet.example.com:443` |
| `enrollment_token` | Yes | Enrollment token for the policy |
| `ca_certificate` | No | CA for the Fleet Server certificate, as PEM text or a file name in `/ssl` |
| `insecure` | No | Skip Fleet Server certificate verification. Test clusters only. |
| `tags` | No | Tags to show on the agent in Fleet |

`ca_certificate` only covers the connection to Fleet Server. If Elasticsearch
also uses a private CA, add it to the output under **Fleet > Settings >
Outputs**.

Example:

```yaml
fleet_url: https://fleet.example.com:443
enrollment_token: "<token>"
tags:
  - home-assistant
```

Start the app, then check its **Log** tab. The agent should appear in Fleet
within a minute, named after your Home Assistant host.

## How it works

- **Enrollment.** On first start the agent enrolls with Fleet and saves its
  state in the app's `/data` folder. Later starts and app updates reuse that
  state, so the agent keeps its identity. If you change `fleet_url`,
  `enrollment_token` or `insecure`, the app moves the old state to
  `/data/state.previous` and enrolls a new agent. Unenroll the old one in Fleet.
  When turning off `insecure`, configure a trusted CA first if Fleet Server
  uses a private certificate; otherwise enrollment fails until trust is configured.
- **Policy.** Everything the agent collects comes from its policy in Fleet. The
  app doesn't collect anything on its own.
- **Host identity.** The agent reports the Home Assistant host name (from the
  Supervisor) and the host's machine ID as `host.name` and `host.id`, not the
  container's.
- **Upgrades.** The agent version comes from the app version. Update the app to
  upgrade the agent. Fleet's **Upgrade agent** action doesn't work for agents
  in containers.
- **Backups.** Home Assistant backups include the agent's state, so a restored
  agent keeps its identity. The agent's own log files are left out.

## Permissions

The app asks for:

- Read-only access to the host journal (`journald`).
- The Supervisor API with the default role (`hassio_api`), only to read the
  host name.
- Read-only access to the `/ssl` folder, for `ca_certificate`.

The agent runs as root inside its container, because the journal files can only
be read by root. It has no extra capabilities, host networking or Docker
access, and these permissions don't lower the app's security rating.

## Using it with the Elasticsearch integration

The integration writes entity states to `metrics-homeassistant.*`, and plans
to send Core logs to `logs-homeassistant.*`. This app writes to
`logs-haos.core-*` and `logs-haos.system-*`, so the two never write to the same
data streams.

If you turn on log sending in the integration as well, you'll get Core's logs
twice, in two shapes. Pick one source for Core logs: this app if you want
startup and crash logs, the integration if you want its structured records.

## Limitations

- Fleet-managed only. There's no standalone mode.
- No host metrics yet. Those need permissions that lower the security rating.
- Only what's still in the journal can be collected. If the agent is stopped
  long enough for Home Assistant OS to rotate the journal, those logs are lost.

## License

The app's own files are MIT licensed. The bundled Elastic Agent is licensed
under the [Elastic License 2.0](https://www.elastic.co/licensing/elastic-license).
