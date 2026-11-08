# Elastic Agent

This app runs [Elastic Agent](https://www.elastic.co/docs/reference/fleet) on
your Home Assistant host and sends its logs to Elasticsearch.

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

The app runs in one of two modes:

- **Standalone** (recommended): give it your Elasticsearch URL and a login,
  and it sets up everything it needs. You don't need Fleet or Kibana.
- **Fleet**: the agent enrolls with your Fleet Server, and you manage what it
  collects from Kibana. See [Fleet mode](#fleet-mode).

## Requirements

- Home Assistant OS or a Supervised install. Apps need the Supervisor. On
  Container or Core installs, run Elastic Agent yourself instead.
- A 64-bit host (`amd64` or `aarch64`). Elastic doesn't publish 32-bit images.
- Elasticsearch 8.14 or newer, including 9.x, Elastic Cloud and Serverless.
  For Fleet mode, Fleet Server must be the same version as the bundled agent
  (9.5.6) or newer.
- Room for the image (about 300 MB download, 1 GB on disk) and for the agent's
  memory use, which is usually a few hundred MB. On a Raspberry Pi with 2 GB of
  RAM, keep an eye on memory after you start it.

## Quick start

1. Find your Elasticsearch URL. On Elastic Cloud, copy the **Elasticsearch**
   endpoint from your deployment or project page. It looks like
   `https://my-deployment.es.us-east-1.aws.elastic.cloud:443`.
2. In the app's **Configuration** tab, set `elasticsearch_url`, and either:
   - `username` and `password` of a user that can set things up, such as the
     `elastic` superuser, or
   - `api_key`, on Serverless projects or if you'd rather not share a
     superuser. See [Using an API key](#using-an-api-key).
3. Start the app and check its **Log** tab. You should see lines like:

   ```text
   [ha-elasticsearch-setup] Connected to Elasticsearch 9.5.6 at https://...
   [ha-elasticsearch-setup] Installed version 1 of the logs-haos.core@custom ingest pipeline.
   [ha-elasticsearch-setup] Created API key ..., which can only write logs-haos.* data streams.
   ```

4. If you gave a username and password, remove them from the configuration
   and save. The app keeps sending logs with the API key it created.
5. In Kibana, open **Discover** and pick the `logs-haos.*` data streams (create
   a data view for `logs-haos.*` if Discover asks for one).

Example:

```yaml
elasticsearch_url: https://my-deployment.es.us-east-1.aws.elastic.cloud:443
username: elastic
password: "<password>"
```

## What the app sets up

With a username and password, on every start the app:

- **Installs the Core log pipeline.** The `logs-haos.core@custom` ingest
  pipeline turns Core's log lines into fields (see [Core log
  fields](#core-log-fields)). The app updates it when a new version of the app
  ships a new version of the pipeline. If you created a pipeline with that name
  yourself, the app leaves it alone.
- **Creates an API key for sending logs**, the first time. The key can only
  write to `logs-haos.*` data streams, and can't read log data. It also has
  `monitor` and `read_pipeline` cluster privileges so startup can check the
  connection and verify the Core pipeline without keeping the password. The app
  saves it in its `/data` folder and reuses it afterwards. It's named
  `Home Assistant Elastic Agent (<host name>)`, under **Stack Management > API
  keys**. If you delete or invalidate the key, add the username and password
  again so the app can create a new one.
- **Writes the agent's configuration** from the app options, and starts the
  agent.

The username and password are only used for setup. The agent never sends data
with them.

Logs go to two data streams:

| Data stream | Contents |
|---|---|
| `logs-haos.core-default` | Home Assistant Core, one event per log record, tracebacks included |
| `logs-haos.system-default` | Supervisor, apps and host, one event per line |

Elasticsearch's built-in `logs` index template covers both, so there's nothing
else to install.

### Using an API key

Give the app an API key instead of a username and password when your project
has no superuser login (Serverless), or when you'd rather not share one. The app
sends logs with the key you give it. To also let it install the Core log
pipeline, include `manage_pipeline`. In Kibana, go to **Dev Tools** and run:

```text
POST /_security/api_key
{
  "name": "home-assistant-elastic-agent",
  "role_descriptors": {
    "home_assistant": {
      "cluster": ["monitor", "manage_pipeline"],
      "indices": [
        {
          "names": ["logs-haos.*-*"],
          "privileges": ["auto_configure", "create_doc"]
        }
      ]
    }
  }
}
```

Copy the `encoded` value from the response into `api_key`. The app also accepts
the `id:api_key` form. If someone else installs the pipeline, `read_pipeline`
can replace `manage_pipeline` so the app can verify and use it without changing
it. If startup can't verify that the pipeline exists (including when Elasticsearch
is unreachable), the app leaves it out of the agent's configuration and sends
Core logs unparsed. Restart the app after installing the pipeline or fixing
access to enable parsing. Previously sent logs aren't parsed retroactively.

## Configuration

| Option | Description |
|---|---|
| `elasticsearch_url` | Elasticsearch URL, for standalone mode |
| `username`, `password` | A user that can set things up, such as `elastic`. Used only to install the pipeline and create the app's API key. Remove them after the first start. |
| `api_key` | Instead of a username and password. The agent sends logs with this key. |
| `collect_core` | Send Home Assistant Core logs. On by default. |
| `collect_supervisor` | Send Supervisor logs, including its plugins (DNS, audio, CLI, multicast, observer). On by default. |
| `collect_apps` | Send logs from every other app. On by default. |
| `collect_host` | Send host OS logs. On by default. |
| `read_existing_logs` | On the first start, also send the logs already in the journal. Off by default, so only new logs are sent. |
| `ca_certificate` | CA for the Elasticsearch (or Fleet Server) certificate, as PEM text or a file name in `/ssl` |
| `insecure` | Skip certificate verification. Test clusters only. |
| `tags` | Tags to add to every event (in Fleet mode, tags to show on the agent) |
| `fleet_url`, `enrollment_token` | Fleet mode. Leave empty for standalone mode. Hidden until you show unused optional options. |

The agent remembers where it stopped reading the journal, so restarts and
updates don't send logs twice, and logs written while it was stopped are sent
when it starts again.

## Core log fields

Core writes lines like:

```text
2026-11-07 17:30:03.789 ERROR (MainThread) [custom_components.elasticsearch.es_publish] Error publishing documents
```

Its Docker container writes one journal entry per line of output, so the agent
joins every line that doesn't start with a timestamp onto the line before it,
and a traceback stays with its log record. The `logs-haos.core@custom` pipeline
removes the colour codes and turns the line into fields:

| Field | Example |
|---|---|
| `log.level` | `error` |
| `log.logger` | `custom_components.elasticsearch.es_publish` |
| `process.thread.name` | `MainThread` |
| `homeassistant.integration` | `elasticsearch` |
| `message` | `Error publishing documents` |
| `error.type`, `error.message`, `error.stack_trace` | from a traceback, when there is one |

The event time stays the journal's time, which is more precise than the time in
the line. Lines the pipeline can't parse are kept as they are and tagged
`haos_core_unparsed`.

## Fleet mode

In Fleet mode the app only enrolls the agent, and everything it collects comes
from its policy in Fleet. Use it if you already run Fleet and want to manage the
agent there. Leave `elasticsearch_url` empty, and show the unused optional
options to set `fleet_url` and `enrollment_token`.

Do these steps in Kibana before you start the app.

1. **Create an agent policy.** Go to **Fleet > Agent policies > Create agent
   policy**. Name it something like `Home Assistant`, and clear **Collect system
   logs and metrics**. The System integration would report on the app's
   container, not on your host.
2. **Add the Core logs integration.** Add **Custom Journald logs** to the
   policy with the integration name `haos-core`, the include match
   `CONTAINER_NAME=homeassistant`, and, under **Advanced options**, the dataset
   name `haos.core` and this parser:

   ```yaml
   - multiline:
       type: pattern
       pattern: '^(\x1b\[[0-9;]*m)?\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2}'
       negate: true
       match: after
   ```

   The parser only works when the input reads Core alone, which is why Core gets
   its own integration.
3. **Add the logs integration for everything else.** Add **Custom Journald
   logs** a second time, with the integration name `haos-system`, no include
   matches, and, under **Advanced options**, the dataset name `haos.system` and
   this processor, which drops Core's lines:

   ```yaml
   - drop_event:
       when:
         equals:
           container.name: homeassistant
   ```

   To collect less, add include matches instead. Use
   `CONTAINER_NAME=hassio_supervisor` for the Supervisor,
   `CONTAINER_NAME=addon_<repository>_<slug>` for one app (for example
   `addon_core_ssh`), or `_SYSTEMD_UNIT=<unit>` for a host service. On the first
   start the integrations begin at the end of the journal. To also send the logs
   already in it, set **Seek** to `head` before you start the app.
4. **Add the ingest pipeline for Core logs.** In **Dev Tools**, run `PUT
   _ingest/pipeline/logs-haos.core@custom` with the contents of
   [`logs-haos.core@custom.json`](https://github.com/legrego/homeassistant-elastic-agent-app/blob/main/elastic_agent/rootfs/usr/share/ha-elastic-agent/logs-haos.core@custom.json)
   as the body. Fleet runs a pipeline with this name for every document in the
   `haos.core` dataset.
5. **Copy the enrollment details.** Go to **Fleet > Enrollment tokens** and copy
   the secret for your policy into `enrollment_token`. Find the Fleet Server URL
   under **Fleet > Settings > Fleet Server hosts**, and put it in `fleet_url`.

`ca_certificate` only covers the connection to Fleet Server. If Elasticsearch
also uses a private CA, add it to the output under **Fleet > Settings >
Outputs**.

Start the app, then check its **Log** tab. The agent should appear in Fleet
within a minute, named after your Home Assistant host.

On first start the agent enrolls with Fleet and saves its state in the app's
`/data` folder. Later starts and app updates reuse that state, so the agent
keeps its identity. If you change `fleet_url`, `enrollment_token` or
`insecure`, the app moves the old state to `/data/state.previous` and enrolls a
new agent. Unenroll the old one in Fleet. When turning off `insecure`, configure
a trusted CA first if Fleet Server uses a private certificate; otherwise
enrollment fails until trust is configured.

Fleet's **Upgrade agent** action doesn't work for agents in containers. Update
the app to upgrade the agent.

## Switching modes

Switching from Fleet to standalone mode, or back, starts the agent with fresh
state. The old state moves to `/data/state.previous`. When leaving Fleet,
unenroll the old agent there. In standalone mode, the first start after a switch
follows `read_existing_logs`, so you may get some logs twice.

## How it works

- **Host identity.** The agent reports the Home Assistant host name (from the
  Supervisor) and the host's machine ID as `host.name` and `host.id`, not the
  container's.
- **Upgrades.** The agent version comes from the app version. Update the app to
  upgrade the agent.
- **Backups.** Home Assistant backups include the agent's state and the app's
  API key, so a restored agent carries on where it stopped. The agent's own log
  files are left out. Backups also include the app's configuration, which is one
  more reason to remove the password after the first start.
- **The agent's own logs** are in the app's **Log** tab. In standalone mode the
  agent doesn't send its own logs and metrics to Elasticsearch, so its API key
  can stay limited to `logs-haos.*`.

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
startup and crash logs, the integration if you want its structured records. To
keep the app for everything else, turn off `collect_core`.

## Limitations

- No host metrics yet. Those need permissions that lower the security rating.
- Only what's still in the journal can be collected. If the agent is stopped
  long enough for Home Assistant OS to rotate the journal, those logs are lost.
- In standalone mode the app always writes to the `default` namespace.

## License

The app's own files are MIT licensed. The bundled Elastic Agent is licensed
under the [Elastic License 2.0](https://www.elastic.co/licensing/elastic-license).
