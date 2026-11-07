# Elastic Agent for Home Assistant

A Home Assistant app (formerly called an add-on) that runs a Fleet-managed
[Elastic Agent](https://www.elastic.co/docs/reference/fleet) on Home Assistant
OS and Supervised installs. It reads the host's systemd journal, so it can send
logs from Home Assistant Core, the Supervisor, every app and the host to
Elasticsearch.

It complements the [Elasticsearch integration](https://github.com/legrego/homeassistant-elasticsearch),
which publishes entity state changes.

This is a community project, not an official Elastic product.

## Install

[![Open your Home Assistant instance and add this app repository.](https://my.home-assistant.io/badges/supervisor_add_addon_repository.svg)](https://my.home-assistant.io/redirect/supervisor_add_addon_repository/?repository_url=https%3A%2F%2Fgithub.com%2Flegrego%2Fhomeassistant-elastic-agent-app)

Or go to **Settings > Apps > App store**, open the menu, choose
**Repositories**, and add `https://github.com/legrego/homeassistant-elastic-agent-app`.

Then install **Elastic Agent** and follow its [documentation](elastic_agent/DOCS.md).

## Apps in this repository

| App | Description |
|---|---|
| [Elastic Agent](elastic_agent/) | Ships Home Assistant OS logs to Elasticsearch through Fleet |

## Development

- `elastic_agent/` is the app. Its `Dockerfile` builds on Elastic's
  `elastic-agent-slim` image and adds `journalctl`, which the journald input
  needs.
- `pipelines/` holds the ingest pipeline for Core logs.
- `tests/smoke/smoke.sh` builds the image and tests it against a generated
  journal and a local Elasticsearch. It needs Docker, curl, jq and python3.

```bash
tests/smoke/smoke.sh
```

CI lints the app and builds both architectures on every pull request. Pushes to
`main` publish `ghcr.io/legrego/ha-app-elastic-agent`, tagged with the
`version` in `elastic_agent/config.yaml`. Bump that version and add a
`CHANGELOG.md` entry for every change to the image.

Renovate opens pull requests when Elastic releases a new agent version.

## License

This repository's code is MIT licensed. The Elastic Agent in the published
image is licensed under the [Elastic License 2.0](https://www.elastic.co/licensing/elastic-license).
