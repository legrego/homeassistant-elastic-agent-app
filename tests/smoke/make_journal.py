#!/usr/bin/env python3
"""Write a journal export file that looks like a Home Assistant OS host.

Docker's journald log driver writes one journal entry per line of container
output, so a Core traceback arrives as several entries. Core and Supervisor
lines carry ANSI colour codes. Run systemd-journal-remote on the output to get
a real journal file.

Usage: make_journal.py OUTPUT MACHINE_ID
"""

from __future__ import annotations

import struct
import sys
import time
import uuid

ESC = "\x1b"
BOOT_ID = uuid.uuid4().hex


def container(name: str, *lines: str) -> list[dict[str, str]]:
    cid = uuid.uuid4().hex + uuid.uuid4().hex
    return [
        {
            "MESSAGE": line,
            "PRIORITY": "6",
            "CONTAINER_NAME": name,
            "CONTAINER_ID": cid[:12],
            "CONTAINER_ID_FULL": cid,
            "CONTAINER_TAG": cid[:12],
            "SYSLOG_IDENTIFIER": cid[:12],
            "IMAGE_NAME": f"ghcr.io/home-assistant/{name}:test",
            "_TRANSPORT": "journal",
            "_COMM": "dockerd",
            "_SYSTEMD_UNIT": "docker.service",
        }
        for line in lines
    ]


def host_unit(unit: str, ident: str, message: str) -> list[dict[str, str]]:
    return [
        {
            "MESSAGE": message,
            "PRIORITY": "6",
            "SYSLOG_IDENTIFIER": ident,
            "_SYSTEMD_UNIT": unit,
            "_TRANSPORT": "journal",
            "_COMM": ident,
        }
    ]


ENTRIES = [
    *host_unit("NetworkManager.service", "NetworkManager", "device (eth0): state change: activated"),
    *container(
        "hassio_supervisor",
        f"{ESC}[32m2026-11-07 17:30:00.000 INFO (MainThread) [supervisor.core] Supervisor is up and running{ESC}[0m",
    ),
    *container(
        "homeassistant",
        f"{ESC}[32m2026-11-07 17:30:01.123 INFO (MainThread) [homeassistant.bootstrap] Home Assistant initialized in 12.34s{ESC}[0m",
        f"{ESC}[33m2026-11-07 17:30:02.456 WARNING (MainThread) [homeassistant.components.http.ban] Login attempt or request with invalid authentication from 192.0.2.10{ESC}[0m",
        f"{ESC}[31m2026-11-07 17:30:03.789 ERROR (MainThread) [custom_components.elasticsearch.es_publish] Error publishing documents",
        "Traceback (most recent call last):",
        '  File "/config/custom_components/elasticsearch/es_publish.py", line 42, in publish',
        '    raise ConnectionError("cluster unavailable")',
        f"ConnectionError: cluster unavailable{ESC}[0m",
        "2026-11-07 17:30:04.000 INFO (SyncWorker_3) [homeassistant.components.recorder.core] Recorder ready",
    ),
    *container("addon_core_ssh", "[17:30:05] INFO: Starting the SSH daemon..."),
]


def field(key: str, value: str) -> bytes:
    data = value.encode()
    if any(c < 0x20 for c in data):
        # Binary-safe form, required for values with control characters.
        return key.encode() + b"\n" + struct.pack("<Q", len(data)) + data + b"\n"
    return f"{key}={value}\n".encode()


def main() -> None:
    output, machine_id = sys.argv[1], sys.argv[2]
    now = int(time.time() * 1_000_000)
    with open(output, "wb") as out:
        for i, entry in enumerate(ENTRIES):
            usec = now - (len(ENTRIES) - i) * 1_000_000
            out.write(field("__REALTIME_TIMESTAMP", str(usec)))
            out.write(field("__MONOTONIC_TIMESTAMP", str(10_000_000 + i * 1_000_000)))
            out.write(field("_BOOT_ID", BOOT_ID))
            out.write(field("_MACHINE_ID", machine_id))
            out.write(field("_HOSTNAME", "homeassistant"))
            for key, value in entry.items():
                out.write(field(key, value))
            out.write(b"\n")


if __name__ == "__main__":
    main()
