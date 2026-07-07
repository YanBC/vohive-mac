# vohive-mac

A minimal SIM-card management platform for **macOS**, inspired by
[VoHive](https://github.com/iniwex5/vohive) (which targets Linux). It drives a
BAIWANG/Quectel-style 4G USB dongle **entirely from userspace over raw USB**
(libusb) — no kernel extensions, no serial drivers — and serves a web console for:

1. **Live modem status** — carrier, RAT, signal, SIM state, WAN IP
2. **SMS** — send messages (full Unicode/UCS2, auto-split) and read the inbox
   (PDU decoding, concatenated-message reassembly)
3. **Cellular data usage** — live throughput chart, session/today totals, daily
   history persisted across restarts

Single Go binary; the UI is embedded. SMS goes over the dongle's AT port via USB
bulk transfers; data usage is sampled from the macOS interface counters of the
dongle's ECM network interface.

## Prerequisites

- macOS 11+ (Apple Silicon or Intel)
- `brew install libusb go`
- A BAIWANG QDC507 dongle (USB ID `2ca3:4006`) **switched to ECM USB mode** —
  a one-time step; see **[docs/dongle-setup.md](docs/dongle-setup.md)**.
  Other Quectel-compatible sticks may work via `BAIWANG_VID`/`BAIWANG_PID`/
  `BAIWANG_IFACE` env overrides.

## Build & run

```sh
go build -o vohive-mac .
./vohive-mac                # web console on http://127.0.0.1:7676
./vohive-mac -addr :7676    # expose on the LAN (no auth — beware)
```

## AT command console

The binary doubles as an ad-hoc AT terminal over the same raw-USB channel
(stop the server first — only one process can claim the AT interface):

```sh
./vohive-mac at 'AT+CSQ' 'AT+COPS?'
```

This is also the tool used for the one-time ECM mode switch in the setup doc.

## HTTP API

| Route | Method | Description |
|---|---|---|
| `/api/status` | GET | Modem/SIM/network status (cached 5 s) |
| `/api/traffic` | GET | Counters, rates, history, daily usage |
| `/api/sms/inbox` | GET | Decoded inbox messages, newest first |
| `/api/sms/send` | POST | `{"to": "+86138...", "text": "..."}` |

## Layout

```
main.go      entrypoint + `at` CLI subcommand
modem.go     raw-USB AT channel, SMS send (UCS2), status queries
pdu.go       SMS-DELIVER PDU decoder + concat reassembly
traffic.go   interface-counter sampler, daily usage persistence (data/usage.json)
server.go    HTTP API + embedded static UI
static/      web console (self-contained HTML/CSS/JS)
docs/        dongle setup manual (USB mode switch, troubleshooting, uninstall)
```

## Limitations / notes

- No authentication — it binds to `127.0.0.1` by default for a reason.
- Data usage is measured at the Mac's interface, so it counts this Mac's traffic
  through the dongle (not other devices using the dongle's hotspot, if any).
- The AT interface is exclusive: the web server and `vohive-mac at` cannot run
  at the same time.
