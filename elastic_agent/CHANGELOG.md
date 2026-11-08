# Changelog

## 0.2.0

- New standalone mode: set `elasticsearch_url` and a username and password
  (such as the `elastic` superuser), and the app sets up the rest. It installs
  the Core log pipeline, creates an API key that can only write `logs-haos.*`,
  and configures the agent. Fleet and Kibana aren't needed. The password can be
  removed after the first start.
- `api_key` can be given instead of a username and password, for Serverless
  projects.
- Core logs are sent unparsed if startup can't verify the ingest pipeline,
  preventing missing pipelines from rejecting logs. The app-created key has
  read-only pipeline access so verification still works after removing the password.
- New options to choose which logs to send (`collect_core`,
  `collect_supervisor`, `collect_apps`, `collect_host`) and whether to send the
  logs already in the journal on the first start (`read_existing_logs`).
- Fleet mode works as before. `fleet_url` and `enrollment_token` are now
  optional, and are hidden until you show unused optional options.
- `ca_certificate` and `insecure` also apply to Elasticsearch in standalone
  mode.
- The Core ingest pipeline moved into the app, at
  `elastic_agent/rootfs/usr/share/ha-elastic-agent/logs-haos.core@custom.json`.

## 0.1.0

- First release, marked experimental.
- Bundles Elastic Agent 9.5.6 (slim image, with `journalctl` added).
- Enrolls with Fleet on first start and keeps its state in `/data`.
- Re-enrolls when the Fleet URL, enrollment token or certificate verification
  setting changes, so the new settings take effect.
- Reads the host journal, so it can collect logs from Home Assistant Core, the
  Supervisor, every app and the host.
