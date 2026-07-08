// Raw-USB AT command channel for the BAIWANG QDC507 4G dongle on macOS.
//
// macOS creates no /dev/cu.* for the dongle's vendor-specific serial
// interfaces, so we talk to the AT port directly over USB bulk transfers
// via libusb (gousb). One Modem owns the interface for the whole process;
// commands are serialized with a mutex.
package main

import (
	"context"
	"encoding/hex"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf16"

	"github.com/google/gousb"
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

	infoOnce  sync.Once
	infoCache map[string]string
}

func NewModem() *Modem { return &Modem{} }

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
		return resp, fmt.Errorf("%s failed: %s", command, strings.TrimSpace(resp))
	}
	return resp, nil
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
func (m *Modem) ListStorage(storage string) ([]pduRecord, error) {
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

	var raw []pduRecord
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
		dec, derr := decodePDU(strings.TrimSpace(lines[i+1]))
		if derr != nil {
			continue // skip undecodable entries rather than failing the list
		}
		dec.index = idx
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

// ------------------------------------------------------------------- status

type Status struct {
	Connected bool   `json:"connected"`
	Error     string `json:"error,omitempty"`
	Model     string `json:"model,omitempty"`
	IMEI      string `json:"imei,omitempty"`
	OwnNumber string `json:"own_number,omitempty"`
	RSSI      int    `json:"rssi"`
	SignalPct int    `json:"signal_pct"`
	Operator  string `json:"operator,omitempty"`
	RAT       string `json:"rat,omitempty"`
	SIM       string `json:"sim,omitempty"`
	WanIP     string `json:"wan_ip,omitempty"`
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
		if r, err := m.cmdLocked("AT+CNUM", 4*time.Second); err == nil {
			if f := firstField(r, "+CNUM:"); f != "" {
				if fields := strings.Split(f, ","); len(fields) >= 2 {
					m.infoCache["own_number"] = strings.Trim(fields[1], `" `)
				}
			}
		}
	})
	st.Model = m.infoCache["model"]
	st.IMEI = m.infoCache["imei"]
	st.OwnNumber = m.infoCache["own_number"]

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
	if r, err := m.cmdLocked("AT+CPIN?", 4*time.Second); err == nil {
		st.SIM = firstField(r, "+CPIN:")
	} else {
		st.SIM = "ERROR"
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
