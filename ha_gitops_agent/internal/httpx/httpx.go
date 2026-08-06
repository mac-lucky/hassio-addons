// Package httpx holds two scraps of net/http glue this add-on's HTTP
// callers share: Doer, the client seam their tests fake, and RemoteHost,
// the port stripping behind web's and hook's ingress-address check. A
// stdlib-only leaf; the old per-package names survive as type aliases.
package httpx

import (
	"net"
	"net/http"
)

// Doer is the subset of *http.Client this add-on needs: one round trip.
// Tests inject a fake instead of making a real HTTP call.
type Doer interface {
	Do(req *http.Request) (*http.Response, error)
}

// RemoteHost returns r.RemoteAddr's host, without the port net/http
// appends. An address that will not split comes back verbatim, not as
// "", so the callers' ingress-address comparison fails closed.
func RemoteHost(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
