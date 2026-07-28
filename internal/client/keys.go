package client

import "strings"

// extended-key encodings the client decodes from the outer terminal so modified
// keys with no legacy byte (Ctrl+1, …) can fire binds. The form decides what to
// do with a key NO bind consumes:
//   - formKitty (CSI code;mods u): only arrives while the outer terminal is in
//     kitty-keyboard mode for a kitty pane → forward the raw bytes to that app.
//   - formMOK (CSI 27;mods;code ~, xterm modifyOtherKeys=1): only arrives while
//     we've put a legacy pane's terminal into modifyOtherKeys → the app can't
//     represent the key, so an unbound one is dropped rather than forwarded raw.
const (
	formNone = iota
	formKitty
	formMOK
)

// decodeExtKey parses a CSI body (bytes after "ESC [", final byte included) into
// a canonical bind token matching config.parseKeyName, recognizing the kitty
// CSI-u form (…u) and the xterm modifyOtherKeys form (27;mods;code~). ok is false
// for any other CSI (arrows, F-keys, …) so the caller falls back to csiKeyName.
func decodeExtKey(seq string) (token string, code, mods, form int, ok bool) {
	if len(seq) < 2 {
		return "", 0, 0, formNone, false
	}
	body := seq[:len(seq)-1]
	switch seq[len(seq)-1] {
	case 'u': // kitty: code[;mods[:event]][;text]
		f := strings.Split(body, ";")
		code, mods = atoiDefault(f[0], -1), 1
		if len(f) > 1 {
			mods = atoiDefault(before(f[1], ':'), 1)
		}
		form = formKitty
	case '~': // xterm modifyOtherKeys=1: 27;mods;code
		f := strings.Split(body, ";")
		if len(f) != 3 || f[0] != "27" {
			return "", 0, 0, formNone, false
		}
		mods, code = atoiDefault(f[1], 1), atoiDefault(f[2], -1)
		form = formMOK
	default:
		return "", 0, 0, formNone, false
	}
	if code < 0 {
		return "", 0, 0, formNone, false
	}
	return keyToken(code, mods), code, mods, form, true
}

// mokLegacyBytes returns the legacy byte encoding for a modifyOtherKeys key that
// NO bind consumed, so a legacy app still receives keys it can actually
// represent — Alt+letter as ESC+letter (readline motions), plain/Ctrl-letter as
// their C0 byte. Returns nil for combos with no legacy form (Ctrl+digit, …),
// which are then dropped rather than forwarded as a garbage CSI sequence.
// Defensive: modifyOtherKeys=1 should only emit sequences for the nil cases, but
// terminals vary, so we down-convert the representable ones rather than trust it.
func mokLegacyBytes(code, mods int) []byte {
	m := 0
	if mods > 0 {
		m = mods - 1
	}
	ctrl, alt, shift := m&4 != 0, m&2 != 0, m&1 != 0
	if code < 0x20 || code > 0x7e { // only handle printable ASCII bases
		return nil
	}
	ch := byte(code)
	switch {
	case ctrl && ch|0x20 >= 'a' && ch|0x20 <= 'z': // Ctrl+letter -> C0 byte
		return []byte{ch & 0x1f}
	case ctrl: // Ctrl+digit/symbol: no legacy encoding
		return nil
	case alt: // Alt+key -> ESC + key (metaSendsEscape)
		if shift {
			ch = byte(strings.ToUpper(string(ch))[0])
		}
		return []byte{0x1b, ch}
	default: // plain / shift-only
		if shift {
			ch = byte(strings.ToUpper(string(ch))[0])
		}
		return []byte{ch}
	}
}

// keyToken builds a bind token from a Unicode key code and a kitty/xterm modifier
// value (bitfield+1: 1=shift 2=alt 4=ctrl …). Prefix order C- then M- matches
// config.parseKeyName; Shift uppercases a letter base. Combos parseKeyName can't
// express (e.g. C-M-) simply won't match any bind.
func keyToken(code, mods int) string {
	m := 0
	if mods > 0 {
		m = mods - 1
	}
	base := string(rune(code))
	if m&1 != 0 && len(base) == 1 && base[0] >= 'a' && base[0] <= 'z' { // shift
		base = strings.ToUpper(base)
	}
	tok := ""
	if m&4 != 0 { // ctrl
		tok += "C-"
	}
	if m&2 != 0 { // alt
		tok += "M-"
	}
	return tok + base
}

func atoiDefault(s string, d int) int {
	if s == "" {
		return d
	}
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return d
		}
		n = n*10 + int(c-'0')
	}
	return n
}

func before(s string, sep byte) string {
	if i := strings.IndexByte(s, sep); i >= 0 {
		return s[:i]
	}
	return s
}

// decodeKeys converts a raw input chunk into a sequence of key-name strings for a
// modal widget's on_key handler. It reuses the reader's existing tables so names
// match what users bind: escape sequences via csiKeyName/ss3KeyName (Up, PgUp,
// F5, Home…), control/printable bytes via byteKey (C-c, letters), and friendly
// aliases for the common editing keys (Enter, Tab, BSpace, Escape).
//
// One read can carry several keys (fast typing / paste), so it returns a slice —
// the caller feeds each in order and re-renders once. Escape sequences split
// across reads (ESC[ in one, A in the next) aren't reassembled — the same
// limitation the copy/picker feeders already have; acceptable for v1.
func decodeKeys(data []byte) []string {
	var out []string
	for i := 0; i < len(data); i++ {
		b := data[i]
		if b == 0x1b { // ESC: CSI (ESC[) / SS3 (ESCO) sequence, or a bare Escape
			if i+1 < len(data) && (data[i+1] == '[' || data[i+1] == 'O') {
				intro := data[i+1]
				j := i + 2
				for j < len(data) && (data[j] < 0x40 || data[j] > 0x7e) {
					j++
				}
				if j < len(data) {
					seq := string(data[i+2 : j+1])
					name := csiKeyName[seq]
					if intro == 'O' {
						name = ss3KeyName[seq]
					}
					if name != "" {
						out = append(out, name)
					}
					i = j // consumed the whole sequence (unknown finals ignored)
					continue
				}
			}
			out = append(out, "Escape")
			continue
		}
		switch {
		case b == '\r' || b == '\n':
			out = append(out, "Enter")
		case b == '\t':
			out = append(out, "Tab")
		case b == 0x7f || b == 0x08:
			out = append(out, "BSpace")
		default:
			if k := byteKey(b); k != "" { // printables (the char) + C-<x>
				out = append(out, k)
			}
		}
	}
	return out
}
