// Raw-USB AT command channel for the BAIWANG QDC507 4G dongle on macOS.
//
// macOS creates no /dev/cu.* for the dongle's vendor-specific serial
// interfaces, so we talk to the AT port directly over USB bulk transfers
// via libusb (gousb). One Modem owns the interface for the whole process;
// commands are serialized with a mutex.
package modem

import (
	"context"
	"encoding/hex"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf16"

	"github.com/google/gousb"

	"github.com/YanBC/vohive-mac/internal/pdu"
	"github.com/YanBC/vohive-mac/internal/sim"
)

var (
	modemVID  = envHex("BAIWANG_VID", 0x2ca3)
	modemPID  = envHex("BAIWANG_PID", 0x4006)
	modemATIf = envInt("BAIWANG_IFACE", 3)
)

func envHex(key string, def uint16) uint16 {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.ParseUint(v, 16, 16); err == nil {
			return uint16(n)
		}
	}
	return def
}

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

type Modem struct {
	mu   sync.Mutex
	ctx  *gousb.Context
	dev  *gousb.Device
	cfg  *gousb.Config
	intf *gousb.Interface
	in   *gousb.InEndpoint
	out  *gousb.OutEndpoint

	// hardware identity: fixed for the life of the dongle, read once
	infoOnce  sync.Once
	infoCache map[string]string

	// SIM identity: NOT fixed — the card can be swapped under us, so this is
	// re-validated against the ICCID on a TTL rather than cached forever.
	simCache sim.Identity
	simOK    bool
	simTime  time.Time

	// last observed "cellular data disabled" state; see dataEnabledLocked.
	dataOff atomic.Bool
	// unix time until which a deliberate modem reboot (SetDataEnabled) is
	// still settling; the ECM link being down before then is expected.
	rebootUntil atomic.Int64
}

// New returns a Modem that connects lazily on its first command.
func New() *Modem { return &Modem{} }

// ---------------------------------------------------------------- connection

func (m *Modem) connectLocked() error {
	if m.in != nil {
		return nil
	}
	if m.ctx == nil {
		m.ctx = gousb.NewContext()
	}
	dev, err := m.ctx.OpenDeviceWithVIDPID(gousb.ID(modemVID), gousb.ID(modemPID))
	if err != nil || dev == nil {
		return fmt.Errorf("dongle not found on USB (%04x:%04x)", modemVID, modemPID)
	}
	cfg, err := dev.Config(1)
	if err != nil {
		dev.Close()
		return fmt.Errorf("usb config: %w", err)
	}
	intf, err := cfg.Interface(modemATIf, 0)
	if err != nil {
		cfg.Close()
		dev.Close()
		return fmt.Errorf("claim interface %d: %w", modemATIf, err)
	}
	var in *gousb.InEndpoint
	var out *gousb.OutEndpoint
	for _, ep := range intf.Setting.Endpoints {
		if ep.TransferType != gousb.TransferTypeBulk {
			continue
		}
		if ep.Direction == gousb.EndpointDirectionIn && in == nil {
			in, _ = intf.InEndpoint(ep.Number)
		}
		if ep.Direction == gousb.EndpointDirectionOut && out == nil {
			out, _ = intf.OutEndpoint(ep.Number)
		}
	}
	if in == nil || out == nil {
		intf.Close()
		cfg.Close()
		dev.Close()
		return fmt.Errorf("interface %d has no bulk endpoint pair", modemATIf)
	}
	m.dev, m.cfg, m.intf, m.in, m.out = dev, cfg, intf, in, out
	return nil
}

func (m *Modem) disconnectLocked() {
	if m.intf != nil {
		m.intf.Close()
	}
	if m.cfg != nil {
		m.cfg.Close()
	}
	if m.dev != nil {
		m.dev.Close()
	}
	m.dev, m.cfg, m.intf, m.in, m.out = nil, nil, nil, nil, nil
}

// ---------------------------------------------------------------- raw I/O

// readUntil accumulates bulk-IN data until any terminator appears or the
// deadline passes. Individual reads use short timeouts so we keep polling.
func (m *Modem) readUntilLocked(terminators []string, timeout time.Duration) (string, error) {
	var data []byte
	buf := make([]byte, 16384)
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		rctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
		n, err := m.in.ReadContext(rctx, buf)
		cancel()
		if n > 0 {
			data = append(data, buf[:n]...)
			s := string(data)
			for _, t := range terminators {
				if strings.Contains(s, t) {
					return s, nil
				}
			}
		}
		if err != nil && rctx.Err() == nil {
			// real USB error, not our poll timeout
			m.disconnectLocked()
			return string(data), fmt.Errorf("usb read: %w", err)
		}
	}
	return string(data), nil
}

func (m *Modem) flushLocked() {
	rctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	buf := make([]byte, 16384)
	m.in.ReadContext(rctx, buf) //nolint:errcheck — best-effort drain
}

func (m *Modem) writeLocked(data []byte) error {
	wctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := m.out.WriteContext(wctx, data); err != nil {
		m.disconnectLocked()
		return fmt.Errorf("usb write: %w", err)
	}
	return nil
}

func (m *Modem) cmdLocked(command string, timeout time.Duration) (string, error) {
	return m.cmdEchoLocked(command, command, timeout)
}

// cmdEchoLocked is cmdLocked with the command name that appears in errors
// given separately: commands carrying a PIN or PUK pass a redacted echo, so a
// code can never reach a log line or an API response. A redacted command also
// quotes only the modem's error line back, never the whole response — with AT
// echo on, that response would contain the code itself.
func (m *Modem) cmdEchoLocked(command, echo string, timeout time.Duration) (string, error) {
	if err := m.connectLocked(); err != nil {
		return "", err
	}
	m.flushLocked()
	if err := m.writeLocked([]byte(command + "\r")); err != nil {
		return "", err
	}
	resp, err := m.readUntilLocked(
		[]string{"OK\r\n", "ERROR\r\n", "+CME ERROR", "+CMS ERROR"}, timeout)
	if err != nil {
		return resp, err
	}
	if strings.Contains(resp, "ERROR") {
		detail := strings.TrimSpace(resp)
		if echo != command {
			detail = errorLine(resp)
		}
		return resp, fmt.Errorf("%s failed: %s", echo, detail)
	}
	return resp, nil
}

// errorLine picks the modem's "…ERROR…" line out of a response, dropping any
// echoed command (which may carry a PIN) and the surrounding framing.
func errorLine(resp string) string {
	for _, line := range strings.Split(resp, "\n") {
		if line = strings.TrimSpace(line); strings.Contains(line, "ERROR") {
			return line
		}
	}
	return "ERROR"
}

// Cmd sends one AT command and returns the full response text.
func (m *Modem) Cmd(command string, timeout time.Duration) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cmdLocked(command, timeout)
}

// ResetUSB issues a USB port reset, forcing macOS to drop and re-enumerate
// the dongle — the only userspace way to revive the ECM data link when the
// firmware fails to re-assert it after system sleep. The AT connection is
// torn down; the next command reconnects once re-enumeration completes.
func (m *Modem) ResetUSB() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.connectLocked(); err != nil {
		return err
	}
	// Release our claimed interface/config first, keeping only the device
	// handle: resetting with an interface claimed can fail outright.
	m.intf.Close()
	m.cfg.Close()
	err := m.dev.Reset()
	m.dev.Close()
	m.dev, m.cfg, m.intf, m.in, m.out = nil, nil, nil, nil, nil
	return err
}

func (m *Modem) Connected() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.connectLocked() == nil
}

// --------------------------------------------------------------- data control
//
// Cellular data on this firmware is controlled by the "qcautoconnect" flag
// (see docs/dongle-setup.md §3), which the router firmware reads only at
// modem boot: with 0 it stops bridging the ECM LAN to the cellular bearer,
// so no user traffic — and no data charges — is possible. The modem stays
// registered (SMS keeps working; it rides the signaling/IMS path) and the
// ECM interface keeps its LAN link and DHCP, it just forwards nothing.
// Verified on the QDC507: a mere USB reset does NOT apply the flag, and
// AT+CGACT=0,1 is refused (the router owns the session) — a CFUN=1,1
// modem reboot is required. The setting lives in NV and survives replug
// and reboot. Note the LTE default bearer still exists while registered,
// so AT+CGPADDR keeps reporting a WAN IP even with data off.

// dataEnabledLocked queries the autoconnect flag. supported=false means the
// modem answered but rejected the command (non-Quectel-compatible firmware):
// data is then uncontrollable by us and must be treated as "on", not "off".
func (m *Modem) dataEnabledLocked() (enabled, supported bool, err error) {
	resp, err := m.cmdLocked(`AT+QCFG="qcautoconnect"`, 5*time.Second)
	if err != nil {
		if strings.Contains(resp, "ERROR") {
			return true, false, nil
		}
		return false, false, err
	}
	// +QCFG: "qcautoconnect",1
	f := firstField(resp, "+QCFG:")
	if i := strings.LastIndex(f, ","); i >= 0 {
		enabled = strings.Trim(strings.TrimSpace(f[i+1:]), `"`) != "0"
		m.dataOff.Store(!enabled)
		return enabled, true, nil
	}
	return true, false, nil
}

// DataEnabled reports whether the modem will (auto)dial a data session.
func (m *Modem) DataEnabled() (enabled, supported bool, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.dataEnabledLocked()
}

// DataOff reports the last state observed by DataEnabled/SetDataEnabled
// without touching the modem. Used by the ECM watchdog: a deliberately
// disabled data link must not be "recovered" with USB resets.
func (m *Modem) DataOff() bool { return m.dataOff.Load() }

// SetDataEnabled flips the autoconnect flag and reboots the modem
// (AT+CFUN=1,1 — the only way the flag takes effect, see above). The dongle
// is off the bus for ~20 s afterwards; the next command reconnects.
func (m *Modem) SetDataEnabled(enabled bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	val := 0
	if enabled {
		val = 1
	}
	if _, err := m.cmdLocked(fmt.Sprintf(`AT+QCFG="qcautoconnect",%d`, val), 5*time.Second); err != nil {
		return err
	}
	m.dataOff.Store(!enabled)
	return m.rebootLocked()
}

// Reboot restarts the modem without changing any setting. The watchdog uses
// it to clear the dongle's DHCP lease table, which is the only way to get an
// IPv4 lease back once that table has filled up (see healDHCP).
func (m *Modem) Reboot() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.rebootLocked()
}

// rebootLocked issues the reboot and opens the settling window every other
// component reads through Rebooting. Caller holds mu.
func (m *Modem) rebootLocked() error {
	if _, err := m.cmdLocked("AT+CFUN=1,1", 10*time.Second); err != nil {
		return err
	}
	m.rebootUntil.Store(time.Now().Add(rebootSettle).Unix())
	m.disconnectLocked() // the device is about to fall off the bus
	return nil
}

// rebootSettle is how long a commanded reboot is expected to take: the dongle
// drops off the bus, re-enumerates, and macOS re-runs DHCP on the new
// interface. Generous on purpose — the whole point of the window is that
// nothing "recovers" a link that is merely restarting.
const rebootSettle = 45 * time.Second

// Rebooting reports whether a deliberate modem reboot is still settling.
// The ECM watchdog must not "recover" the expected link-down of a reboot.
func (m *Modem) Rebooting() bool { return time.Now().Unix() < m.rebootUntil.Load() }

// ------------------------------------------------- SMS send (text mode, UCS2)

func ucs2Hex(s string) string {
	codes := utf16.Encode([]rune(s))
	b := make([]byte, 0, len(codes)*2)
	for _, c := range codes {
		b = append(b, byte(c>>8), byte(c))
	}
	return strings.ToUpper(hex.EncodeToString(b))
}

// splitUCS2 splits text into chunks of at most 70 UTF-16 code units
// (the single-SMS UCS2 limit), never splitting a surrogate pair.
func splitUCS2(text string, limit int) []string {
	var chunks []string
	var cur []rune
	units := 0
	for _, r := range text {
		w := 1
		if r > 0xFFFF {
			w = 2
		}
		if units+w > limit {
			chunks = append(chunks, string(cur))
			cur, units = nil, 0
		}
		cur = append(cur, r)
		units += w
	}
	if len(cur) > 0 {
		chunks = append(chunks, string(cur))
	}
	return chunks
}

type SendResult struct {
	Parts int   `json:"parts"`
	Refs  []int `json:"refs"`
}

// SendSMS sends a text message. Messages longer than 70 UCS2 code units are
// split into multiple independent SMS.
func (m *Modem) SendSMS(to, text string) (*SendResult, error) {
	to = strings.TrimSpace(to)
	num := strings.TrimPrefix(to, "+")
	if num == "" || strings.Trim(num, "0123456789") != "" {
		return nil, fmt.Errorf("invalid destination number %q", to)
	}
	if strings.TrimSpace(text) == "" {
		return nil, fmt.Errorf("empty message")
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	for _, setup := range []string{`AT+CMGF=1`, `AT+CSCS="UCS2"`, `AT+CSMP=17,167,0,8`} {
		if _, err := m.cmdLocked(setup, 5*time.Second); err != nil {
			return nil, err
		}
	}
	defer m.cmdLocked(`AT+CSCS="IRA"`, 3*time.Second) //nolint:errcheck — restore charset

	res := &SendResult{}
	for _, chunk := range splitUCS2(text, 70) {
		ref, err := m.sendOneLocked(to, chunk)
		if err != nil {
			return res, err
		}
		res.Parts++
		res.Refs = append(res.Refs, ref)
	}
	return res, nil
}

func (m *Modem) sendOneLocked(to, chunk string) (int, error) {
	if err := m.writeLocked([]byte(fmt.Sprintf("AT+CMGS=\"%s\"\r", ucs2Hex(to)))); err != nil {
		return 0, err
	}
	prompt, err := m.readUntilLocked([]string{"> ", "ERROR\r\n", "+CMS ERROR"}, 8*time.Second)
	if err != nil {
		return 0, err
	}
	if !strings.Contains(prompt, ">") {
		return 0, fmt.Errorf("no SMS prompt: %s", strings.TrimSpace(prompt))
	}
	if err := m.writeLocked(append([]byte(ucs2Hex(chunk)), 0x1a)); err != nil {
		return 0, err
	}
	resp, err := m.readUntilLocked(
		[]string{"OK\r\n", "ERROR\r\n", "+CMS ERROR"}, 40*time.Second)
	if err != nil {
		return 0, err
	}
	if !strings.Contains(resp, "+CMGS:") {
		return 0, fmt.Errorf("send failed: %s", strings.TrimSpace(resp))
	}
	refStr := strings.TrimSpace(strings.SplitN(
		strings.SplitN(resp, "+CMGS:", 2)[1], "\r", 2)[0])
	ref, _ := strconv.Atoi(refStr)
	return ref, nil
}

// ------------------------------------ SMS storage access (PDU mode, robust)

// ListStorage returns all decodable messages in one storage ("SM" or "ME"),
// with their slot indexes, without merging concatenated parts.
func (m *Modem) ListStorage(storage string) ([]pdu.Record, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	// select <mem1> (the read/list/delete storage) only
	if _, err := m.cmdLocked(fmt.Sprintf(`AT+CPMS="%s"`, storage), 5*time.Second); err != nil {
		return nil, err
	}
	if _, err := m.cmdLocked("AT+CMGF=0", 5*time.Second); err != nil {
		return nil, err
	}
	resp, err := m.cmdLocked("AT+CMGL=4", 20*time.Second) // 4 = ALL in PDU mode
	if err != nil {
		return nil, err
	}

	var raw []pdu.Record
	lines := strings.Split(strings.ReplaceAll(resp, "\r", ""), "\n")
	for i := 0; i < len(lines); i++ {
		line := strings.TrimSpace(lines[i])
		if !strings.HasPrefix(line, "+CMGL:") {
			continue
		}
		idxStr := strings.TrimSpace(strings.SplitN(
			strings.TrimPrefix(line, "+CMGL:"), ",", 2)[0])
		idx, _ := strconv.Atoi(idxStr)
		if i+1 >= len(lines) {
			break
		}
		dec, derr := pdu.Decode(strings.TrimSpace(lines[i+1]))
		if derr != nil {
			continue // skip undecodable entries rather than failing the list
		}
		dec.Index = idx
		raw = append(raw, *dec)
		i++
	}
	return raw, nil
}

// DeleteMessages removes the given slot indexes from one storage.
func (m *Modem) DeleteMessages(storage string, indexes []int) error {
	if len(indexes) == 0 {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, err := m.cmdLocked(fmt.Sprintf(`AT+CPMS="%s"`, storage), 5*time.Second); err != nil {
		return err
	}
	for _, idx := range indexes {
		if _, err := m.cmdLocked(fmt.Sprintf("AT+CMGD=%d", idx), 8*time.Second); err != nil {
			return err
		}
	}
	return nil
}

// -------------------------------------------------------------- SIM identity

// simCacheTTL bounds how stale a SIM identity may be. Every refresh re-reads
// the ICCID (one AT command), so a card swap is noticed within this window.
const simCacheTTL = 30 * time.Second

// readICCIDLocked tries the vendor variants in turn; firmwares disagree on
// which one they answer to.
func (m *Modem) readICCIDLocked() string {
	for _, cmd := range []string{"AT+QCCID", "AT+CCID", "AT+ICCID"} {
		resp, err := m.cmdLocked(cmd, 5*time.Second)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(resp, "\n") {
			line = strings.TrimSpace(line)
			if line == "" || line == "OK" || strings.HasPrefix(line, "AT") {
				continue
			}
			for _, p := range []string{"+QCCID:", "+CCID:", "+ICCID:"} {
				line = strings.TrimPrefix(line, p)
			}
			if d := sim.Digits(line); len(d) >= 18 {
				return d
			}
		}
	}
	return ""
}

// simInfoLocked returns the current SIM, re-reading it when the cache expires.
// A cheap ICCID read decides whether the rest is still valid, so the common
// case (same card) costs one AT command per TTL.
func (m *Modem) simInfoLocked() (sim.Identity, error) {
	if m.simOK && time.Since(m.simTime) < simCacheTTL {
		return m.simCache, nil
	}
	if m.Rebooting() {
		if m.simOK {
			return m.simCache, nil // the card cannot have changed mid-reboot
		}
		return sim.Identity{}, fmt.Errorf("modem rebooting")
	}

	iccid := m.readICCIDLocked()
	if iccid == "" {
		m.simOK = false
		return sim.Identity{}, fmt.Errorf("no SIM (ICCID unreadable)")
	}
	// Same card, and we already have a number for it: nothing else to read.
	// A blank cached number is re-queried — CNUM can start answering once the
	// SIM registers on the network.
	if m.simOK && iccid == m.simCache.ICCID && m.simCache.Number != "" {
		m.simTime = time.Now()
		return m.simCache, nil
	}

	id := sim.Identity{ICCID: iccid}
	if r, err := m.cmdLocked("AT+CIMI", 4*time.Second); err == nil {
		for _, line := range strings.Split(r, "\n") {
			if d := sim.Digits(strings.TrimSpace(line)); len(d) >= 14 && len(d) <= 15 {
				id.IMSI = d
				break
			}
		}
	}
	if r, err := m.cmdLocked("AT+CNUM", 4*time.Second); err == nil {
		if f := firstField(r, "+CNUM:"); f != "" {
			if fields := strings.Split(f, ","); len(fields) >= 2 {
				id.Number = strings.Trim(fields[1], `" `)
			}
		}
	}
	if r, err := m.cmdLocked("AT+COPS?", 4*time.Second); err == nil {
		if f := firstField(r, "+COPS:"); strings.Contains(f, `"`) {
			id.Operator = strings.SplitN(f, `"`, 3)[1]
		}
	}
	m.simCache, m.simOK, m.simTime = id, true, time.Now()
	return id, nil
}

// SIMInfo returns the SIM currently in the dongle.
func (m *Modem) SIMInfo() (sim.Identity, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.simInfoLocked()
}

// ------------------------------------------------------------------- status

type Status struct {
	Connected bool   `json:"connected"`
	Error     string `json:"error,omitempty"`
	Model     string `json:"model,omitempty"`
	IMEI      string `json:"imei,omitempty"`
	OwnNumber string `json:"own_number,omitempty"`
	ICCID     string `json:"iccid,omitempty"`
	RSSI      int    `json:"rssi"`
	SignalPct int    `json:"signal_pct"`
	Operator  string `json:"operator,omitempty"`
	RAT       string `json:"rat,omitempty"`
	SIM       string `json:"sim,omitempty"`
	WanIP     string `json:"wan_ip,omitempty"`
	// nil when the firmware doesn't support the autoconnect toggle
	DataEnabled *bool `json:"data_enabled,omitempty"`
}

func firstField(resp, prefix string) string {
	for _, line := range strings.Split(resp, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, prefix) {
			return strings.TrimSpace(strings.TrimPrefix(line, prefix))
		}
	}
	return ""
}

func (m *Modem) Status() Status {
	st := Status{RSSI: 99}
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, err := m.cmdLocked("ATE0", 4*time.Second); err != nil {
		st.Error = err.Error()
		return st
	}
	m.cmdLocked(`AT+CSCS="IRA"`, 3*time.Second) //nolint:errcheck
	st.Connected = true

	m.infoOnce.Do(func() {
		m.infoCache = map[string]string{}
		if r, err := m.cmdLocked("ATI", 4*time.Second); err == nil {
			var parts []string
			for _, l := range strings.Split(r, "\n") {
				l = strings.TrimSpace(l)
				if l != "" && l != "OK" && !strings.HasPrefix(l, "Revision") {
					parts = append(parts, l)
				}
			}
			m.infoCache["model"] = strings.Join(parts, " ")
		}
		if r, err := m.cmdLocked("AT+CGSN", 4*time.Second); err == nil {
			for _, l := range strings.Split(r, "\n") {
				l = strings.TrimSpace(l)
				if len(l) >= 14 && strings.Trim(l, "0123456789") == "" {
					m.infoCache["imei"] = l
					break
				}
			}
		}
	})
	st.Model = m.infoCache["model"]
	st.IMEI = m.infoCache["imei"]

	// the SIM (unlike the IMEI/model) can be swapped, so this is TTL-cached
	if id, err := m.simInfoLocked(); err == nil {
		st.OwnNumber = id.Number
		st.ICCID = id.ICCID
	}

	if r, err := m.cmdLocked("AT+CSQ", 4*time.Second); err == nil {
		if f := firstField(r, "+CSQ:"); f != "" {
			if rssi, err := strconv.Atoi(strings.SplitN(f, ",", 2)[0]); err == nil {
				st.RSSI = rssi
				if rssi >= 0 && rssi <= 31 {
					st.SignalPct = rssi * 100 / 31
				}
			}
		}
	}
	if r, err := m.cmdLocked("AT+COPS?", 4*time.Second); err == nil {
		if f := firstField(r, "+COPS:"); strings.Contains(f, `"`) {
			st.Operator = strings.SplitN(f, `"`, 3)[1]
			last := f[strings.LastIndex(f, ",")+1:]
			st.RAT = map[string]string{
				"0": "GSM", "2": "3G", "4": "3G", "7": "LTE",
				"11": "5G", "13": "5G",
			}[strings.TrimSpace(last)]
		}
	}
	if state, err := m.pinStateLocked(); err == nil {
		st.SIM = state // READY, or the code the card is waiting for — see pin.go
	} else {
		st.SIM = "ERROR"
	}
	if en, ok, err := m.dataEnabledLocked(); err == nil && ok {
		st.DataEnabled = &en
	}
	if r, err := m.cmdLocked("AT+CGPADDR=1", 4*time.Second); err == nil {
		if f := firstField(r, "+CGPADDR:"); strings.Contains(f, ",") {
			ip := strings.Trim(strings.SplitN(f, ",", 2)[1], `"`)
			ip = strings.SplitN(ip, ",", 2)[0]
			if ip != "0.0.0.0" {
				st.WanIP = ip
			}
		}
	}
	return st
}
