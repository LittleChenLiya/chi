package middleware

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/netip"
)

// ClientIPFromTrustedProxies stores the client IP read from the
// X-Forwarded-For header, but ONLY when the peer that opened the TCP
// connection to this server (r.RemoteAddr) is itself one of the given
// trusted CIDRs. Read it with [GetClientIP].
//
// This is the deployment shape Express ("trust proxy"), Gin
// ("TrustedProxies") and Echo ("TrustedProxies") implement: the direct
// connection is the one thing a client cannot spoof, so it gates
// everything else.
//
//   - If r.RemoteAddr is NOT in a trusted CIDR, the forwarding headers are
//     ignored completely and the peer itself is the client IP. An attacker
//     who can reach the server directly cannot forge X-Forwarded-For.
//
//   - If r.RemoteAddr IS trusted, X-Forwarded-For is walked right-to-left
//     (every header instance merged in wire order, comma-split, trimmed),
//     skipping entries that fall in a trusted CIDR. The first entry that is
//     not trusted is the client.
//
//   - If no untrusted entry is found (header missing, empty, or every entry
//     trusted), or any entry fails to parse (garbage, an IP:port entry), the
//     client IP falls back to the direct connection peer. A bad header never
//     panics and never falls back to a value the client chose.
//
// r.RemoteAddr is parsed with net.SplitHostPort, so IPv6 peers in brackets
// ("[2001:db8::1]:443") and v4-mapped IPv6 ("[::ffff:1.2.3.4]:443") are
// handled; v4-mapped addresses fold to plain IPv4. The IPv6 zone
// ("%eth0") is stripped for the trust-prefix check but preserved in the
// stored fallback, matching [ClientIPFromRemoteAddr].
//
// No DNS lookups are performed; trust is decided purely by IP prefix
// containment.
//
// Compare [ClientIPFromXFF], which walks X-Forwarded-For without checking
// the direct peer — use it only when something else (a security group or a
// firewall) guarantees only trusted proxies can connect.
//
// Panics at registration if any CIDR is invalid.
func ClientIPFromTrustedProxies(trustedCIDRs ...string) func(http.Handler) http.Handler {
	prefixes := make([]netip.Prefix, len(trustedCIDRs))
	for i, cidr := range trustedCIDRs {
		prefix, err := netip.ParsePrefix(cidr)
		if err != nil {
			panic(fmt.Sprintf("middleware.ClientIPFromTrustedProxies: invalid trusted CIDR %q: %v", cidr, err))
		}
		prefixes[i] = prefix
	}
	return func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if ip, ok := clientIPFromTrustedProxies(r, prefixes); ok {
				r = r.WithContext(context.WithValue(r.Context(), clientIPCtxKey, ip))
			}
			h.ServeHTTP(w, r)
		})
	}
}

// clientIPFromTrustedProxies implements [ClientIPFromTrustedProxies]. It
// returns the zero Addr and false when no client IP can be determined
// (r.RemoteAddr itself is unparseable).
func clientIPFromTrustedProxies(r *http.Request, prefixes []netip.Prefix) (netip.Addr, bool) {
	peer, ok := remoteAddrPeerIP(r)
	if !ok {
		return netip.Addr{}, false
	}

	// The direct connection is the only unspoofable signal. If it is not
	// a trusted proxy, ignore every forwarding header and answer with the
	// peer itself.
	if !inAnyPrefix(peer.WithZone(""), prefixes) {
		return peer, true
	}

	var found netip.Addr
	walkXFF(r.Header[xForwardedForHeader], func(v string) bool {
		ip, ok := parseHeaderAddr(v)
		if !ok {
			// Malformed header: stop the walk and fall back to the peer.
			return true
		}
		if inAnyPrefix(ip, prefixes) {
			return false // trusted hop; keep walking left.
		}
		found = ip
		return true
	})
	if found.IsValid() {
		return found, true
	}
	return peer, true
}

// remoteAddrPeerIP parses r.RemoteAddr into a netip.Addr, mirroring
// [ClientIPFromRemoteAddr]: host:port is split (IPv6 in brackets),
// v4-mapped IPv6 folds to plain v4, and an IPv6 zone is preserved.
func remoteAddrPeerIP(r *http.Request) (netip.Addr, bool) {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr // RemoteAddr may already be a bare IP (e.g. in tests).
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}, false
	}
	return ip.Unmap(), true
}
