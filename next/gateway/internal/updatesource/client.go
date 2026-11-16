package updatesource

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"time"
)

// Client is the HTTP client of the update sources. It never uses an
// environment proxy (neither its credentials nor its reach belong to the
// source) and resolves every connection's host itself, dialing only the
// addresses it resolved: unless allowPrivate, all of them must be public, so
// that no DNS answer, rebinding included, can point it into a private
// network. It has no total timeout: callers bound metadata requests by
// deadline and bundle downloads by progress.
func Client(allowPrivate bool) *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Proxy = nil
	tr.DialContext = pinnedDial(allowPrivate)
	return &http.Client{Transport: tr}
}

func pinnedDial(allowPrivate bool) func(ctx context.Context, network, address string) (net.Conn, error) {
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, err
		}
		ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
		if err != nil {
			return nil, err
		}
		for _, ip := range ips {
			if !allowPrivate && !publicIP(ip) {
				return nil, errors.New("update source host resolved to a non-public address")
			}
		}
		last := errors.New("update source host has no addresses")
		for _, ip := range ips {
			c, err := (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
			if err == nil {
				return c, nil
			}
			last = err
		}
		return nil, last
	}
}

// nonPublic are the special-purpose ranges a global unicast check misses.
var nonPublic = func() []netip.Prefix {
	var out []netip.Prefix
	for _, s := range []string{"0.0.0.0/8", "100.64.0.0/10", "192.0.0.0/24", "192.0.2.0/24", "198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24", "240.0.0.0/4", "2001:db8::/32"} {
		out = append(out, netip.MustParsePrefix(s))
	}
	return out
}()

func publicIP(ip netip.Addr) bool {
	ip = ip.Unmap()
	if !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
		return false
	}
	for _, p := range nonPublic {
		if p.Contains(ip) {
			return false
		}
	}
	return true
}
