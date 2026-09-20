# BAIWANG 4G USB Dongle on macOS — Setup Manual

How to get a cheap BAIWANG-branded 4G LTE USB stick (a "UFI"-style dongle, modem
model **QDC507**, USB ID `2ca3:4006`) working as an internet connection on a Mac —
and as the modem behind **vohive-mac**.

**TL;DR:** Out of the box the dongle uses a USB mode that only Windows/Linux drivers
understand, so macOS shows nothing — no network interface, no serial port. The fix is a
one-time command that switches the modem's USB mode to **CDC ECM**, which macOS supports
natively. After that it's plug-and-play forever: plug it in and a "Baiwang" network
service appears in System Settings, just like a USB Ethernet adapter.

Tested on: Apple Silicon MacBook, macOS 15, China Unicom SIM. Should apply to any
macOS 11+ and any carrier (adjust the APN).

---

## 1. Background: why it doesn't work out of the box

The dongle is a small Qualcomm-based LTE modem running Quectel-compatible firmware.
Its factory USB composition (`AT+QCFG="usbnet",0` = QMI/RMNET mode) exposes five
**vendor-specific** USB interfaces:

| Interface | Function | macOS driver |
|-----------|--------------------------|--------------|
| 0 | Qualcomm DIAG | none |
| 1 | Serial (NMEA/GPS) | none |
| 2 | Serial (modem) | none |
| 3 | Serial (**AT commands**) | none |
| 4 | QMI/RMNET data | none |

macOS has no driver for any of these — that's why nothing appears in Network settings
and there is no `/dev/cu.*` device to send AT commands to.

The firmware supports other USB modes (`AT+QCFG="usbnet",<n>`):

| n | Mode | Works on |
|---|-------|----------|
| 0 | QMI/RMNET (factory default) | Linux (qmi_wwan), Windows (vendor driver) |
| 1 | **CDC ECM** | **macOS (native)**, Linux |
| 2 | MBIM | Windows 10+, Linux |
| 3 | RNDIS | Windows |

The trick is getting the switch command *into* the modem when macOS can't even see a
serial port. Solution: talk to the AT port directly over raw USB using libusb — which
is exactly what the `vohive-mac at` subcommand does.

## 2. One-time setup

### 2.1 Prerequisites

- [Homebrew](https://brew.sh): `brew install libusb go`
- The `vohive-mac` binary (`go build -o vohive-mac ./cmd/vohive-mac`)
- The dongle plugged in, with a SIM inserted

> Only one process can claim the AT interface at a time — if the vohive-mac
> server is running, stop it before using `vohive-mac at`.

### 2.2 Verify the Mac sees the dongle

```sh
ioreg -p IOUSB -w0 | grep -i baiwang
```

You should see a line like `+-o Baiwang@00100000`. If not, try another USB port/cable.

### 2.3 Talk to the modem

```sh
./vohive-mac at 'ATI' 'AT+CPIN?' 'AT+CSQ' 'AT+COPS?'
```

Expected output (roughly):

```
>>> ATI
Baiwang / QDC507 / Revision: QDC507GLEFM21 ... OK
>>> AT+CPIN?
+CPIN: READY          ← SIM detected, no PIN lock
>>> AT+CSQ
+CSQ: 25,99           ← signal 0–31; below ~10 is poor
>>> AT+COPS?
+COPS: 0,0,"CHN-UNICOM",7   ← registered on carrier, 7 = LTE
```

If `+CPIN?` returns `SIM PIN`, unlock with `./vohive-mac at 'AT+CPIN="<pin>"'`.

### 2.4 Check the APN (usually already correct)

```sh
./vohive-mac at 'AT+CGDCONT?'
```

Context 1 should carry your carrier's APN (e.g. `3gnet` for China Unicom, `cmnet` for
China Mobile, `ctnet` for China Telecom). If it's wrong or empty:

```sh
./vohive-mac at 'AT+CGDCONT=1,"IPV4V6","<your-apn>"'
```

### 2.5 Switch USB mode to ECM (the actual fix)

```sh
./vohive-mac at 'AT+QCFG="usbnet",1' 'AT+CFUN=1,1'
```

Both commands should answer `OK`. The dongle reboots itself (LED blinks, USB device
disappears for ~15 seconds). This setting is stored in the modem's non-volatile memory —
**you only ever do this once**; it survives replugging, Mac reboots, and other computers.

### 2.6 Confirm it worked

After ~20 seconds:

```sh
ifconfig en6        # interface number may differ; look for the newest "enX"
networksetup -listallnetworkservices   # a "Baiwang" service should be listed
```

The interface should be `status: active` with an IPv4 like `192.168.225.x` (DHCP from
the dongle at `192.168.225.1`), often plus a public IPv6. macOS creates the "Baiwang"
network service automatically — no manual configuration needed. Test:

```sh
ping -c 3 -S 192.168.225.<your-ip> 223.5.5.5
```

## 3. Daily use

- **Plug in → online.** The dongle attaches, the "Baiwang" service comes up, DHCP
  happens automatically. Autoconnect to the cellular network is enabled in firmware
  (`AT+QCFG="qcautoconnect",1`), so there's nothing to dial.
- **Priority vs Wi-Fi:** if Wi-Fi is also connected, macOS prefers whichever service is
  higher in **System Settings → Network → ⋯ (three-dot menu) → Set Service Order**.
  Drag "Baiwang" to the top to route through 4G, or simply turn Wi-Fi off.
- **Check status/signal any time:** run the vohive-mac web console, or
  `./vohive-mac at 'AT+CSQ' 'AT+COPS?' 'AT+CGPADDR'`
  (works in ECM mode too — the AT port is still on interface 3).
- The dongle NATs you behind `192.168.225.1`; inbound connections from the internet are
  not possible (normal for cellular anyway).

### Troubleshooting

| Symptom | Fix |
|---|---|
| No "Baiwang" service after plugging in | `ioreg -p IOUSB -w0 \| grep -i baiwang` — if absent, reseat USB; if present, check System Settings → Network for a new inactive service and enable it |
| Interface up but no internet | Check signal `AT+CSQ` (needs > ~10), registration `AT+COPS?`, APN `AT+CGDCONT?`, and that the SIM has a data plan/credit |
| Have IP but names don't resolve | Another VPN/proxy may be capturing DNS — test with `dig @192.168.225.1 example.com` |
| `vohive-mac at` says dongle not found | Dongle not enumerated, or wrong VID/PID — check `ioreg` output, or set `BAIWANG_VID`/`BAIWANG_PID` |
| `vohive-mac at` hangs / claim error | The vohive-mac server (or another process) already holds the AT interface — stop it first |

## 4. "Uninstall" — undoing everything

The setup makes exactly two kinds of changes. Undo whichever you care about:

### 4.1 Revert the dongle to its factory USB mode

Only needed if you want to use it with the Windows driver suite again
(Linux handles ECM fine, so for Linux you can leave it as is):

```sh
./vohive-mac at 'AT+QCFG="usbnet",0' 'AT+CFUN=1,1'
```

### 4.2 Clean up the Mac

- Unplug the dongle.
- Remove the leftover network service: **System Settings → Network**, select
  **Baiwang**, three-dot menu → **Delete Service**. (Or:
  `sudo networksetup -removenetworkservice "Baiwang"`.)
- Delete the vohive-mac directory, and optionally `brew uninstall libusb`.

Nothing else was installed — no kernel extensions, no launch daemons, no drivers.
ECM support is built into macOS.

---

## Appendix: adapting to other dongle models

If your stick has a different USB ID or interface layout, override via environment
variables (both the server and the `at` subcommand honor them):

```sh
BAIWANG_VID=2c7c BAIWANG_PID=0125 BAIWANG_IFACE=2 ./vohive-mac at 'ATI'
```

To locate the AT port, probe each vendor-specific interface that has a bulk IN/OUT
endpoint pair (`BAIWANG_IFACE=0,1,2,…`) and see which one answers `OK` to `AT`.
Useful discovery commands once you're in: `AT+CLAC` lists every AT command the
firmware supports; `AT+QCFG=?` lists all configurable options on Quectel-compatible
firmware.
