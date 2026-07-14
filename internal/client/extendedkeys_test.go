package client

import "testing"

func TestKittyNegotiate(t *testing.T) {
	cases := []struct {
		name      string
		old, want int
		enabled   bool
		out       string
		state     int
	}{
		{"enable from legacy", 0, 1, true, "\x1b[>1u", 1},
		{"disable to legacy", 1, 0, true, "\x1b[<1u", 0},
		{"change flags pops then pushes", 1, 5, true, "\x1b[<1u\x1b[>5u", 5},
		{"no change", 1, 1, true, "", 1},
		{"option off ignores want", 0, 1, false, "", 0},
		{"option off pops existing", 1, 1, false, "\x1b[<1u", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, state := kittyNegotiate(tc.old, tc.want, tc.enabled)
			if string(out) != tc.out {
				t.Fatalf("out = %q, want %q", out, tc.out)
			}
			if state != tc.state {
				t.Fatalf("state = %d, want %d", state, tc.state)
			}
		})
	}
}
