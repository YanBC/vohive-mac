// SIM PIN handling: entering the code a locked card is waiting for, and
// turning the power-on PIN request on or off.
//
// Every code offered to a card is spent. Three wrong PINs block the SIM until
// the 8-digit PUK from the carrier is entered, and ten wrong PUKs destroy the
// card permanently — so nothing here sends a code the modem did not ask for.
// The pending state (AT+CPIN?) is re-read immediately before every attempt, a
// PIN is never fed to a PUK prompt, malformed input is rejected locally rather
// than spent on the card, and the remaining-attempt counter (AT+QPINC?) is
// reported back so the user can see what a mistake costs.
//
// Codes never reach a log line or an API response: the commands that carry
// them go through Modem.cmdEchoLocked with a redacted echo.
package modem

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// pinSettle bounds how long we wait for a card to reach READY after it accepts
// a code: the modem answers OK before the SIM has finished initialising, and
// reporting the state it had a moment ago would look like the unlock failed.
const pinSettle = 5 * time.Second

// InputError marks a refusal decided here, before anything reached the card —
// a malformed code, or an operation the current lock state does not allow. The
// API answers 400 for these and 502 only for what the modem actually said.
type InputError struct{ msg string }

func (e InputError) Error() string { return e.msg }

func badInput(format string, a ...any) error { return InputError{fmt.Sprintf(format, a...)} }

// SIMLock is the PIN state of the card in the dongle.
type SIMLock struct {
	// State is the raw AT+CPIN? value: READY, SIM PIN, SIM PUK, SIM PIN2…
	State string `json:"state"`
	// Ready is true when no code is pending and the card is usable.
	Ready bool `json:"ready"`
	// Need is the code the card is waiting for: "pin", "puk", "pin2", "puk2",
	// or "other" for a state (e.g. a network lock) no SIM code can clear.
	Need string `json:"need,omitempty"`
	// Enabled is whether the card asks for its PIN at power-on. nil when the
	// firmware won't say (AT+CLCK="SC",2 refused).
	Enabled *bool `json:"enabled,omitempty"`
	// Attempts left before the card escalates; -1 when the firmware won't say.
	PINRetries int `json:"pin_retries"`
	PUKRetries int `json:"puk_retries"`
}

// codeNeeded maps an AT+CPIN? state to the code that clears it. States are
// matched by suffix because firmwares prefix them variously ("SIM PIN",
// "PH-SIM PIN"); anything unrecognised is "other" — a phone/network lock, not
// something a SIM code opens.
func codeNeeded(state string) string {
	switch {
	case state == "READY":
		return ""
	case strings.HasSuffix(state, "PUK2"):
		return "puk2"
	case strings.HasSuffix(state, "PIN2"):
		return "pin2"
	case strings.HasSuffix(state, "PUK"):
		return "puk"
	case strings.HasSuffix(state, "PIN"):
		return "pin"
	}
	return "other"
}

// checkCode rejects input the card would only bounce. Every rejected attempt
// costs one of the three the SIM allows, so malformed codes never reach it.
func checkCode(code, what string, min, max int) error {
	if code == "" {
		return badInput("%s is required", what)
	}
	if strings.Trim(code, "0123456789") != "" {
		return badInput("%s must be digits only", what)
	}
	if len(code) < min || len(code) > max {
		if min == max {
			return badInput("%s must be %d digits", what, min)
		}
		return badInput("%s must be %d to %d digits", what, min, max)
	}
	return nil
}

// pinStateLocked reads AT+CPIN?. A locked card answers this normally (with
// OK), so an error here means the AT port or the card is unreachable, not
// that a code is pending.
func (m *Modem) pinStateLocked() (string, error) {
	resp, err := m.cmdLocked("AT+CPIN?", 5*time.Second)
	if err != nil {
		return "", err
	}
	state := strings.ToUpper(strings.TrimSpace(firstField(resp, "+CPIN:")))
	if state == "" {
		return "", fmt.Errorf("modem gave no +CPIN state")
	}
	return state, nil
}

// pinRetriesLocked reads the remaining PIN/PUK attempts. Quectel-style
// firmware answers AT+QPINC?; anything else just errors, and the counter is
// then simply unknown — never guessed.
func (m *Modem) pinRetriesLocked() (pin, puk int, ok bool) {
	resp, err := m.cmdLocked("AT+QPINC?", 5*time.Second)
	if err != nil {
		return 0, 0, false
	}
	// +QPINC: "SC",3,10   (and a "P2" line for PIN2)
	for _, line := range strings.Split(resp, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "+QPINC:") {
			continue
		}
		f := strings.Split(strings.TrimPrefix(line, "+QPINC:"), ",")
		if len(f) < 3 || strings.Trim(strings.TrimSpace(f[0]), `" `) != "SC" {
			continue
		}
		p, err1 := strconv.Atoi(strings.TrimSpace(f[1]))
		k, err2 := strconv.Atoi(strings.TrimSpace(f[2]))
		if err1 != nil || err2 != nil {
			continue
		}
		return p, k, true
	}
	return 0, 0, false
}

func (m *Modem) simLockLocked() (SIMLock, error) {
	state, err := m.pinStateLocked()
	if err != nil {
		return SIMLock{}, err
	}
	lk := SIMLock{
		State:      state,
		Ready:      state == "READY",
		Need:       codeNeeded(state),
		PINRetries: -1,
		PUKRetries: -1,
	}
	if lk.Ready {
		// AT+CLCK="SC",2 is only answerable once the card is usable.
		if resp, err := m.cmdLocked(`AT+CLCK="SC",2`, 5*time.Second); err == nil {
			if f := firstField(resp, "+CLCK:"); f != "" {
				enabled := strings.TrimSpace(strings.SplitN(f, ",", 2)[0]) == "1"
				lk.Enabled = &enabled
			}
		}
	} else if lk.Need == "pin" || lk.Need == "puk" {
		// A pending SIM PIN/PUK prompt is proof the lock is on.
		enabled := true
		lk.Enabled = &enabled
	}
	if pin, puk, ok := m.pinRetriesLocked(); ok {
		lk.PINRetries, lk.PUKRetries = pin, puk
	}
	return lk, nil
}

// SIMLockState reports whether the card in the dongle wants a code, whether it
// asks for one at power-on, and how many attempts are left.
func (m *Modem) SIMLockState() (SIMLock, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.simLockLocked()
}

// PINReady reports whether the card is usable right now, asking the modem
// directly (one AT command). The ECM watchdog uses it: a card waiting for its
// PIN carries no data, and no amount of USB resetting will change that.
func (m *Modem) PINReady() (ready bool, need string, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	state, err := m.pinStateLocked()
	if err != nil {
		return false, "", err
	}
	return state == "READY", codeNeeded(state), nil
}

// cmeCode extracts the numeric code from a +CME ERROR response, or -1.
func cmeCode(resp string) int {
	f := firstField(resp, "+CME ERROR:")
	n, err := strconv.Atoi(strings.TrimSpace(f))
	if err != nil {
		return -1
	}
	return n
}

// codeErrorLocked turns the +CME ERROR from a rejected code into something the
// user can act on, and appends the attempts the card has left — the number
// that decides whether a second guess is worth risking.
func (m *Modem) codeErrorLocked(resp string, err error) error {
	var msg string
	switch cmeCode(resp) {
	case 3:
		msg = "operation not allowed — on most firmware the PIN lock must be enabled before the PIN can be changed"
	case 10:
		msg = "no SIM in the dongle"
	case 11, 12, 17, 18:
		msg = "the card is asking for a different code than the one supplied"
	case 13:
		msg = "SIM failure"
	case 16:
		msg = "incorrect code"
	default:
		return err // unrecognised: pass the modem's own (redacted) wording
	}
	pin, puk, ok := m.pinRetriesLocked()
	switch {
	case !ok:
		return fmt.Errorf("%s", msg)
	case pin > 0:
		return fmt.Errorf("%s — %d PIN attempt(s) left before the card needs its PUK", msg, pin)
	case pin == 0 && puk > 0:
		return fmt.Errorf("%s — the PIN is now blocked; %d PUK attempt(s) left", msg, puk)
	default:
		return fmt.Errorf("%s — no attempts left", msg)
	}
}

// awaitReadyLocked re-reads the lock state until the card reports READY, for
// up to pinSettle: an accepted code returns OK before the SIM has finished
// initialising, and the state read a moment too early looks like a failure.
func (m *Modem) awaitReadyLocked() (SIMLock, error) {
	deadline := time.Now().Add(pinSettle)
	for {
		lk, err := m.simLockLocked()
		if err == nil && lk.Ready {
			return lk, nil
		}
		if time.Now().After(deadline) {
			return lk, err
		}
		time.Sleep(400 * time.Millisecond)
	}
}

// EnterPIN feeds the code a locked card is waiting for. It sends only what the
// card actually asked for: offering a PIN to a card that wants its PUK — or to
// one that is already unlocked — burns an attempt for nothing, so both are
// refused here without touching the SIM.
//
// A card in PUK state needs newPIN too: the PUK does not unblock the old PIN,
// it replaces it.
func (m *Modem) EnterPIN(code, newPIN string) (SIMLock, error) {
	code = strings.TrimSpace(code)
	newPIN = strings.TrimSpace(newPIN)

	m.mu.Lock()
	defer m.mu.Unlock()
	m.cmdLocked("ATE0", 4*time.Second) //nolint:errcheck — keep the code out of the echo

	state, err := m.pinStateLocked()
	if err != nil {
		return SIMLock{}, err
	}
	var resp string
	switch need := codeNeeded(state); need {
	case "":
		return SIMLock{}, badInput("the SIM is already unlocked")
	case "pin", "pin2":
		if err := checkCode(code, "PIN", 4, 8); err != nil {
			return SIMLock{}, err
		}
		resp, err = m.cmdEchoLocked(
			fmt.Sprintf(`AT+CPIN="%s"`, code), `AT+CPIN=<pin>`, 15*time.Second)
	case "puk", "puk2":
		if err := checkCode(code, "PUK", 8, 8); err != nil {
			return SIMLock{}, err
		}
		if err := checkCode(newPIN, "new PIN", 4, 8); err != nil {
			return SIMLock{}, badInput(
				"this card is blocked and needs its PUK plus a new PIN to replace the blocked one: %s", err)
		}
		resp, err = m.cmdEchoLocked(
			fmt.Sprintf(`AT+CPIN="%s","%s"`, code, newPIN), `AT+CPIN=<puk>,<newpin>`, 15*time.Second)
	default:
		return SIMLock{}, badInput("the SIM is in state %q, which no SIM code can clear", state)
	}
	if err != nil {
		lk, _ := m.simLockLocked()
		return lk, m.codeErrorLocked(resp, err)
	}
	// The card is a different one as far as readable identity goes: IMSI,
	// number and operator are unreadable while locked, so drop the cache.
	m.simOK = false
	return m.awaitReadyLocked()
}

// SetPINLock turns the power-on PIN request on or off (AT+CLCK="SC"). The
// card's current PIN is required either way — enabling the lock does not
// choose a new PIN, it starts demanding the one the card already has (a fresh
// SIM's is the carrier's default, usually printed on the holder). Use
// ChangePIN to pick a different one.
func (m *Modem) SetPINLock(enabled bool, pin string) (SIMLock, error) {
	pin = strings.TrimSpace(pin)
	if err := checkCode(pin, "PIN", 4, 8); err != nil {
		return SIMLock{}, err
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	m.cmdLocked("ATE0", 4*time.Second) //nolint:errcheck

	state, err := m.pinStateLocked()
	if err != nil {
		return SIMLock{}, err
	}
	if codeNeeded(state) != "" {
		return SIMLock{}, badInput(
			"the SIM is locked (%s) — enter its code before changing the lock setting", state)
	}
	mode := 0
	if enabled {
		mode = 1
	}
	resp, err := m.cmdEchoLocked(
		fmt.Sprintf(`AT+CLCK="SC",%d,"%s"`, mode, pin),
		fmt.Sprintf(`AT+CLCK="SC",%d,<pin>`, mode), 10*time.Second)
	if err != nil {
		return SIMLock{}, m.codeErrorLocked(resp, err)
	}
	return m.simLockLocked()
}

// ChangePIN replaces the card's PIN (AT+CPWD="SC"). Most firmware refuses
// while the lock is disabled — there is no PIN in force to change — so enable
// the lock first.
func (m *Modem) ChangePIN(oldPIN, newPIN string) (SIMLock, error) {
	oldPIN, newPIN = strings.TrimSpace(oldPIN), strings.TrimSpace(newPIN)
	if err := checkCode(oldPIN, "current PIN", 4, 8); err != nil {
		return SIMLock{}, err
	}
	if err := checkCode(newPIN, "new PIN", 4, 8); err != nil {
		return SIMLock{}, err
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	m.cmdLocked("ATE0", 4*time.Second) //nolint:errcheck

	state, err := m.pinStateLocked()
	if err != nil {
		return SIMLock{}, err
	}
	if codeNeeded(state) != "" {
		return SIMLock{}, badInput(
			"the SIM is locked (%s) — enter its code before changing the PIN", state)
	}
	resp, err := m.cmdEchoLocked(
		fmt.Sprintf(`AT+CPWD="SC","%s","%s"`, oldPIN, newPIN),
		`AT+CPWD="SC",<oldpin>,<newpin>`, 10*time.Second)
	if err != nil {
		return SIMLock{}, m.codeErrorLocked(resp, err)
	}
	return m.simLockLocked()
}
