package client

import (
	"slices"
	"testing"
)

func TestWsNewOrder(t *testing.T) {
	for _, c := range []struct{ in, want []string }{
		{[]string{"ff", "new"}, []string{"new", "ff"}},
		{[]string{"ff", "n", "foo", "-gh"}, []string{"new", "ff", "foo", "-gh"}},
		{[]string{"new", "n"}, []string{"new", "n"}},
		{[]string{"home", "new"}, []string{"home", "new"}},
		{[]string{"ff", "stackr"}, []string{"ff", "stackr"}},
		{[]string{"ff"}, []string{"ff"}},
	} {
		if got := wsNewOrder(c.in); !slices.Equal(got, c.want) {
			t.Errorf("wsNewOrder(%v) = %v, want %v", c.in, got, c.want)
		}
	}
}
