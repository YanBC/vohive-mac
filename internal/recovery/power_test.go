package recovery

import (
	"testing"
	"time"
)

func TestParseDarkWake(t *testing.T) {
	for _, tc := range []struct {
		name string
		out  string
		want bool
	}{
		{"full wake", "Current System Capabilities are: CPU Graphics Audio Network \nCurrent Power State: 4\n", false},
		{"dark wake", "Current System Capabilities are: CPU Network \nCurrent Power State: 4\n", true},
		{"dark wake without network", "Current System Capabilities are: CPU\n", true},
		{"unreadable output counts as full wake", "pmset: unknown option\n", false},
		{"empty capability list", "Current System Capabilities are: \n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseDarkWake(tc.out); got != tc.want {
				t.Fatalf("parseDarkWake = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestDownThreshold(t *testing.T) {
	now := time.Unix(10_000, 0)
	for _, tc := range []struct {
		name   string
		wokeAt time.Time
		want   time.Duration
	}{
		{"no wake seen", time.Time{}, linkDownThreshold},
		{"just woke", now.Add(-10 * time.Second), postWakeThreshold},
		{"woke long ago", now.Add(-2 * time.Minute), linkDownThreshold},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := downThreshold(now, tc.wokeAt); got != tc.want {
				t.Fatalf("downThreshold = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestSleptBetween(t *testing.T) {
	for _, tc := range []struct {
		name       string
		wall, mono time.Duration
		want       bool
	}{
		{"awake tick", 3 * time.Second, 3 * time.Second, false},
		{"clock slew", 3*time.Second + time.Millisecond, 3 * time.Second, false},
		{"one-second doze", 4100 * time.Millisecond, 3 * time.Second, true},
		{"long sleep", 17 * time.Minute, 3 * time.Second, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := sleptBetween(tc.wall, tc.mono); got != tc.want {
				t.Fatalf("sleptBetween(%v, %v) = %v, want %v", tc.wall, tc.mono, got, tc.want)
			}
		})
	}
}
