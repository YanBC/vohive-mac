// Package sim holds the identity of a SIM card, independent of how it is read
// or where it is stored.
//
// It sits below both the modem and the store so that neither has to depend on
// the other: the modem reads an Identity off the card over USB, and the store
// persists one — without the store (and its tests) pulling in libusb.
package sim

import "strings"

// Identity is the card currently in the dongle. ICCID is the identity: it is
// printed on the card and always readable. Number (AT+CNUM) is blank on most
// prepaid/MVNO SIMs — treat it as a label, never as a key.
type Identity struct {
	ICCID    string `json:"iccid"`
	IMSI     string `json:"imsi,omitempty"`
	Number   string `json:"number,omitempty"`
	Operator string `json:"operator,omitempty"`
}

// iccidTail is the abbreviated ICCID older builds used in generated labels.
// Nothing produces it any more — the whole point of showing a card its ICCID
// is telling two unnamed cards apart, and the last six digits of two ICCIDs
// from the same batch can be all that differs — but it is still the form
// stored in every row written before that change, so IsGeneratedLabel has to
// keep recognising it. Databases are rewritten to the full ICCID by the
// store's migration.
func (s Identity) iccidTail() string {
	if len(s.ICCID) > 6 {
		return "…" + s.ICCID[len(s.ICCID)-6:]
	}
	return s.ICCID
}

// DefaultLabel is what the UI shows for a SIM until the user renames it.
func (s Identity) DefaultLabel() string {
	if s.Number != "" {
		return s.Number
	}
	if s.Operator != "" {
		return s.Operator + " " + s.ICCID
	}
	return s.ICCID
}

// IsGeneratedLabel reports whether label is one this app could have produced
// for this card, rather than a name the user typed.
//
// It has to check every form DefaultLabel can emit, not just today's, because
// a label and the identity beside it are not written in lockstep: the identity
// is refreshed on every sighting, while the label is written once. A card
// first seen at its PIN prompt is labelled with its ICCID and then learns its
// number, so the two are legitimately out of step — comparing the label
// against the *current* default would read that placeholder as a user's choice
// and pin it forever.
func (s Identity) IsGeneratedLabel(label string) bool {
	if label == "" || label == s.ICCID || label == s.iccidTail() {
		return true
	}
	if s.Number != "" && label == s.Number {
		return true
	}
	if s.Operator == "" {
		return false
	}
	return label == s.Operator+" "+s.ICCID || label == s.Operator+" "+s.iccidTail()
}

// Digits keeps the characters an ICCID/IMSI may contain (ICCIDs are sometimes
// padded with 'F' nibbles) and drops AT framing noise.
func Digits(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == 'F' || r == 'f':
			b.WriteRune('F')
		}
	}
	return b.String()
}
