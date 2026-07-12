# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

A single-binary Go web app that manages a BAIWANG/Quectel-style 4G USB dongle (QDC507, USB ID `2ca3:4006`) on macOS **entirely from userspace over raw USB** via libusb/gousb — macOS has no driver for the dongle's serial interfaces, so AT commands go over USB bulk transfers directly. Features: modem status, SMS send/archive, and cellular data-usage tracking, served as a web console with an embedded UI.

## Commands

```sh
go build -o vohive-mac .        # build (requires: brew install libusb)
./vohive-mac                    # run server on http://127.0.0.1:7676
./vohive-mac at 'AT+CSQ'        # ad-hoc AT command console (stop the server first)
go vet ./...
go test ./...                   # db_test.go only: schema migration + SIM attribution
```

The only tests are `db_test.go` (the schema migration and per-SIM attribution paths, which are the parts that can silently corrupt history). Everything else generally requires the physical dongle plugged in; the `at` subcommand is the quickest way to poke the modem.

## Architecture

All code is `package main` in the repo root. The data flow:

- **`modem.go`** — the only component that touches USB. One `Modem` owns the claimed AT interface for the whole process; every AT exchange is serialized behind `Modem.mu` (`cmdLocked` and friends). Connection is lazy and self-healing: any USB error calls `disconnectLocked()` so the next command reconnects. SMS **sending** uses text mode + UCS2 hex; SMS **reading** uses PDU mode. Device identity is overridable via `BAIWANG_VID`/`BAIWANG_PID`/`BAIWANG_IFACE` env vars. Hardware identity (model, IMEI) is cached once; **SIM** identity (`SIMIdentity`: ICCID/IMSI/MSISDN/operator) is *not* — the card can be swapped, so it is re-validated against a cheap ICCID read on a 30 s TTL.
- **`sims.go`** — `SIMRegistry` polls the modem every 30 s for the current card and maps it to a `sims` row. It is the only thing that reads the SIM over USB; every writer of a SIM-scoped row asks it via `CurrentID()` (a mutex read, no USB I/O), which keeps the 3 s traffic sampler off the AT port. A read failure is *not* a swap: the last known SIM stays current so a dongle blip doesn't dump rows into the unknown bucket.
- **`pdu.go`** — standalone SMS-DELIVER PDU decoder (GSM7/UCS2/8-bit, UDH parsing for concatenated-message ref/seq/total). Pure functions.
- **`ingest.go`** — `Archiver` drains modem storages ("SM" SIM, "ME" flash) into SQLite every 60 s, reassembles concatenated messages (waits up to 24 h `partGracePeriod` for missing parts), then deletes archived messages from hardware (unless `-archive-delete=false`) so the tiny slot pools never fill and bounce messages. Order matters: a message is deleted only after `ArchiveInbound` succeeds. Messages are credited to the SIM in the dongle at drain time.
- **`db.go`** — `Store` over SQLite (`data/vohive.db`, WAL, `SetMaxOpenConns(1)`). **Everything is per-SIM**: `messages` and `usage` carry a `sim_id` into `sims`, which is keyed on **ICCID, not the phone number** (`AT+CNUM` is blank on many prepaid/MVNO SIMs, so numbers are labels, never identities). `sim_id` 0 is the reserved "unknown SIM" bucket. Inbound dedup is a content hash in a `UNIQUE` column *scoped to the SIM*, so re-listing undeleted hardware messages is idempotent but the same text on two cards is two messages. Schema changes go through `migrate()` + `PRAGMA user_version`. Legacy `data/usage.json` is imported once at startup and renamed.
- **`traffic.go`** — data usage is *not* read from the modem: it samples macOS interface byte counters (`netstat -ibn`) for the dongle's ECM interface (found via `ioreg -r -n Baiwang`). Handles counter resets and interface renames on replug. Bytes are attributed to the SIM active at sample time; the accumulator rebinds (persisting the outgoing one) when the card *or* the day changes. Daily totals write-through to the store every 30 s. The snapshot's `up` field is the *real* link state (`ifconfig` "status: active"), not mere interface presence — the ECM link can be down while the interface still exists.
- **`recovery.go`** — ECM link watchdog. The dongle's firmware drops the ECM Ethernet link on USB suspend (lid close) and never re-asserts it after resume, leaving macOS with a dead data path while the AT port still works. The watchdog detects link-down ≥ 10 s with the AT port still answering and issues a USB port reset (`Modem.ResetUSB`) to force re-enumeration; rate-limited to one reset per 60 s. Down-time must be observed while continuously awake: a wall-clock jump between ticks means the machine slept, which clears the timer and imposes a 15 s settle — resetting while the USB stack is still resuming can knock the dongle off the bus until physically replugged.
- **`server.go`** — HTTP API (`/api/status`, `/api/sims`, `/api/traffic`, `/api/data`, `/api/sms/inbox`, `/api/sms/send`, `/api/sms/move`) + embedded `static/` UI. Status is cached 5 s to throttle AT-port polling; the inbox handler triggers `SyncIfStale(30s)` before reading the DB. SIM-scoped views take `?sim=<id>`, defaulting to the card in the dongle; live values (rates, session bytes, interface) exist only for that card, so requesting another SIM returns stored history only.
- **`main.go`** — wiring + the `at` CLI subcommand. Startup order matters: `SIMRegistry.Refresh()` runs synchronously *before* the traffic tracker and archiver start, so the process's first bytes and messages are attributed to the real SIM instead of the unknown bucket.

## Constraints to keep in mind

- **The AT interface is exclusive**: only one process can claim it. The server and `vohive-mac at` cannot run simultaneously, and within the process everything must go through the single `Modem` and its mutex.
- **History adoption is a once-ever event, not once-per-process.** `Store.AdoptUnknown` claims the pre-SIM history for the first SIM identified, and is guarded by `meta['history_adopted']`. This is a *guess*: the archive drained off the hardware can contain messages received under a card the app never saw (this repo's own DB had years-old China Telecom messages under a China Unicom SIM). The user corrects it by re-filing messages onto another SIM — including back onto the unknown SIM — via `/api/sms/move`. If that guard is ever weakened to a per-process check, every restart would undo those corrections.
- The UI in `static/` is a single self-contained HTML file embedded via `go:embed` — rebuilding the binary is required to pick up UI changes.
- No authentication on the HTTP API; it binds to `127.0.0.1` by default deliberately.
- `docs/dongle-setup.md` documents the one-time `AT+QCFG="usbnet",1` switch to CDC ECM mode that makes the dongle usable on macOS at all — relevant when debugging "dongle not found" or interface-numbering issues (interface layout differs by USB mode; AT is interface 3).
