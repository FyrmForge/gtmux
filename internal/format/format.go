// Package format expands a small tmux-like #{...} format language shared by
// the client status bar and the server's info commands (display-message,
// list-panes):
//
//	##            a literal #
//	#{var}        a variable from the vars map ("" if absent)
//	#{?var,a,b}   a if var is non-empty else b; branches may nest #{...}
//	#{b:var}      basename of var; #{d:var} dirname
//	#{=N:var}     truncate to first N chars; #{=-N:var} last N; modifiers stack
//	#{t:var}      var read as unix seconds, formatted (default, or #{t:var;%H:%M})
//
// Unknown #x sequences (e.g. the client's #client(...)/#server(...)) are left
// for the caller's own expander; this package only owns the pure core. #()
// shell substitution is deliberately absent — this package runs no shell.
package format

import (
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// LoopFunc returns the per-item variable maps for a loop kind — "S" (sessions),
// "W" (windows), "P" (panes) — used to expand #{S:...}/#{W:...}/#{P:...}. nil (or
// a nil return) means loops expand to "".
type LoopFunc func(kind string) []map[string]string

// Expand substitutes the #{...} forms in format using vars (no loops).
func Expand(format string, vars map[string]string) string {
	return ExpandLoop(format, vars, nil)
}

// ExpandLoop is Expand with a provider for the S/W/P iteration loops.
func ExpandLoop(format string, vars map[string]string, loop LoopFunc) string {
	var b strings.Builder
	for i := 0; i < len(format); {
		if format[i] != '#' || i+1 >= len(format) {
			b.WriteByte(format[i])
			i++
			continue
		}
		switch {
		case format[i+1] == '#':
			b.WriteByte('#')
			i += 2
		case format[i+1] == '{':
			body, next := MatchDelim(format, i+2, '{', '}')
			if next < 0 {
				b.WriteByte(format[i])
				i++
				continue
			}
			b.WriteString(expandBrace(body, vars, loop))
			i = next
		default:
			b.WriteByte(format[i])
			i++
		}
	}
	return b.String()
}

// mergeVars overlays item onto outer (item wins), for a loop iteration's scope.
func mergeVars(outer, item map[string]string) map[string]string {
	m := make(map[string]string, len(outer)+len(item))
	for k, v := range outer {
		m[k] = v
	}
	for k, v := range item {
		m[k] = v
	}
	return m
}

// expandBrace handles one #{...} body: a plain variable, a #{?var,then,else}
// conditional, an operator/modifier, or an S/W/P loop.
func expandBrace(body string, vars map[string]string, loop LoopFunc) string {
	// #{S:...} / #{W:...} / #{P:...}: expand the template once per item, each with
	// that item's vars merged over the outer scope, and concatenate. ponytail:
	// flat only — a nested loop reuses the same provider, not the item's children.
	if loop != nil && len(body) >= 2 && body[1] == ':' && (body[0] == 'S' || body[0] == 'W' || body[0] == 'P') {
		var b strings.Builder
		for _, item := range loop(string(body[0])) {
			b.WriteString(ExpandLoop(body[2:], mergeVars(vars, item), loop))
		}
		return b.String()
	}
	// #{t:var} / #{t:var;strftime}: read var as unix seconds and format it, with a
	// custom strftime layout after ';' (else the default ANSIC). The spec follows
	// the var — not before the ':' — so a "%H:%M" spec's colons don't mis-split.
	if strings.HasPrefix(body, "t:") {
		arg := body[2:]
		varPart, spec := arg, ""
		if i := strings.IndexByte(arg, ';'); i >= 0 {
			varPart, spec = arg[:i], arg[i+1:]
		}
		val := expandBrace(varPart, vars, loop)
		n, err := strconv.ParseInt(strings.TrimSpace(val), 10, 64)
		if err != nil {
			return val
		}
		layout := time.ANSIC
		if spec != "" {
			layout = strftimeToGo(spec)
		}
		return time.Unix(n, 0).Format(layout)
	}
	if op, rest, ok := splitOp(body); ok {
		return applyOp(op, rest, vars, loop)
	}
	if mod, rest, ok := splitModifier(body); ok {
		return applyModifier(mod, expandBrace(rest, vars, loop))
	}
	if !strings.HasPrefix(body, "?") {
		return vars[body]
	}
	parts := SplitTopLevel(body[1:])
	if len(parts) != 3 {
		return ""
	}
	// The condition is a bare variable name (tested non-empty) or a nested
	// format/operator like #{==:...} (expanded, then truthy = non-empty and
	// not "0"). Only the latter contains "#{", so switch on that.
	cond := parts[0]
	var t bool
	if strings.Contains(cond, "#{") {
		v := ExpandLoop(cond, vars, loop)
		t = v != "" && v != "0"
	} else {
		t = vars[cond] != ""
	}
	branch := parts[2]
	if t {
		branch = parts[1]
	}
	return ExpandLoop(branch, vars, loop)
}

// splitModifier detects a leading `mod:` on a #{...} body — one of b, d, or
// =N/=-N (truncate). It returns the modifier, the remaining body, and whether
// a modifier matched. A plain var name has no ':' so it never matches.
func splitModifier(body string) (mod, rest string, ok bool) {
	c := strings.IndexByte(body, ':')
	if c <= 0 {
		return "", "", false
	}
	m := body[:c]
	switch {
	case m == "b" || m == "d" || m == "n":
	case strings.HasPrefix(m, "="):
		if _, err := strconv.Atoi(m[1:]); err != nil {
			return "", "", false
		}
	default:
		return "", "", false
	}
	return m, body[c+1:], true
}

// strftimeToGo translates the common strftime directives to a Go reference-time
// layout. Unknown directives pass through literally.
var strftimeRepl = strings.NewReplacer(
	"%Y", "2006", "%y", "06", "%m", "01", "%d", "02",
	"%H", "15", "%I", "03", "%M", "04", "%S", "05",
	"%p", "PM", "%b", "Jan", "%B", "January", "%a", "Mon", "%A", "Monday",
	"%Z", "MST", "%z", "-0700", "%%", "%",
)

func strftimeToGo(spec string) string { return strftimeRepl.Replace(spec) }

// applyModifier runs one modifier over an already-expanded value.
func applyModifier(mod, val string) string {
	switch {
	case mod == "b":
		return filepath.Base(val)
	case mod == "d":
		return filepath.Dir(val)
	case mod == "n":
		// length of the variable's value, in runes
		return strconv.Itoa(len([]rune(val)))
	default: // =N / =-N truncate, by rune (ponytail: not wcwidth — fine until CJK)
		n, _ := strconv.Atoi(mod[1:])
		r := []rune(val)
		if n >= 0 {
			if n < len(r) {
				return string(r[:n])
			}
			return val
		}
		if -n < len(r) {
			return string(r[len(r)+n:])
		}
		return val
	}
}

// MatchDelim returns the text between position start and the matching close
// delimiter (depth-counted, so nested delimiters work), plus the index just
// past it. next is -1 if the close is missing.
func MatchDelim(s string, start int, open, close byte) (body string, next int) {
	depth := 1
	for i := start; i < len(s); i++ {
		switch s[i] {
		case open:
			depth++
		case close:
			depth--
			if depth == 0 {
				return s[start:i], i + 1
			}
		}
	}
	return "", -1
}

// SplitTopLevel splits s on commas that aren't inside a nested #{...}.
func SplitTopLevel(s string) []string {
	var parts []string
	depth, start := 0, 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '{':
			depth++
		case '}':
			depth--
		case ',':
			if depth == 0 {
				parts = append(parts, s[start:i])
				start = i + 1
			}
		}
	}
	return append(parts, s[start:])
}
