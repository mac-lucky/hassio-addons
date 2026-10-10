package httpx

import (
	"net/http"
	"testing"
)

// TestRemoteHostStripsThePort: net/http appends a port to RemoteAddr,
// and comparing that against the bare proxy address refuses everything.
func TestRemoteHostStripsThePort(t *testing.T) {
	cases := []struct {
		name string
		addr string
		want string
	}{
		{"ipv4", "172.30.32.2:41234", "172.30.32.2"},
		{"ipv6", "[::1]:8099", "::1"},
		{"loopback", "127.0.0.1:1", "127.0.0.1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := &http.Request{RemoteAddr: tc.addr}
			if got := RemoteHost(r); got != tc.want {
				t.Errorf("RemoteHost(%q) = %q, want %q", tc.addr, got, tc.want)
			}
		})
	}
}

// TestRemoteHostReturnsAnUnsplittableAddressVerbatim: a hand-built
// request can carry no port, and returning "" would make every such
// address look alike to the ingress comparison.
func TestRemoteHostReturnsAnUnsplittableAddressVerbatim(t *testing.T) {
	for _, addr := range []string{"172.30.32.2", "", "@"} {
		r := &http.Request{RemoteAddr: addr}
		if got := RemoteHost(r); got != addr {
			t.Errorf("RemoteHost(%q) = %q, want it returned unchanged", addr, got)
		}
	}
}
