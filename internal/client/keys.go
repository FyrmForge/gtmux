package client

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
