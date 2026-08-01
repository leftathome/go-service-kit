package outbound

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
)

// ErrBlockedAddress is returned when a destination resolves to an address the
// egress guard refuses to connect to: loopback, RFC1918, CGNAT, link-local
// (including the cloud metadata address), unique-local, unspecified,
// broadcast, or multicast.
var ErrBlockedAddress = errors.New("outbound: destination address is not permitted")

type (
	// dialFunc matches net.Dialer.DialContext.
	dialFunc func(ctx context.Context, network, addr string) (net.Conn, error)

	// lookupFunc resolves a hostname to IP addresses. It exists as a seam so
	// the guard can be tested against a rebinding resolver without DNS.
	lookupFunc func(ctx context.Context, host string) ([]netip.Addr, error)
)

// blockedPrefixes covers ranges the stdlib netip predicates do not.
var blockedPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),       // "this network"
	netip.MustParsePrefix("100.64.0.0/10"),   // RFC6598 CGNAT
	netip.MustParsePrefix("192.0.0.0/24"),    // IETF protocol assignments
	netip.MustParsePrefix("192.0.2.0/24"),    // TEST-NET-1
	netip.MustParsePrefix("198.18.0.0/15"),   // benchmarking
	netip.MustParsePrefix("198.51.100.0/24"), // TEST-NET-2
	netip.MustParsePrefix("203.0.113.0/24"),  // TEST-NET-3
	netip.MustParsePrefix("240.0.0.0/4"),     // reserved, includes 255.255.255.255
	netip.MustParsePrefix("::/128"),          // unspecified
	netip.MustParsePrefix("2001:db8::/32"),   // documentation
	netip.MustParsePrefix("2002:a00::/24"),   // 6to4 wrapping 10/8
	netip.MustParsePrefix("2002:ac10::/28"),  // 6to4 wrapping 172.16/12
	netip.MustParsePrefix("2002:c0a8::/32"),  // 6to4 wrapping 192.168/16
	netip.MustParsePrefix("2002:7f00::/24"),  // 6to4 wrapping 127/8
	netip.MustParsePrefix("2002:a9fe::/32"),  // 6to4 wrapping 169.254/16
	netip.MustParsePrefix("64:ff9b::/96"),    // NAT64
	netip.MustParsePrefix("64:ff9b:1::/48"),  // local-use NAT64
	netip.MustParsePrefix("100::/64"),        // discard-only
}

// blockedIP reports whether ip is one this client must never connect to.
// The decision is made on a resolved address, never on a hostname, so a name
// that resolves public once and private later gains nothing.
func blockedIP(ip netip.Addr) bool {
	if !ip.IsValid() {
		return true
	}
	ip = ip.Unmap()
	switch {
	case ip.IsLoopback(),
		ip.IsPrivate(), // RFC1918 and IPv6 unique-local fc00::/7
		ip.IsLinkLocalUnicast(),
		ip.IsLinkLocalMulticast(),
		ip.IsInterfaceLocalMulticast(),
		ip.IsMulticast(),
		ip.IsUnspecified():
		return true
	}
	for _, p := range blockedPrefixes {
		if p.Contains(ip) {
			return true
		}
	}
	// NAT64 and 6to4 embed a v4 address; check the embedded address too.
	if v4 := embeddedV4(ip); v4.IsValid() && blockedIP(v4) {
		return true
	}
	return false
}

// embeddedV4 extracts the IPv4 address embedded in a NAT64 (64:ff9b::/96) or
// 6to4 (2002::/16) address, if any.
func embeddedV4(ip netip.Addr) netip.Addr {
	if !ip.Is6() {
		return netip.Addr{}
	}
	b := ip.As16()
	switch {
	case netip.MustParsePrefix("64:ff9b::/96").Contains(ip):
		return netip.AddrFrom4([4]byte{b[12], b[13], b[14], b[15]})
	case netip.MustParsePrefix("2002::/16").Contains(ip):
		return netip.AddrFrom4([4]byte{b[2], b[3], b[4], b[5]})
	}
	return netip.Addr{}
}

// defaultLookup resolves through the process resolver.
func defaultLookup(ctx context.Context, host string) ([]netip.Addr, error) {
	return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
}

// guardedDial wraps base with the SSRF guard. It resolves the host itself,
// refuses the connection if ANY answer is a blocked address, and then dials
// the checked IP literal -- there is no second resolution between the check
// and the connect for an attacker to poison.
//
// A record set that mixes public and private answers is itself the tell, so
// the whole dial is refused rather than racing for the "good" answer.
func guardedDial(base dialFunc, lookup lookupFunc) dialFunc {
	if lookup == nil {
		lookup = defaultLookup
	}
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, fmt.Errorf("outbound: bad address %q: %w", addr, err)
		}

		if ip, err := netip.ParseAddr(host); err == nil {
			if blockedIP(ip) {
				return nil, fmt.Errorf("%w: %s", ErrBlockedAddress, ip)
			}
			return base(ctx, network, addr)
		}

		addrs, err := lookup(ctx, host)
		if err != nil {
			return nil, fmt.Errorf("outbound: resolve %q: %w", host, err)
		}
		if len(addrs) == 0 {
			return nil, fmt.Errorf("outbound: resolve %q: no addresses", host)
		}
		for _, ip := range addrs {
			if blockedIP(ip) {
				return nil, fmt.Errorf("%w: %s resolves to %s", ErrBlockedAddress, host, ip)
			}
		}

		var errs []error
		for _, ip := range addrs {
			conn, err := base(ctx, network, net.JoinHostPort(ip.String(), port))
			if err == nil {
				return conn, nil
			}
			errs = append(errs, err)
		}
		return nil, errors.Join(errs...)
	}
}
