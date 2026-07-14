package client

import (
	"os/exec"
	"strings"
	"time"

	fmtx "github.com/FyrmForge/gtmux/internal/format"
)

// statusExpander expands status_left/status_right format strings client-side:
//
//	#{var}          a variable from the per-tick data map (server-provided)
//	#{?var,a,b}     a if var is non-empty else b; branches may contain #{...}
//	#client(cmd)    first line of `sh -c cmd` run on THIS client, cached
//	#server(cmd)    the server's output for cmd (streamed in ServerShell)
//	##              a literal #
//	#(...)          bare form has no side declared — ignored
//
// One per compositor. #client() output is cached for interval; #server()
// output is whatever the server last streamed (it caches server-side).
type statusExpander struct {
	interval time.Duration
	cache    map[string]shellResult // #client(cmd) local cache
}

type shellResult struct {
	out string
	at  time.Time
}

func newStatusExpander(interval time.Duration) *statusExpander {
	return &statusExpander{interval: interval, cache: map[string]shellResult{}}
}

func (e *statusExpander) expand(format string, vars, serverShell map[string]string) string {
	var b strings.Builder
	for i := 0; i < len(format); {
		if format[i] != '#' || i+1 >= len(format) {
			b.WriteByte(format[i])
			i++
			continue
		}
		rest := format[i+1:]
		switch {
		case format[i+1] == '#':
			b.WriteByte('#')
			i += 2
		case format[i+1] == '{':
			body, next := fmtx.MatchDelim(format, i+2, '{', '}')
			if next < 0 {
				b.WriteByte(format[i])
				i++
				continue
			}
			b.WriteString(e.expandBrace(body, vars, serverShell))
			i = next
		case strings.HasPrefix(rest, "client("):
			body, next := fmtx.MatchDelim(format, i+1+len("client("), '(', ')')
			if next < 0 {
				b.WriteByte(format[i])
				i++
				continue
			}
			b.WriteString(e.clientShell(body))
			i = next
		case strings.HasPrefix(rest, "server("):
			body, next := fmtx.MatchDelim(format, i+1+len("server("), '(', ')')
			if next < 0 {
				b.WriteByte(format[i])
				i++
				continue
			}
			b.WriteString(serverShell[body])
			i = next
		case format[i+1] == '(':
			// bare #(...): no side declared, so ignore it (consume the body).
			_, next := fmtx.MatchDelim(format, i+2, '(', ')')
			if next < 0 {
				b.WriteByte(format[i])
				i++
				continue
			}
			i = next
		default:
			b.WriteByte(format[i])
			i++
		}
	}
	return b.String()
}

// expandBrace handles one #{...} body: a plain variable, or a
// #{?var,then,else} conditional whose branches are expanded recursively.
func (e *statusExpander) expandBrace(body string, vars, serverShell map[string]string) string {
	// Bodies with no nested shell substitution reuse the pure format engine, so
	// its modifiers (b/d/t/n/=N) and operators (==, ||, m, e, …) work in the
	// status/window formats too. Bodies that nest #client()/#server()/#() keep the
	// shell-aware recursive path below.
	if !strings.Contains(body, "#client(") && !strings.Contains(body, "#server(") && !strings.Contains(body, "#(") {
		return fmtx.Expand("#{"+body+"}", vars)
	}
	if !strings.HasPrefix(body, "?") {
		return vars[body]
	}
	parts := fmtx.SplitTopLevel(body[1:])
	if len(parts) != 3 {
		return ""
	}
	branch := parts[2]
	if vars[parts[0]] != "" {
		branch = parts[1]
	}
	return e.expand(branch, vars, serverShell)
}

// clientShell runs a #client(cmd) locally, serving cached output if it ran
// within the last interval. ponytail: synchronous on the decode goroutine —
// fine for subsecond commands; make it async if a slow one stalls redraws.
func (e *statusExpander) clientShell(cmd string) string {
	if r, ok := e.cache[cmd]; ok && time.Since(r.at) < e.interval {
		return r.out
	}
	out, _ := exec.Command("sh", "-c", cmd).Output()
	line := firstLine(out)
	e.cache[cmd] = shellResult{out: line, at: time.Now()}
	return line
}

// extractServerCmds returns the deduplicated bodies of every #server(cmd) in
// the given formats, so the client can tell the server which commands to run
// on its behalf (the server no longer holds the formats).
func extractServerCmds(formats ...string) []string {
	var cmds []string
	seen := map[string]bool{}
	for _, f := range formats {
		for i := 0; i < len(f); {
			if f[i] == '#' && strings.HasPrefix(f[i+1:], "server(") {
				body, next := fmtx.MatchDelim(f, i+1+len("server("), '(', ')')
				if next < 0 {
					i++
					continue
				}
				if !seen[body] {
					seen[body] = true
					cmds = append(cmds, body)
				}
				i = next
				continue
			}
			i++
		}
	}
	return cmds
}

// firstLine trims a command's output to its first line, whitespace-trimmed.
func firstLine(out []byte) string {
	line := strings.TrimSpace(string(out))
	if nl := strings.IndexByte(line, '\n'); nl >= 0 {
		line = line[:nl]
	}
	return line
}
