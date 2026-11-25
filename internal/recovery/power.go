package recovery

import (
	"os/exec"
	"strings"
	"time"
)

// absolute path: this runs as root, so it is not left to $PATH
const pmsetPath = "/usr/bin/pmset"

// sleptThreshold is how far the wall clock may run ahead of the monotonic
// clock between two ticks before the gap is taken to be a system sleep. Go's
// monotonic clock on darwin is mach_absolute_time, which stops while the
// machine sleeps and the wall clock does not, so the difference is the time
// spent asleep. Lid-closed dozing produces sleeps of about a second, far too
// short for a tick-gap check to see; NTP slewing moves the two apart by
// microseconds per tick.
const sleptThreshold = 500 * time.Millisecond

// sleptBetween reports whether the machine slept between two ticks, given the
// wall-clock and monotonic time that passed between them.
func sleptBetween(wall, mono time.Duration) bool {
	return wall-mono > sleptThreshold
}

// inDarkWake reports whether the machine is in a dark wake: awake for
// maintenance (Power Nap, network upkeep, a scheduled wake) with the lid
// closed or the display off, and about to go back to sleep. The kernel
// grants such a wake CPU and Network but not Graphics; a full wake has all
// of them.
//
// Nothing is using the data link then, and the dongle's link is down only
// because the bus is suspending and resuming under it; it will drop again at
// the next sleep whatever is done. A failure to read the state counts as a
// full wake, so a broken pmset never stops a real recovery.
func inDarkWake() bool {
	out, err := exec.Command(pmsetPath, "-g", "systemstate").Output()
	if err != nil {
		return false
	}
	return parseDarkWake(string(out))
}

// parseDarkWake reads `pmset -g systemstate`, whose capability line is
// "Current System Capabilities are: CPU Graphics Audio Network" in a full
// wake and lacks Graphics in a dark one.
func parseDarkWake(out string) bool {
	const prefix = "Current System Capabilities are:"
	for _, line := range strings.Split(out, "\n") {
		caps, ok := strings.CutPrefix(strings.TrimSpace(line), prefix)
		if !ok {
			continue
		}
		f := strings.Fields(caps)
		hasCPU, hasGraphics := false, false
		for _, c := range f {
			hasCPU = hasCPU || c == "CPU"
			hasGraphics = hasGraphics || c == "Graphics"
		}
		return hasCPU && !hasGraphics
	}
	return false
}
