package client

import (
	"strconv"
	"strings"

	"github.com/FyrmForge/gtmux/internal/emu"
	"github.com/FyrmForge/gtmux/internal/proto"
)

// prompt is a client-owned text-entry line (rename-window/session, command
// prompt). A keybind opens it from the client's own state (proto.OpenPrompt);
// editing is local; Enter commits the text back as an Action (see commit).
type prompt struct {
	kind    string // "session" | "window" | "command" | "confirm"
	buf     []byte
	labels  []string // custom labels (command-prompt -p, comma-split; confirm-before -p); one per prompt stage. empty = default
	answers []string // command-prompt: answers collected from stages already committed
	tmpl    []string // command-prompt template: %1..%N/%% substituted with the answers; nil = run text verbatim
	cmd     []string // confirm-before: command to run on y
	// editKeys enables emacs-style line editing (C-u kill line, C-w kill word);
	// set from the status-keys option. vi mode leaves it off — gtmux can't do
	// modal editing since ESC cancels the prompt.
	editKeys bool
}

func newPrompt(m *proto.OpenPrompt, editKeys bool) *prompt {
	return &prompt{kind: m.Kind, buf: []byte(m.Prefill), editKeys: editKeys}
}

// stages is how many prompts to collect before running the template — tmux's
// command-prompt -p "a,b" asks twice. One or zero labels is a single prompt.
func (p *prompt) stages() int {
	if len(p.labels) > 1 {
		return len(p.labels)
	}
	return 1
}

// advance records the current input as one answer and clears the buffer for the
// next prompt. Returns true if more stages remain (stay open), false when this
// was the last (the caller commits). command-prompt only.
func (p *prompt) advance() bool {
	p.answers = append(p.answers, string(p.buf))
	p.buf = p.buf[:0]
	return len(p.answers) < p.stages()
}

// label is the status-bar tag shown before the typed text — the current stage's
// custom label if any, else the default for the kind.
func (p *prompt) label() string {
	if s := len(p.answers); s < len(p.labels) {
		return p.labels[s]
	}
	switch p.kind {
	case "session":
		return "rename-session"
	case "window":
		return "rename-window"
	case "command":
		return ":"
	}
	return p.kind
}

// commit maps the typed text to a runCommand action, or nil to cancel (empty).
func (p *prompt) commit() []string {
	text := string(p.buf)
	switch p.kind {
	case "session":
		if text == "" {
			return nil
		}
		return []string{"rename-session", text}
	case "window":
		if text == "" {
			return nil
		}
		return []string{"rename-window", text}
	case "command":
		if p.tmpl != nil {
			// %N carries the Nth prompt's answer (%1 as its own field takes the
			// whole answer, spaces included; embedded %N substituted in place);
			// %% is the first answer, for back-compat with single-prompt binds.
			// Empty input is allowed — tmux still runs the template.
			out := make([]string, len(p.tmpl))
			for i, f := range p.tmpl {
				out[i] = substituteAnswers(f, p.answers)
			}
			return out
		}
		// No template: run the (single) typed line verbatim.
		if len(p.answers) == 0 || p.answers[0] == "" {
			return nil
		}
		return tokenize(p.answers[0])
	}
	return nil
}

// substituteAnswers replaces %1..%9 in f with the respective prompt answers and
// %% with the first — command-prompt template expansion. Highest index first so
// %1 doesn't clobber a %1x (only %1..%9 are supported, tmux's range).
func substituteAnswers(f string, answers []string) string {
	for i := len(answers) - 1; i >= 0 && i < 9; i-- {
		f = strings.ReplaceAll(f, "%"+strconv.Itoa(i+1), answers[i])
	}
	if len(answers) > 0 {
		f = strings.ReplaceAll(f, "%%", answers[0])
	}
	return f
}

// tokenize splits a command line into words like a shell: whitespace separates,
// '...' and "..." group (spaces inside stay in one word), \x escapes the next
// byte. Enough for command-prompt input and choose-*/display-menu targets —
// not a full shell parser (no variable/glob expansion).
func tokenize(s string) []string {
	var out []string
	var cur strings.Builder
	inWord := false
	for i := 0; i < len(s); {
		c := s[i]
		switch c {
		case ' ', '\t':
			if inWord {
				out = append(out, cur.String())
				cur.Reset()
				inWord = false
			}
			i++
		case '\'':
			inWord = true
			for i++; i < len(s) && s[i] != '\''; i++ {
				cur.WriteByte(s[i])
			}
			i++ // skip closing quote
		case '"':
			inWord = true
			for i++; i < len(s) && s[i] != '"'; i++ {
				if s[i] == '\\' && i+1 < len(s) {
					i++
				}
				cur.WriteByte(s[i])
			}
			i++
		case '\\':
			inWord = true
			if i++; i < len(s) {
				cur.WriteByte(s[i])
				i++
			}
		default:
			inWord = true
			cur.WriteByte(c)
			i++
		}
	}
	if inWord {
		out = append(out, cur.String())
	}
	return out
}

// promptResult reports the outcome of a chunk of prompt input.
type promptResult struct {
	done   bool     // prompt closed (committed or cancelled)
	action []string // non-empty = run this command on the server
}

func (p *prompt) feed(data []byte) promptResult {
	if p.kind == "confirm" {
		// Only y/Y runs the command; every other key (n, ESC, q, and crucially
		// Enter) cancels — matches tmux, and stops confirm-before kill-window
		// from firing on a stray return.
		for _, b := range data {
			if b == 'y' || b == 'Y' {
				return promptResult{done: true, action: p.cmd}
			}
			return promptResult{done: true}
		}
		return promptResult{}
	}
	for _, b := range data {
		switch b {
		case '\r', '\n':
			// command-prompt may have more stages to collect (tmux -p "a,b");
			// advance records this answer and keeps the overlay open for the next.
			if p.kind == "command" && p.advance() {
				return promptResult{}
			}
			return promptResult{done: true, action: p.commit()}
		case 0x1b:
			return promptResult{done: true}
		case 0x7f, 0x08: // DEL / backspace
			if len(p.buf) > 0 {
				p.buf = p.buf[:len(p.buf)-1]
			}
		case 0x15: // C-u: kill the whole line (emacs status-keys)
			if p.editKeys {
				p.buf = p.buf[:0]
			}
		case 0x17: // C-w: kill the last word (emacs status-keys)
			if p.editKeys {
				n := len(p.buf)
				for n > 0 && p.buf[n-1] == ' ' {
					n--
				}
				for n > 0 && p.buf[n-1] != ' ' {
					n--
				}
				p.buf = p.buf[:n]
			}
		default:
			if b >= 0x20 && b < 0x7f {
				p.buf = append(p.buf, b)
			}
		}
	}
	return promptResult{}
}

// picker is a client-owned selection list (choose-window / choose-session).
// The server opens it (proto.OpenPicker); navigation is local; Enter sends
// {Verb, Target} back as an Action. Ported from the server's chooseKind state.
type picker struct {
	title   string
	verb    string // "select-window" | "switch-session" | "run"
	items   []string
	targets []string
	sel     int
	// filterable (choose-tree): printable keys build filter and narrow the list
	// live, arrows navigate. When off, j/k navigate and typing is inert.
	filterable bool
	filter     string
	previews   [][]emu.Line // per-item static styled preview (selected item's pane), or nil
}

func newPicker(m *proto.OpenPicker) *picker {
	return &picker{title: m.Title, verb: m.Verb, items: m.Items, targets: m.Targets, filterable: m.Filter, previews: m.Previews, sel: m.Sel}
}

// view returns the original indices of the items matching the current filter
// (all of them when not filtering). sel indexes into this slice.
func (p *picker) view() []int {
	idx := make([]int, 0, len(p.items))
	f := strings.ToLower(p.filter)
	for i, it := range p.items {
		if f == "" || strings.Contains(strings.ToLower(it), f) {
			idx = append(idx, i)
		}
	}
	return idx
}

func (p *picker) move(d int) {
	n := len(p.view())
	p.sel += d
	if p.sel < 0 {
		p.sel = 0
	}
	if p.sel >= n {
		p.sel = n - 1
	}
	if p.sel < 0 {
		p.sel = 0
	}
}

func (p *picker) commit() pickerResult {
	v := p.view()
	if p.sel < 0 || p.sel >= len(v) {
		return pickerResult{done: true}
	}
	t := p.targets[v[p.sel]]
	// display-menu/choose-tree: the target IS the command line (verb "run").
	// choose-*: {verb, target} — a two-word select command.
	if p.verb == "run" {
		return pickerResult{done: true, action: tokenize(t)}
	}
	return pickerResult{done: true, action: []string{p.verb, t}}
}

// pickerResult reports the outcome of a chunk of picker input.
type pickerResult struct {
	done   bool
	action []string
}

func (p *picker) feed(data []byte) pickerResult {
	for i := 0; i < len(data); i++ {
		b := data[i]
		// Arrow keys via in-chunk CSI lookahead. Navigation works in both modes;
		// in filterable mode it's the ONLY nav (letters build the filter instead).
		if b == 0x1b && i+1 < len(data) && data[i+1] == '[' {
			j := i + 2
			for j < len(data) && (data[j] < 0x40 || data[j] > 0x7e) {
				j++
			}
			if j < len(data) {
				switch string(data[i+2 : j+1]) {
				case "A":
					p.move(-1)
				case "B":
					p.move(1)
				}
				i = j
				continue
			}
		}
		if p.filterable {
			switch {
			case b == '\r' || b == '\n':
				return p.commit()
			case b == 0x1b: // bare ESC cancels
				return pickerResult{done: true}
			case b == 0x7f || b == 0x08: // backspace edits the filter
				if p.filter != "" {
					p.filter = p.filter[:len(p.filter)-1]
					p.sel = 0
				}
			case b >= 0x20 && b < 0x7f:
				p.filter += string(b)
				p.sel = 0
			}
			continue
		}
		switch b {
		case 'j':
			p.move(1)
		case 'k':
			p.move(-1)
		case '\r', '\n':
			return p.commit()
		case 'q', 0x1b:
			return pickerResult{done: true}
		}
	}
	return pickerResult{}
}
