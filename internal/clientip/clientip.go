// Package clientip turns a client address into the identity that per-client
// limits count by (#188).
//
// An IPv4 address identifies one client, or one NAT. An IPv6 subscriber, in
// contrast, usually gets a whole /64, often a /56 or a /48: a client can use
// a fresh address for every request, and would get a fresh budget each time
// if limits counted full IPv6 addresses. Limits therefore count an IPv6
// address by its prefix.
package clientip

import (
	"net/netip"
)

// DefaultIPv6PrefixLength is the prefix that IPv6 clients are grouped by
// when the configuration does not set one: the /64 that one subscriber, or
// one LAN, gets at the least.
const DefaultIPv6PrefixLength = 64

// Key returns the identity of addr for rate limits and connection counts:
//   - an IPv4 address, or an IPv4-mapped IPv6 address, as the IPv4 address;
//   - an IPv6 address as its network of ipv6Prefix bits, such as
//     "2001:db8:1:2::/64";
//   - anything that is not an address, unchanged, so that a caller never
//     loses its key.
//
// An ipv6Prefix outside 1 to 128 is DefaultIPv6PrefixLength. The zone of a
// link-local address is dropped: it names an interface of this host, not
// the client.
func Key(addr string, ipv6Prefix int) string {
	ip, err := netip.ParseAddr(addr)
	if err != nil {
		return addr
	}
	ip = ip.WithZone("").Unmap()
	if ip.Is4() {
		return ip.String()
	}
	if ipv6Prefix < 1 || ipv6Prefix > 128 {
		ipv6Prefix = DefaultIPv6PrefixLength
	}
	prefix, err := ip.Prefix(ipv6Prefix)
	if err != nil {
		return ip.String()
	}
	return prefix.String()
}
