// Package netguard protects outbound webhook and provider calls against SSRF: dialing
// loopback, private, link-local or metadata addresses is refused unless the host or
// CIDR is allow-listed [NFR-14].
package netguard

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"time"
)

// Blocked reports whether ip is in a range outbound calls must not reach by default.
func Blocked(ip netip.Addr) bool {
	ip = ip.Unmap()
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsUnspecified() || ip.IsMulticast() || cgnat.Contains(ip) || ip == metadataV4
}

var (
	cgnat      = netip.MustParsePrefix("100.64.0.0/10")
	metadataV4 = netip.MustParseAddr("169.254.169.254")
)

// Allow is an allow-list of hosts and CIDRs (for example "localhost", "10.0.0.0/8").
type Allow struct {
	hosts    map[string]bool
	prefixes []netip.Prefix
}

func ParseAllow(entries []string) Allow {
	a := Allow{hosts: map[string]bool{}}
	for _, e := range entries {
		if p, err := netip.ParsePrefix(e); err == nil {
			a.prefixes = append(a.prefixes, p)
		} else if ip, err := netip.ParseAddr(e); err == nil {
			a.prefixes = append(a.prefixes, netip.PrefixFrom(ip, ip.BitLen()))
		} else {
			a.hosts[e] = true
		}
	}
	return a
}

func (a Allow) permits(host string, ip netip.Addr) bool {
	if a.hosts["*"] || a.hosts[host] {
		return true
	}
	for _, p := range a.prefixes {
		if p.Contains(ip.Unmap()) {
			return true
		}
	}
	return false
}

// DialContext resolves the host itself and refuses blocked addresses, so DNS rebinding
// cannot sneak a private address past a hostname check.
func DialContext(allow Allow) func(ctx context.Context, network, addr string) (net.Conn, error) {
	d := &net.Dialer{Timeout: 10 * time.Second}
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
		if err != nil {
			return nil, err
		}
		for _, ip := range ips {
			if Blocked(ip) && !allow.permits(host, ip) {
				return nil, fmt.Errorf("netguard: refusing to dial %s (%s): private or internal address not allow-listed", host, ip)
			}
		}
		return d.DialContext(ctx, network, net.JoinHostPort(ips[0].String(), port))
	}
}

// Transport returns an HTTP transport using the guarded dialer.
func Transport(allow []string) *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.Proxy = nil // a proxy would dial on our behalf and bypass the check
	t.DialContext = DialContext(ParseAllow(allow))
	return t
}
