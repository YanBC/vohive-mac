// Minimal SMS-DELIVER PDU decoder: GSM7 / UCS2 / 8-bit, UDH-aware,
// with reassembly of concatenated messages.
package main

import (
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"unicode/utf16"
)

var gsm7Base = []rune(
	"@£$¥èéùìòÇ\nØø\rÅåΔ_ΦΓΛΩΠΨΣΘΞ\x1bÆæßÉ !\"#¤%&'()*+,-./0123456789:;<=>?" +
		"¡ABCDEFGHIJKLMNOPQRSTUVWXYZÄÖÑÜ§¿abcdefghijklmnopqrstuvwxyzäöñüà")

var gsm7Ext = map[byte]rune{
	0x0A: '\f', 0x14: '^', 0x28: '{', 0x29: '}', 0x2F: '\\',
	0x3C: '[', 0x3D: '~', 0x3E: ']', 0x40: '|', 0x65: '€',
}

type pduRecord struct {
	index     int
	sender    string
	timestamp string
	text      string
	concatRef int // -1 when not concatenated
	concatTot int
	concatSeq int
}

func swapNibbles(s string) string {
	b := []byte(s)
	for i := 0; i+1 < len(b); i += 2 {
		b[i], b[i+1] = b[i+1], b[i]
	}
	return strings.TrimRight(strings.TrimRight(string(b), "F"), "f")
}

func decodeGSM7(data []byte, septetCount, skipSeptets int) string {
	var codes []byte
	bits, nbits := 0, 0
	for _, by := range data {
		bits |= int(by) << nbits
		nbits += 8
		for nbits >= 7 {
			codes = append(codes, byte(bits&0x7F))
			bits >>= 7
			nbits -= 7
		}
	}
	if septetCount > len(codes) {
		septetCount = len(codes)
	}
	if skipSeptets > septetCount {
		skipSeptets = septetCount
	}
	codes = codes[skipSeptets:septetCount]
	var sb strings.Builder
	esc := false
	for _, c := range codes {
		switch {
		case esc:
			if r, ok := gsm7Ext[c]; ok {
				sb.WriteRune(r)
			} else {
				sb.WriteByte('?')
			}
			esc = false
		case c == 0x1B:
			esc = true
		case int(c) < len(gsm7Base):
			sb.WriteRune(gsm7Base[c])
		default:
			sb.WriteByte('?')
		}
	}
	return sb.String()
}

func decodePDU(pduHex string) (*pduRecord, error) {
	raw, err := hex.DecodeString(strings.TrimSpace(pduHex))
	if err != nil {
		return nil, fmt.Errorf("bad hex: %w", err)
	}
	rd := func(pos int) (byte, error) {
		if pos >= len(raw) {
			return 0, fmt.Errorf("truncated PDU")
		}
		return raw[pos], nil
	}

	pos := 0
	smscLen, err := rd(pos)
	if err != nil {
		return nil, err
	}
	pos += 1 + int(smscLen)
	fo, err := rd(pos)
	if err != nil {
		return nil, err
	}
	pos++
	hasUDH := fo&0x40 != 0

	addrLen, err := rd(pos)
	if err != nil {
		return nil, err
	}
	pos++
	addrType, err := rd(pos)
	if err != nil {
		return nil, err
	}
	pos++
	addrBytes := (int(addrLen) + 1) / 2
	if pos+addrBytes > len(raw) {
		return nil, fmt.Errorf("truncated sender")
	}
	addrRaw := raw[pos : pos+addrBytes]
	pos += addrBytes

	var sender string
	if addrType&0x70 == 0x50 { // alphanumeric, GSM7-packed
		sender = decodeGSM7(addrRaw, int(addrLen)*4/7, 0)
	} else {
		sender = swapNibbles(strings.ToUpper(hex.EncodeToString(addrRaw)))
		if len(sender) > int(addrLen) {
			sender = sender[:addrLen]
		}
		if addrType&0x70 == 0x10 {
			sender = "+" + sender
		}
	}

	pos++ // TP-PID
	dcs, err := rd(pos)
	if err != nil {
		return nil, err
	}
	pos++
	if pos+7 > len(raw) {
		return nil, fmt.Errorf("truncated timestamp")
	}
	ts := strings.ToUpper(hex.EncodeToString(raw[pos : pos+7]))
	pos += 7
	sw := func(i int) string { return swapNibbles(ts[i : i+2]) }
	timestamp := fmt.Sprintf("20%s-%s-%s %s:%s:%s", sw(0), sw(2), sw(4), sw(6), sw(8), sw(10))

	udl, err := rd(pos)
	if err != nil {
		return nil, err
	}
	pos++
	ud := raw[pos:]

	rec := &pduRecord{sender: sender, timestamp: timestamp, concatRef: -1}

	udhLen := 0
	if hasUDH && len(ud) > 0 {
		udhLen = int(ud[0]) + 1
		if udhLen > len(ud) {
			udhLen = len(ud)
		}
		for i := 1; i+1 < udhLen; {
			iei, ielen := ud[i], int(ud[i+1])
			if i+2+ielen > udhLen {
				break
			}
			switch {
			case iei == 0x00 && ielen == 3:
				rec.concatRef = int(ud[i+2])
				rec.concatTot = int(ud[i+3])
				rec.concatSeq = int(ud[i+4])
			case iei == 0x08 && ielen == 4:
				rec.concatRef = int(ud[i+2])<<8 | int(ud[i+3])
				rec.concatTot = int(ud[i+4])
				rec.concatSeq = int(ud[i+5])
			}
			i += 2 + ielen
		}
	}

	coding := 0 // 0=GSM7 1=8bit 2=UCS2
	switch {
	case dcs&0xC0 == 0x00:
		coding = int(dcs>>2) & 0x03
		if coding == 3 {
			coding = 0
		}
	case dcs&0xF0 == 0xE0:
		coding = 2
	case dcs&0xF0 == 0xF0:
		if dcs&0x04 != 0 {
			coding = 1
		}
	}

	switch coding {
	case 2:
		body := ud[udhLen:]
		codes := make([]uint16, 0, len(body)/2)
		for i := 0; i+1 < len(body); i += 2 {
			codes = append(codes, uint16(body[i])<<8|uint16(body[i+1]))
		}
		rec.text = string(utf16.Decode(codes))
	case 1:
		rec.text = "<binary: " + hex.EncodeToString(ud[udhLen:]) + ">"
	default:
		skip := 0
		if udhLen > 0 {
			skip = (udhLen*8 + 6) / 7
		}
		rec.text = decodeGSM7(ud, int(udl), skip)
	}
	return rec, nil
}

// reassemble merges concatenated-SMS parts into single messages, newest first.
func reassemble(msgs []pduRecord) []InboxMessage {
	type key struct {
		sender string
		ref    int
	}
	singles := make([]pduRecord, 0, len(msgs))
	groups := map[key]map[int]pduRecord{}
	for _, m := range msgs {
		if m.concatRef >= 0 {
			k := key{m.sender, m.concatRef}
			if groups[k] == nil {
				groups[k] = map[int]pduRecord{}
			}
			groups[k][m.concatSeq] = m
		} else {
			singles = append(singles, m)
		}
	}
	for _, parts := range groups {
		seqs := make([]int, 0, len(parts))
		total := 0
		for s, p := range parts {
			seqs = append(seqs, s)
			total = p.concatTot
		}
		sort.Ints(seqs)
		merged := parts[seqs[0]]
		var sb strings.Builder
		for _, s := range seqs {
			sb.WriteString(parts[s].text)
		}
		if len(parts) < total {
			fmt.Fprintf(&sb, " …[%d/%d parts]", len(parts), total)
		}
		merged.text = sb.String()
		singles = append(singles, merged)
	}
	sort.Slice(singles, func(i, j int) bool {
		return singles[i].timestamp > singles[j].timestamp
	})
	out := make([]InboxMessage, len(singles))
	for i, m := range singles {
		out[i] = InboxMessage{
			Index: m.index, Sender: m.sender,
			Timestamp: m.timestamp, Text: m.text,
		}
	}
	return out
}
