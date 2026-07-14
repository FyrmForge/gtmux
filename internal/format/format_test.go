package format

import (
	"testing"
	"time"
)

func TestExpandLoop(t *testing.T) {
	loop := func(kind string) []map[string]string {
		switch kind {
		case "W":
			return []map[string]string{
				{"window_index": "1", "window_name": "edit", "window_active": "1"},
				{"window_index": "2", "window_name": "shell", "window_active": ""},
			}
		case "P":
			return []map[string]string{{"pane_index": "0"}, {"pane_index": "1"}}
		}
		return nil
	}
	cases := []struct{ in, want string }{
		{"#{W:#{window_index}:#{window_name} }", "1:edit 2:shell "},
		{"#{P:[#{pane_index}]}", "[0][1]"},
		{"#{W:#{?window_active,*,-}}", "*-"},                 // conditional per item
		{"#{W:#{window_index} }#{P:#{pane_index}}", "1 2 01"}, // two flat loops
		{"#{S:x}", ""},                                       // provider returns nil for S
	}
	for _, c := range cases {
		if got := ExpandLoop(c.in, map[string]string{}, loop); got != c.want {
			t.Errorf("ExpandLoop(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	// A nil provider makes loops expand to nothing.
	if got := Expand("[#{W:#{window_index}}]", nil); got != "[]" {
		t.Errorf("nil-loop = %q, want %q", got, "[]")
	}
}

func TestExpand(t *testing.T) {
	vars := map[string]string{"session": "dev", "zoom": "1", "branch": "", "path": "/home/dev/gtmux", "uni": "héllo"}
	cases := []struct{ in, want string }{
		{"[#{session}]", "[dev]"},
		{"#{missing}", ""},
		{"##{session}", "#{session}"},
		{"#{?zoom,Z,-}", "Z"},
		{"#{?branch,on:#{branch},off}", "off"},
		{"#{?session,#{session},none}", "dev"},
		{"plain", "plain"},
		{"#{unclosed", "#{unclosed"},
		{"#{b:path}", "gtmux"},     // basename
		{"#{d:path}", "/home/dev"}, // dirname
		{"#{=3:session}", "dev"},   // truncate first N (N==len)
		{"#{=2:session}", "de"},    // truncate first N
		{"#{=-2:session}", "ev"},   // truncate last N
		{"#{=10:session}", "dev"},  // N > len -> unchanged
		{"#{=2:b:path}", "gt"},     // stacked: basename then truncate
		{"#{time:x}", ""},          // unknown modifier -> plain var (absent)
		{"#{=2:uni}", "hé"},        // truncate counts runes, not bytes
		{"#{=-3:uni}", "llo"},      // last-N by rune
		{"#{t:session}", "dev"},    // t: on a non-numeric value passes through
		{"#{n:session}", "3"},      // n: length of value in runes
		{"#{n:uni}", "5"},          // rune length (héllo = 5 runes)
		// comparison / logical operators return "1"/"0", usable in a conditional
		{"#{==:#{session},dev}", "1"},
		{"#{==:#{session},prod}", "0"},
		{"#{!=:#{session},prod}", "1"},
		{"#{<:2,10}", "1"},  // numeric, not lexical
		{"#{>:2,10}", "0"},
		{"#{>=:10,10}", "1"},
		{"#{||:#{branch},#{session}}", "1"}, // branch empty, session set
		{"#{||:,0}", "0"},
		{"#{&&:#{zoom},#{session}}", "1"},
		{"#{&&:#{zoom},#{branch}}", "0"},
		{"#{?#{==:#{zoom},1},ZOOM,-}", "ZOOM"}, // op inside a conditional
		{"#{m:*mux,gtmux}", "1"},               // glob match
		{"#{m:*mux,tmux2}", "0"},
		{"#{m/r:^g.*x$,gtmux}", "1"}, // regex match
		{"#{a:65}", "A"},             // char by code
		{"#{a:0x41}", ""},            // non-decimal -> empty
		// arithmetic
		{"#{e:1+2*3}", "7"},
		{"#{e:(1+2)*3}", "9"},
		{"#{e:10/4}", "2.5"},
		{"#{e|2:10/3}", "3.33"},
		{"#{e:#{zoom}+9}", "10"}, // expands vars inside the expression
		{"#{e:5%3}", "2"},
		{"#{e:1/0}", ""}, // divide by zero -> empty
	}
	for _, c := range cases {
		if got := Expand(c.in, vars); got != c.want {
			t.Errorf("Expand(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	// t: formats unix seconds; compare against the stdlib to stay TZ-independent.
	ts := int64(1_600_000_000)
	tvars := map[string]string{"ts": "1600000000"}
	if got, want := Expand("#{t:ts}", tvars), time.Unix(ts, 0).Format(time.ANSIC); got != want {
		t.Errorf("Expand t: = %q, want %q", got, want)
	}
	// t: with a custom strftime spec after ';' — colons in the spec must survive.
	if got, want := Expand("#{t:ts;%Y-%m-%d %H:%M}", tvars), time.Unix(ts, 0).Format("2006-01-02 15:04"); got != want {
		t.Errorf("Expand t:spec = %q, want %q", got, want)
	}
}

// #2: the choose-tree -f filter mechanism — workspacer emits
// `#{m:<prefix>-*,#{session_name}}`; a session passes when Expand is truthy.
func TestChooseTreeFilterExpr(t *testing.T) {
	expr := "#{m:work-*,#{session_name}}"
	truthy := func(n string) bool {
		r := Expand(expr, map[string]string{"session_name": n})
		return r != "" && r != "0"
	}
	if !truthy("work-api") {
		t.Errorf("work-api should match %q", expr)
	}
	if truthy("play-api") {
		t.Errorf("play-api should NOT match %q", expr)
	}
}
