# Changelog

## 0.1.0

- First release, marked experimental.
- Bundles Elastic Agent 9.5.6 (slim image, with `journalctl` added).
- Enrolls with Fleet on first start and keeps its state in `/data`.
- Re-enrolls when the Fleet URL, enrollment token or certificate verification
  setting changes, so the new settings take effect.
- Reads the host journal, so it can collect logs from Home Assistant Core, the
  Supervisor, every app and the host.
