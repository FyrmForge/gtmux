package server

import (
	"strings"
	"testing"
)

func TestAutoName(t *testing.T) {
	tests := []struct {
		name    string
		nameFmt string
		taken   []string
		want    string
	}{
		{"empty numeric", "%d", nil, "0"},
		{"lowest unused", "%d", []string{"0", "1"}, "2"},
		{"fills gap", "%d", []string{"0", "2"}, "1"},
		{"prefix format", "s%d", []string{"s0"}, "s1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &registry{sessions: map[string]*session{}, nameFmt: tt.nameFmt}
			for _, n := range tt.taken {
				r.sessions[n] = nil
			}
			if got := r.autoName(); got != tt.want {
				t.Errorf("autoName() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestResolve covers resolve's branch/error logic. The create-success paths
// (auto-name, named create) spawn a real session goroutine, so they're left to
// the e2e suite; here we exercise only the branches that don't create.
func TestResolve(t *testing.T) {
	only := &session{} // sentinel to check identity, never run

	tests := []struct {
		name    string
		taken   map[string]*session
		reqName string
		create  bool
		want    *session // nil = expect an error
		errHas  string
	}{
		{"duplicate create", map[string]*session{"x": only}, "x", true, nil, "duplicate session: x"},
		{"attach existing", map[string]*session{"x": only}, "x", false, only, ""},
		{"attach missing", map[string]*session{"x": only}, "y", false, nil, "no such session: y"},
		{"bare attach none", map[string]*session{}, "", false, nil, "no sessions"},
		{"bare attach one", map[string]*session{"only": only}, "", false, only, ""},
		{"bare attach many", map[string]*session{"a": only, "b": only}, "", false, nil, "specify one: a, b"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &registry{sessions: tt.taken, nameFmt: "%d"}
			got, err := r.resolve(tt.reqName, tt.create, 80, 24, "")
			if tt.want == nil {
				if err == nil {
					t.Fatalf("resolve() = %v, want error containing %q", got, tt.errHas)
				}
				if !strings.Contains(err.Error(), tt.errHas) {
					t.Errorf("error = %q, want to contain %q", err, tt.errHas)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolve() error = %v, want session", err)
			}
			if got != tt.want {
				t.Errorf("resolve() = %p, want %p", got, tt.want)
			}
		})
	}
}
