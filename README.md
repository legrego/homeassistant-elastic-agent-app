# Elastic Agent for Home Assistant

A Home Assistant app (formerly called an add-on) that runs
[Elastic Agent](https://www.elastic.co/docs/reference/fleet) on Home Assistant
OS and Supervised installs. It reads the host's systemd journal, so it can send
logs from Home Assistant Core, the Supervisor, every app and the host to
Elasticsearch.

Give it your Elasticsearch URL and a superuser login, and it sets up the rest:
the ingest pipeline for Core logs, a write-only API key, and the agent's
configuration. It can also enroll with Fleet instead.

A status page in Home Assistant shows whether the agent is healthy and whether
logs are getting through to Elasticsearch or Fleet.

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
| [Elastic Agent](elastic_agent/) | Ships Home Assistant OS logs to Elasticsearch |

## Development

- `elastic_agent/` is the app. Its `Dockerfile` builds on Elastic's
  `elastic-agent-slim` image and adds `journalctl`, which the journald input
  needs.
- `elastic_agent/rootfs/usr/local/bin/ha-entrypoint` keeps the web UI running
  independently of agent startup. `ha-agent-start` reads the options and starts
  the agent in standalone or Fleet mode. `ha-elasticsearch-setup`
  prepares Elasticsearch for standalone mode.
- `elastic_agent/status/` is the status page served through ingress: a small
  Go server (standard library only) with the page embedded. The Dockerfile
  builds it in its own stage. Run its tests with
  `cd elastic_agent/status && go test ./...`.
- `elastic_agent/rootfs/usr/share/ha-elastic-agent/` holds the ingest pipeline
  for Core logs and `agent-config.jq`, which builds the standalone agent's
  configuration from the options.
- `tests/smoke/smoke.sh` builds the image and tests both modes against a
  generated journal, a local Elasticsearch with security on and a fake
  Supervisor, including the status page. It needs Docker, curl, jq and
  python3.

```bash
tests/smoke/smoke.sh
```

CI lints the app and builds both architectures on every pull request. Pushes to
`main` publish `ghcr.io/legrego/ha-app-elastic-agent`, tagged with the
`version` in `elastic_agent/config.yaml`. Bump that version and add a
`CHANGELOG.md` entry for every change to the image. Once a new version's images
are published, CI creates a `v<version>` GitHub release with that version's
changelog section as the release notes. Lint fails if the changelog has no
section for the current version.

Renovate opens pull requests when Elastic releases a new agent version.

## License

This repository's code is MIT licensed. The Elastic Agent in the published
image is licensed under the [Elastic License 2.0](https://www.elastic.co/licensing/elastic-license).
