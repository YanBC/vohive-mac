# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

A single-binary Go web app that manages a BAIWANG/Quectel-style 4G USB dongle (QDC507, USB ID `2ca3:4006`) on macOS **entirely from userspace over raw USB** via libusb/gousb — macOS has no driver for the dongle's serial interfaces, so AT commands go over USB bulk transfers directly. Features: modem status, SMS send/archive, and cellular data-usage tracking, served as a web console with an embedded UI.

## Commands

```sh
go build -o vohive-mac .        # build (requires: brew install libusb)
./vohive-mac                    # run server on http://127.0.0.1:7676
./vohive-mac at 'AT+CSQ'        # ad-hoc AT command console (stop the server first)
go vet ./...                    # no tests exist; vet is the only check
```

There is no test suite. Testing changes generally requires the physical dongle plugged in; the `at` subcommand is the quickest way to poke the modem.

## Architecture

All code is `package main` in the repo root. The data flow:

- **`modem.go`** — the only component that touches USB. One `Modem` owns the claimed AT interface for the whole process; every AT exchange is serialized behind `Modem.mu` (`cmdLocked` and friends). Connection is lazy and self-healing: any USB error calls `disconnectLocked()` so the next command reconnects. SMS **sending** uses text mode + UCS2 hex; SMS **reading** uses PDU mode. Device identity is overridable via `BAIWANG_VID`/`BAIWANG_PID`/`BAIWANG_IFACE` env vars.
- **`pdu.go`** — standalone SMS-DELIVER PDU decoder (GSM7/UCS2/8-bit, UDH parsing for concatenated-message ref/seq/total). Pure functions; the one place unit tests could easily be added.
- **`ingest.go`** — `Archiver` drains modem storages ("SM" SIM, "ME" flash) into SQLite every 60 s, reassembles concatenated messages (waits up to 24 h `partGracePeriod` for missing parts), then deletes archived messages from hardware (unless `-archive-delete=false`) so the tiny slot pools never fill and bounce messages. Order matters: a message is deleted only after `ArchiveInbound` succeeds.
- **`db.go`** — `Store` over SQLite (`data/vohive.db`, WAL, `SetMaxOpenConns(1)`). Inbound dedup via a content hash in a `UNIQUE` column, so re-listing undeleted hardware messages is idempotent. Legacy `data/usage.json` is imported once at startup and renamed.
- **`traffic.go`** — data usage is *not* read from the modem: it samples macOS interface byte counters (`netstat -ibn`) for the dongle's ECM interface (found via `ioreg -r -n Baiwang`). Handles counter resets and interface renames on replug. Daily totals write-through to the store every 30 s. The snapshot's `up` field is the *real* link state (`ifconfig` "status: active"), not mere interface presence — the ECM link can be down while the interface still exists.
- **`recovery.go`** — ECM link watchdog. The dongle's firmware drops the ECM Ethernet link on USB suspend (lid close) and never re-asserts it after resume, leaving macOS with a dead data path while the AT port still works. The watchdog detects link-down ≥ 10 s with the AT port still answering and issues a USB port reset (`Modem.ResetUSB`) to force re-enumeration; rate-limited to one reset per 60 s. Down-time must be observed while continuously awake: a wall-clock jump between ticks means the machine slept, which clears the timer and imposes a 15 s settle — resetting while the USB stack is still resuming can knock the dongle off the bus until physically replugged.
- **`server.go`** — HTTP API (`/api/status`, `/api/traffic`, `/api/data`, `/api/sms/inbox`, `/api/sms/send`) + embedded `static/` UI. Status is cached 5 s to throttle AT-port polling; the inbox handler triggers `SyncIfStale(30s)` before reading the DB.
- **`main.go`** — wiring + the `at` CLI subcommand.

## Constraints to keep in mind

- **The AT interface is exclusive**: only one process can claim it. The server and `vohive-mac at` cannot run simultaneously, and within the process everything must go through the single `Modem` and its mutex.
- The UI in `static/` is a single self-contained HTML file embedded via `go:embed` — rebuilding the binary is required to pick up UI changes.
- No authentication on the HTTP API; it binds to `127.0.0.1` by default deliberately.
- `docs/dongle-setup.md` documents the one-time `AT+QCFG="usbnet",1` switch to CDC ECM mode that makes the dongle usable on macOS at all — relevant when debugging "dongle not found" or interface-numbering issues (interface layout differs by USB mode; AT is interface 3).
