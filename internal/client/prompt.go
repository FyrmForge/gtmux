package client

import (
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/FyrmForge/gtmux/internal/emu"
	"github.com/FyrmForge/gtmux/internal/proto"
)

// prompt is a client-owned text-entry line (rename-window/session, command
// prompt). A keybind opens it from the client's own state (proto.OpenPrompt);
// editing is local; Enter commits the text back as an Action (see commit).
type prompt struct {
	kind    string // "session" | "window" | "command" | "confirm"
	buf     []byte
	back    int      // cursor: bytes from the end of buf (0 = at the end, the zero value)
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

// question is the current stage's custom label (command-prompt -p), shown on
// the prompt box's top border; "" when the prompt has none.
func (p *prompt) question() string {
	if s := len(p.answers); s < len(p.labels) {
		return p.labels[s]
	}
	return ""
}

// origin names what the prompt will run, shown before the typed text
// ("rename-window: foo") however it was opened — keybind or typed command.
func (p *prompt) origin() string {
	switch {
	case p.kind == "session":
		return "rename-session"
	case p.kind == "window":
		return "rename-window"
	case len(p.tmpl) > 0:
		return p.tmpl[0]
	}
	return "command"
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
	for i := 0; i < len(data); i++ {
		b := data[i]
		// Cursor keys arrive as ESC [ x / ESC O x; read them in-chunk so the ESC
		// doesn't cancel. Unknown sequences are swallowed.
		if b == 0x1b && i+1 < len(data) && (data[i+1] == '[' || data[i+1] == 'O') {
			j := i + 2
			for j < len(data) && (data[j] < 0x40 || data[j] > 0x7e) {
				j++
			}
			if j < len(data) {
				switch string(data[i+2 : j+1]) {
				case "D":
					p.left()
				case "C":
					p.right()
				case "H", "1~", "7~":
					p.back = len(p.buf)
				case "F", "4~", "8~":
					p.back = 0
				case "3~": // Delete: the rune under the cursor
					if p.back > 0 {
						p.right()
						p.backspace()
					}
				}
				i = j
				continue
			}
		}
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
			p.backspace()
		case 0x01: // C-a / C-e: start / end of line (emacs status-keys)
			if p.editKeys {
				p.back = len(p.buf)
			}
		case 0x05:
			if p.editKeys {
				p.back = 0
			}
		case 0x15: // C-u: kill everything before the cursor (emacs status-keys)
			if p.editKeys {
				p.buf = p.buf[len(p.buf)-p.back:]
			}
		case 0x17: // C-w: kill the word before the cursor (emacs status-keys)
			if p.editKeys {
				at := len(p.buf) - p.back
				n := at
				for n > 0 && p.buf[n-1] == ' ' {
					n--
				}
				for n > 0 && p.buf[n-1] != ' ' {
					n--
				}
				p.buf = append(p.buf[:n], p.buf[at:]...)
			}
		default:
			if b >= 0x20 && b < 0x7f {
				at := len(p.buf) - p.back
				p.buf = append(p.buf[:at], append([]byte{b}, p.buf[at:]...)...)
			}
		}
	}
	return promptResult{}
}

// head is the text before the cursor.
func (p *prompt) head() []byte { return p.buf[:len(p.buf)-p.back] }

func (p *prompt) left() {
	if _, n := utf8.DecodeLastRune(p.head()); n > 0 {
		p.back += n
	}
}

func (p *prompt) right() {
	if _, n := utf8.DecodeRune(p.buf[len(p.buf)-p.back:]); n > 0 {
		p.back -= n
	}
}

func (p *prompt) backspace() {
	h := p.head()
	if _, n := utf8.DecodeLastRune(h); n > 0 {
		p.buf = append(h[:len(h)-n], p.buf[len(h):]...)
	}
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
	descs      []string     // per-item dim description column (command palette), or nil
	palette    *palette     // command-palette mode (prefix+:), or nil — see palette.go
}

func newPicker(m *proto.OpenPicker) *picker {
	return &picker{title: m.Title, verb: m.Verb, items: m.Items, targets: m.Targets, filterable: m.Filter, previews: m.Previews, sel: m.Sel}
}

// view returns the original indices of the items matching the current filter
// (all of them when not filtering). sel indexes into this slice. The match is
// fuzzy: substring hits first, then subsequence-only hits ("gtx" ~ "gtmux"),
// each group in list order.
func (p *picker) view() []int {
	idx := make([]int, 0, len(p.items))
	var fuzzy []int
	q := p.query()
	for i, it := range p.items {
		if pos, sub := matchPos(q, it); sub {
			idx = append(idx, i)
		} else if pos != nil {
			fuzzy = append(fuzzy, i)
		}
	}
	return append(idx, fuzzy...)
}

// query is the part of the filter the items are matched against: all of it,
// except in palette stage two, where it's the argument after the command.
func (p *picker) query() string {
	if p.palette != nil {
		return p.filter[min(p.palette.off, len(p.filter)):]
	}
	return p.filter
}

// matchPos returns the rune indexes of s that q matches, case-insensitively:
// the first contiguous run when q is a substring (sub=true), else the leftmost
// subsequence; nil when neither. An empty q is a substring of everything.
func matchPos(q, s string) (pos []int, sub bool) {
	lower := func(x string) []rune {
		r := []rune(x)
		for i := range r {
			r[i] = unicode.ToLower(r[i]) // per rune, so indexes stay aligned with s
		}
		return r
	}
	qr, sr := lower(q), lower(s)
	if len(qr) == 0 {
		return nil, true
	}
	for i := 0; i+len(qr) <= len(sr); i++ {
		if string(sr[i:i+len(qr)]) == string(qr) {
			for j := range qr {
				pos = append(pos, i+j)
			}
			return pos, true
		}
	}
	for i := 0; i < len(sr) && len(pos) < len(qr); i++ {
		if sr[i] == qr[len(pos)] {
			pos = append(pos, i)
		}
	}
	if len(pos) < len(qr) {
		return nil, false
	}
	return pos, false
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
			if p.palette != nil {
				switch b {
				case '\r', '\n':
					if res := p.palette.commit(p); res.done {
						res.action = confirmKill(res.action)
						return res
					}
					continue
				case '\t':
					p.palette.complete(p)
					continue
				}
			}
			switch {
			case b == '\r' || b == '\n':
				return p.commit()
			case b == 0x1b: // bare ESC cancels
				return pickerResult{done: true}
			case b == 0x0e: // C-n / C-p navigate (fzf); letters go to the filter
				p.move(1)
			case b == 0x10:
				p.move(-1)
			case b == 0x7f || b == 0x08: // backspace edits the filter
				if p.filter != "" {
					p.filter = p.filter[:len(p.filter)-1]
					p.sel = 0
				}
			case b >= 0x20 && b < 0x7f:
				p.filter += string(b)
				p.sel = 0
			}
			if p.palette != nil {
				p.palette.refresh(p) // the stage can change with every keystroke
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

// splitSeq splits an action on lone ";" tokens into its commands, dropping
// empty ones; no ";" returns it as the only command.
func splitSeq(act []string) [][]string {
	var out [][]string
	start := 0
	for i := 0; i <= len(act); i++ {
		if i == len(act) || act[i] == ";" {
			if i > start {
				out = append(out, act[start:i])
			}
			start = i + 1
		}
	}
	return out
}
