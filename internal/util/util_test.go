package util

import (
	"testing"
)

func TestLaterTSComparesInstants(t *testing.T) {
	// Lexically "2026-01-01T09:00:00+08:00" sorts before "...T02:00:00Z", but as
	// instants the first is 01:00 UTC and the second is 02:00 UTC.
	a := "2026-01-01T09:00:00+08:00"
	b := "2026-01-01T02:00:00Z"
	if got := LaterTS(a, b); got != b {
		t.Fatalf("laterTS(%q, %q) = %q, want %q", a, b, got, b)
	}
	if got := LaterTS("", b); got != b {
		t.Fatalf("laterTS(\"\", b) = %q", got)
	}
	if got := LaterTS(a, ""); got != a {
		t.Fatalf("laterTS(a, \"\") = %q", got)
	}
}
