// Package netif is everything this app knows about the dongle's ECM network
// interface: finding it, reading its link state and low-data flags, setting
// those flags, and sampling its byte counters.
//
// It is deliberately below the SIM and the modem. The metered reconciler and
// the ECM watchdog both need to know what the interface is doing, and both
// read it through one shared parse here rather than spawning an ifconfig of
// their own every few seconds.
package netif

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
)

// absolute path: this one is executed as root, so it is not left to $PATH
const ifconfigPath = "/sbin/ifconfig"

// LinkState is one parse of `ifconfig <iface>`.
type LinkState struct {
	Exists      bool // the interface is there at all
	Up          bool // and its link is actually up ("status: active")
	RoutableV4  bool // it holds an IPv4 address worth routing over
	Expensive   bool // IFEF_EXPENSIVE
	Constrained bool // IFXF_CONSTRAINED
}

// Marked reports the low-data marking as this app applies it: both flags, set
// and cleared together.
func (l LinkState) Marked() bool { return l.Expensive && l.Constrained }

// Flags print inside the comma-separated eflags/xflags lists, so they are
// matched between delimiters — CONSTRAINED must not match ULTRA_CONSTRAINED,
// a different flag that ifconfig prints the same way.
var (
	expensiveRe   = regexp.MustCompile(`[<,]EXPENSIVE[,>]`)
	constrainedRe = regexp.MustCompile(`[<,]CONSTRAINED[,>]`)
)

// readLink reads the interface's link state and low-data flags in one pass.
// The ECM function asserts link state itself; after sleep/wake it can stay
// down (ifconfig "status: inactive") even though the interface still exists
// and netstat still lists it, so counter readability must not be used as "up".
//
// -v is required, not cosmetic: the eflags/xflags lists the low-data flags
// live in are printed only in verbose mode, and without it a marked link reads
// back as unmarked forever. Verbose mode can also exit non-zero while printing
// a perfectly good interface (it reads a supplemental sysctl that is not
// always permitted), so the output decides whether the read worked — a missing
// interface is the case that prints nothing at all.
func readLink(iface string) LinkState {
	out, _ := exec.Command("ifconfig", "-v", iface).Output()
	if len(bytes.TrimSpace(out)) == 0 {
		return LinkState{} // no such interface (unplugged, or renamed)
	}
	return parseLink(string(out))
}

func parseLink(out string) LinkState {
	return LinkState{
		Exists:      true,
		Up:          strings.Contains(out, "status: active"),
		RoutableV4:  hasRoutableV4(out),
		Expensive:   expensiveRe.MatchString(out),
		Constrained: constrainedRe.MatchString(out),
	}
}

// hasRoutableV4 reports whether the interface holds an IPv4 address that is
// worth routing over. A 169.254/16 address is what macOS self-assigns when
// its DHCP client found no server, so it counts as no IPv4 at all — the same
// as an interface that has not been configured yet. The distinction matters
// because the ECM link can be "status: active" and carrying IPv6 (SLAAC needs
// no server to answer) while every IPv4 route is missing; see the watchdog's
// healDHCP.
//
// Matched on whole fields rather than by substring: "inet6" must not match,
// and an interface can hold a real lease and a stale link-local address at
// once, in which case the real one wins.
func hasRoutableV4(out string) bool {
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) < 2 || f[0] != "inet" {
			continue
		}
		if !strings.HasPrefix(f[1], "169.254.") {
			return true
		}
	}
	return false
}

// SetMetered sets or clears both low-data flags on the interface. Root only.
func SetMetered(iface string, on bool) error {
	args := []string{iface, "expensive", "constrained"}
	if !on {
		args = []string{iface, "-expensive", "-constrained"}
	}
	out, err := exec.Command(ifconfigPath, args...).CombinedOutput()
	if err != nil {
		if msg := strings.TrimSpace(string(out)); msg != "" {
			return fmt.Errorf("%s: %s", ifconfigPath, msg)
		}
		return fmt.Errorf("%s: %w", ifconfigPath, err)
	}
	return nil
}

// Privileged reports whether this process can set the flags at all.
func Privileged() bool { return os.Geteuid() == 0 }
