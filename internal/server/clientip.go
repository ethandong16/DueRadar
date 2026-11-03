package server

import (
	"net"
	"net/http"
	"net/netip"
)

// clientIP returns the address used for per-client limits such as login and
// registration throttling. When a request arrives from a local reverse proxy
// (loopback, private network, or the Tailscale CGNAT range), Cloudflare's
// CF-Connecting-IP is used instead of the proxy address so unrelated users do
// not share a single budget. The listener is not reachable from the public
// internet in that setup, so the header cannot be forged by clients that are
// not already on the private network.
func clientIP(r *http.Request) string {
	remote, ok := addrFromHostPort(r.RemoteAddr)
	if !ok {
		return r.RemoteAddr
	}
	if trustedProxy(remote) {
		if ip, err := netip.ParseAddr(r.Header.Get("CF-Connecting-IP")); err == nil {
			ip = ip.Unmap()
			if isPublicAddr(ip) {
				return ip.String()
			}
		}
	}
	return remote.String()
}

func addrFromHostPort(hostPort string) (netip.Addr, bool) {
	host, _, err := net.SplitHostPort(hostPort)
	if err != nil {
		host = hostPort
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}, false
	}
	return ip.Unmap(), true
}

func trustedProxy(ip netip.Addr) bool {
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsUnspecified() ||
		(ip.Is4() && netip.MustParsePrefix("100.64.0.0/10").Contains(ip)) ||
		(ip.Is6() && netip.MustParsePrefix("fc00::/7").Contains(ip))
}

func isPublicAddr(ip netip.Addr) bool {
	return ip.IsValid() && ip.IsGlobalUnicast() && !ip.IsPrivate() && !ip.IsLoopback() &&
		!ip.IsLinkLocalUnicast() && !ip.IsLinkLocalMulticast() && !ip.IsInterfaceLocalMulticast() &&
		!ip.IsMulticast() && !ip.IsUnspecified()
}
