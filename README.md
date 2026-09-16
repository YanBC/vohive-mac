# vohive-mac

A minimal SIM-card management platform for **macOS**, inspired by
[VoHive](https://github.com/iniwex5/vohive) (which targets Linux). It drives a
BAIWANG/Quectel-style 4G USB dongle **entirely from userspace over raw USB**
(libusb) — no kernel extensions, no serial drivers — and serves a web console for:

1. **Live modem status** — carrier, RAT, signal, SIM state, WAN IP
2. **SMS** — send messages (full Unicode/UCS2, auto-split) and browse the
   message archive (PDU decoding, concatenated-message reassembly). Messages
   are continuously archived from the modem/SIM storage into SQLite and, by
   default, deleted from the hardware afterwards — the tiny hardware slot
   pools (e.g. 50 on SIM, 23 in modem flash) never fill up and bounce
   incoming messages. Disable deletion with `-archive-delete=false`.
3. **Cellular data usage** — live throughput chart, session/today totals, daily
   history persisted across restarts
4. **SIM PIN** — unlock a PIN-protected card (or unblock one with its PUK), and
   turn the card's power-on PIN lock on or off / change its PIN. Remaining
   attempts are shown before you spend one; codes are never logged
5. **Low data mode** — mark the dongle's link metered so macOS stops treating a
   capped LTE plan as free Ethernet (see below)

Single Go binary; the UI is embedded. SMS goes over the dongle's AT port via USB
bulk transfers; data usage is sampled from the macOS interface counters of the
dongle's ECM network interface. All persistent state lives in one SQLite
database at `data/vohive.db` (a legacy `data/usage.json` is imported once and
renamed).

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

## Low data mode

macOS turns Low Data Mode on by itself for an iPhone personal hotspot, but the
dongle in ECM mode looks like ordinary wired Ethernet: the OS sees a free link
and runs iCloud sync, Photos uploads, App Store and system-update downloads
over what is actually a metered SIM.

The **low data** toggle in the Data Usage panel fixes that. It sets the two
interface flags behind Low Data Mode — `IFEF_EXPENSIVE` and `IFXF_CONSTRAINED`,
as `ifconfig en6 expensive constrained` would — and macOS then reports the link
to every app as expensive and constrained. System services and well-behaved
apps hold off on their own.

**It is on by default.** A SIM is assumed to be on a paid plan until you say
otherwise: guessing wrong that way costs a little sync latency, guessing wrong
the other way spends your data. Turn it off per card for an unlimited plan —
that choice sticks across restarts and is never re-defaulted.

Three things to know:

- **It needs root.** There is no `networksetup` command for these flags and no
  preference file to write, so the server must run as root to set them:

  ```sh
  sudo ./vohive-mac
  ```

  Without it the setting is still saved per SIM, the toggle shows *pending*,
  and the log says what is missing. Database files are handed back to the user
  who ran `sudo`, so a later non-root run still works.

- **The setting belongs to the SIM, not the dongle.** The same interface is a
  metered link with a capped travel SIM in it and a free one with an unlimited
  card, so each SIM carries its own flag and swapping cards re-applies it. New
  cards start metered, as does the reserved "unknown SIM" a card that cannot be
  read falls back to.

- **It is a hint, not a cap.** Nothing is enforced: apps that ignore the flags
  keep using the link at full speed. For a hard stop, the **data** switch next
  to it turns the modem's cellular data off outright.

The flags live on the interface instance, so they vanish on every replug and on
every USB reset the watchdog performs after sleep/wake. The server re-asserts
them within a few seconds, and deliberately leaves them in place when it exits:
the dongle keeps carrying traffic after the server stops.

## HTTP API

| Route | Method | Description |
|---|---|---|
| `/api/status` | GET | Modem/SIM/network status (cached 5 s) |
| `/api/traffic` | GET | Counters, rates, history, daily usage |
| `/api/metered` | GET / POST | Low data mode for a SIM / `{"enabled": true}` to mark the link expensive + constrained |
| `/api/sim/lock` | GET / POST | PIN state + attempts left / `{"enabled": true, "pin": "1234"}` to switch the power-on lock |
| `/api/sim/unlock` | POST | `{"code": "1234"}` — or `{"code": "<8-digit puk>", "new_pin": "1234"}` for a blocked card |
| `/api/sim/pin` | POST | `{"pin": "1234", "new_pin": "5678"}` — change the card's PIN |
| `/api/sms/inbox` | GET | Archived messages (in + out), newest first; triggers a modem sync if stale |
| `/api/sms/send` | POST | `{"to": "+86138...", "text": "..."}` — also recorded in the archive |

## Layout

```
main.go      entrypoint + `at` CLI subcommand
modem.go     raw-USB AT channel, SMS send (UCS2), storage access, status queries
pin.go       SIM PIN: unlock/PUK-unblock, power-on lock toggle, PIN change
pdu.go       SMS-DELIVER PDU decoder
ingest.go    SMS archiver: modem/SIM storage → SQLite (delete after archive)
recovery.go  ECM link watchdog: USB-resets the dongle when the data link
             stays down after sleep/wake (firmware never re-asserts it)
db.go        SQLite store (messages + usage tables)
metered.go   low data mode: keeps the ECM link's expensive/constrained flags
             in step with the current SIM's setting (needs root)
traffic.go   interface-counter sampler, write-through daily usage persistence
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
- Low data mode needs the server to run as root, and is advisory: macOS and
  well-behaved apps honour the flags, nothing enforces them.
