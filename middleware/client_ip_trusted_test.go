package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
)

// TestClientIPFromTrustedProxies_Acceptance pins the two acceptance cases:
// a trusted direct peer yields the rightmost-untrusted XFF entry, and an
// untrusted direct peer cannot use XFF at all.
func TestClientIPFromTrustedProxies_Acceptance(t *testing.T) {
	const cidr = "10.0.0.0/8"

	t.Run("trusted_peer_uses_xff_rightmost_untrusted", func(t *testing.T) {
		got := runTrusted(t, []string{cidr}, func(r *http.Request) {
			r.RemoteAddr = "10.0.0.2:54321"
			r.Header.Set("X-Forwarded-For", "203.0.113.7, 10.0.0.1")
		})
		if got != "203.0.113.7" {
			t.Errorf("want 203.0.113.7, got %q", got)
		}
	})

	t.Run("untrusted_peer_ignores_xff", func(t *testing.T) {
		got := runTrusted(t, []string{cidr}, func(r *http.Request) {
			r.RemoteAddr = "192.0.2.5:54321"
			r.Header.Set("X-Forwarded-For", "203.0.113.7, 10.0.0.1")
		})
		if got != "192.0.2.5" {
			t.Errorf("want 192.0.2.5 (XFF ignored), got %q", got)
		}
	})
}

func TestClientIPFromTrustedProxies(t *testing.T) {
	tt := []struct {
		name  string
		cidrs []string
		xff   []string
		peer  string
		out   string
	}{
		// Trusted peer: XFF is honored, trusted hops skipped right-to-left.
		{"trusted_peer_single_hop_skipped", []string{"10.0.0.0/8"}, []string{"203.0.113.7, 10.0.0.1"}, "10.0.0.2:1234", "203.0.113.7"},
		{"trusted_peer_multiple_hops_skipped", []string{"10.0.0.0/8", "198.51.100.0/24"}, []string{"203.0.113.7, 198.51.100.5, 10.0.0.1"}, "10.0.0.2:1234", "203.0.113.7"},
		{"trusted_peer_all_trusted_falls_back_to_peer", []string{"10.0.0.0/8"}, []string{"10.0.0.1, 10.0.0.2"}, "10.0.0.9:1234", "10.0.0.9"},
		{"trusted_peer_missing_xff_falls_back_to_peer", []string{"10.0.0.0/8"}, nil, "10.0.0.2:1234", "10.0.0.2"},
		{"trusted_peer_empty_xff_falls_back_to_peer", []string{"10.0.0.0/8"}, []string{""}, "10.0.0.2:1234", "10.0.0.2"},
		{"trusted_peer_garbage_xff_falls_back_to_peer", []string{"10.0.0.0/8"}, []string{"203.0.113.7, garbage, 10.0.0.1"}, "10.0.0.2:1234", "10.0.0.2"},
		{"trusted_peer_garbage_rightmost_falls_back_to_peer", []string{"10.0.0.0/8"}, []string{"garbage"}, "10.0.0.2:1234", "10.0.0.2"},
		{"trusted_peer_xff_with_port_falls_back_to_peer", []string{"10.0.0.0/8"}, []string{"203.0.113.7:8080, 10.0.0.1"}, "10.0.0.2:1234", "10.0.0.2"},
		{"trusted_peer_ipv6_xff", []string{"2001:db8::/32"}, []string{"203.0.113.7, 2001:db8::1"}, "[2001:db8::2]:1234", "203.0.113.7"},
		{"trusted_peer_v4mapped_xff_folded", []string{"10.0.0.0/8"}, []string{"::ffff:203.0.113.7, 10.0.0.1"}, "10.0.0.2:1234", "203.0.113.7"},
		{"trusted_peer_loopback_cidr", []string{"127.0.0.0/8"}, []string{"203.0.113.7, 127.0.0.1"}, "127.0.0.1:1234", "203.0.113.7"},

		// Untrusted peer: XFF is ignored no matter what it contains.
		{"untrusted_peer_xff_ignored", []string{"10.0.0.0/8"}, []string{"203.0.113.7, 10.0.0.1"}, "192.0.2.5:1234", "192.0.2.5"},
		{"untrusted_peer_garbage_xff_ignored", []string{"10.0.0.0/8"}, []string{"garbage"}, "192.0.2.5:1234", "192.0.2.5"},
		{"untrusted_peer_spoofed_admin_ignored", []string{"10.0.0.0/8"}, []string{"10.0.0.1"}, "192.0.2.5:1234", "192.0.2.5"},

		// Empty trust list: nobody is trusted, always fall back to the peer.
		{"empty_trust_list_always_peer", nil, []string{"203.0.113.7, 10.0.0.1"}, "10.0.0.2:1234", "10.0.0.2"},
	}
	for _, tc := range tt {
		t.Run(tc.name, func(t *testing.T) {
			got := runTrusted(t, tc.cidrs, func(r *http.Request) {
				r.RemoteAddr = tc.peer
				for _, v := range tc.xff {
					r.Header.Add("X-Forwarded-For", v)
				}
			})
			if got != tc.out {
				t.Errorf("want %q, got %q", tc.out, got)
			}
		})
	}
}

// TestClientIPFromTrustedProxies_DuplicateHeaders pins the deterministic
// rule for duplicate X-Forwarded-For headers: all instances are merged in
// wire order, so an attacker cannot pick which value security logic sees.
func TestClientIPFromTrustedProxies_DuplicateHeaders(t *testing.T) {
	t.Run("merged_in_wire_order", func(t *testing.T) {
		got := runTrusted(t, []string{"198.51.100.0/24"}, func(r *http.Request) {
			r.RemoteAddr = "198.51.100.1:1234"
			r.Header.Add("X-Forwarded-For", "127.0.0.1")
			r.Header.Add("X-Forwarded-For", "8.8.8.8, 198.51.100.2")
		})
		if got != "8.8.8.8" {
			t.Errorf("want 8.8.8.8 (merged rightmost-untrusted), got %q", got)
		}
	})

	t.Run("attacker_prepend_in_first_header_ignored", func(t *testing.T) {
		got := runTrusted(t, []string{"10.0.0.0/8"}, func(r *http.Request) {
			r.RemoteAddr = "10.0.0.2:1234"
			r.Header.Add("X-Forwarded-For", "6.6.6.6, 203.0.113.7")
			r.Header.Add("X-Forwarded-For", "10.0.0.1")
		})
		if got != "203.0.113.7" {
			t.Errorf("want 203.0.113.7, got %q", got)
		}
	})
}

// TestClientIPFromTrustedProxies_IPv6RemoteAddr pins the deterministic rule
// for IPv6 peers: net.SplitHostPort strips the port, v4-mapped folds to v4,
// and a zone is stripped for the trust check but preserved in storage.
func TestClientIPFromTrustedProxies_IPv6RemoteAddr(t *testing.T) {
	t.Run("ipv6_with_port_trusted", func(t *testing.T) {
		got := runTrusted(t, []string{"2001:db8::/32"}, func(r *http.Request) {
			r.RemoteAddr = "[2001:db8::2]:443"
			r.Header.Set("X-Forwarded-For", "203.0.113.7")
		})
		if got != "203.0.113.7" {
			t.Errorf("want 203.0.113.7, got %q", got)
		}
	})

	t.Run("ipv6_with_port_no_xff_falls_back", func(t *testing.T) {
		got := runTrusted(t, []string{"2001:db8::/32"}, func(r *http.Request) {
			r.RemoteAddr = "[2001:db8::2]:443"
		})
		if got != "2001:db8::2" {
			t.Errorf("want 2001:db8::2, got %q", got)
		}
	})

	t.Run("ipv6_untrusted_peer_ignores_xff", func(t *testing.T) {
		got := runTrusted(t, []string{"2001:db8::/32"}, func(r *http.Request) {
			r.RemoteAddr = "[2001:db9::2]:443"
			r.Header.Set("X-Forwarded-For", "203.0.113.7")
		})
		if got != "2001:db9::2" {
			t.Errorf("want 2001:db9::2 (XFF ignored), got %q", got)
		}
	})

	t.Run("v4mapped_peer_folds_to_v4", func(t *testing.T) {
		got := runTrusted(t, []string{"10.0.0.0/8"}, func(r *http.Request) {
			r.RemoteAddr = "[::ffff:10.0.0.2]:443"
		})
		if got != "10.0.0.2" {
			t.Errorf("want 10.0.0.2 (v4-mapped folded), got %q", got)
		}
	})

	t.Run("zoned_peer_matches_trusted_prefix", func(t *testing.T) {
		got := runTrusted(t, []string{"fe80::/10"}, func(r *http.Request) {
			r.RemoteAddr = "[fe80::1%eth0]:443"
			r.Header.Set("X-Forwarded-For", "203.0.113.7")
		})
		if got != "203.0.113.7" {
			t.Errorf("want 203.0.113.7 (zone stripped for trust check), got %q", got)
		}
	})
}

// TestClientIPFromTrustedProxies_RemoteAddrNotMutated pins the core contract:
// unlike RealIP, this middleware never mutates r.RemoteAddr.
func TestClientIPFromTrustedProxies_RemoteAddrNotMutated(t *testing.T) {
	const original = "10.0.0.2:54321"
	var capturedRemoteAddr, capturedClientIP string
	r := chi.NewRouter()
	r.Use(ClientIPFromTrustedProxies("10.0.0.0/8"))
	r.Get("/", func(w http.ResponseWriter, r *http.Request) {
		capturedRemoteAddr = r.RemoteAddr
		capturedClientIP = GetClientIP(r.Context())
	})

	req := httptest.NewRequest("GET", "/", nil)
	req.RemoteAddr = original
	req.Header.Set("X-Forwarded-For", "203.0.113.7, 10.0.0.1")
	r.ServeHTTP(httptest.NewRecorder(), req)

	if capturedRemoteAddr != original {
		t.Errorf("r.RemoteAddr was mutated: want %q, got %q", original, capturedRemoteAddr)
	}
	if capturedClientIP != "203.0.113.7" {
		t.Errorf("want 203.0.113.7, got %q", capturedClientIP)
	}
}

// TestClientIPFromTrustedProxies_BadCIDRRejectedAtRegistration pins the
// registration-time rejection: a bad CIDR panics before any request is
// served.
func TestClientIPFromTrustedProxies_BadCIDRRejectedAtRegistration(t *testing.T) {
	for _, bad := range []string{"not-a-cidr", "10.0.0.0", "203.0.113.7", "999.0.0.0/8", "10.0.0.0/99"} {
		t.Run(bad, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatalf("expected panic on invalid CIDR %q", bad)
				}
			}()
			ClientIPFromTrustedProxies(bad)
		})
	}
}

// TestClientIPFromTrustedProxies_UnparseableRemoteAddr pins fail-closed: if
// the direct connection peer cannot be parsed, no client IP is set.
func TestClientIPFromTrustedProxies_UnparseableRemoteAddr(t *testing.T) {
	got := runTrusted(t, []string{"10.0.0.0/8"}, func(r *http.Request) {
		r.RemoteAddr = "not-an-ip"
		r.Header.Set("X-Forwarded-For", "203.0.113.7, 10.0.0.1")
	})
	if got != "" {
		t.Errorf("want empty (fail-closed), got %q", got)
	}
}

// TestClientIPFromTrustedProxies_WithRealIP covers the combination with the
// legacy RealIP. RealIP is deprecated (it mutates r.RemoteAddr
// and trusts the leftmost XFF value); these tests pin the combined behavior
// so the interaction is deterministic.
func TestClientIPFromTrustedProxies_WithRealIP(t *testing.T) {
	t.Run("new_middleware_first_context_value_survives", func(t *testing.T) {
		var clientIP, remoteAddr string
		r := chi.NewRouter()
		r.Use(ClientIPFromTrustedProxies("10.0.0.0/8"))
		r.Use(RealIP)
		r.Get("/", func(w http.ResponseWriter, r *http.Request) {
			clientIP = GetClientIP(r.Context())
			remoteAddr = r.RemoteAddr
		})

		req := httptest.NewRequest("GET", "/", nil)
		req.RemoteAddr = "10.0.0.2:1234"
		req.Header.Set("X-Forwarded-For", "203.0.113.7, 10.0.0.1")
		r.ServeHTTP(httptest.NewRecorder(), req)

		// The new middleware stored the rightmost-untrusted value in context;
		// RealIP then mutated r.RemoteAddr to the leftmost value. The context
		// value is unaffected.
		if clientIP != "203.0.113.7" {
			t.Errorf("context client IP: want 203.0.113.7, got %q", clientIP)
		}
		if remoteAddr != "203.0.113.7" {
			t.Errorf("r.RemoteAddr: want 203.0.113.7 (RealIP-mutated), got %q", remoteAddr)
		}
	})

	t.Run("realip_first_gate_reads_mutated_peer", func(t *testing.T) {
		// RealIP runs first and mutates r.RemoteAddr to the attacker-
		// controlled leftmost XFF value. The new middleware then gates on
		// that mutated peer, so a spoofed leftmost value becomes the answer.
		// This is precisely why RealIP must not be combined with (or used
		// instead of) the trusted-proxy
		got := runChain(t,
			[]func(http.Handler) http.Handler{
				RealIP,
				ClientIPFromTrustedProxies("10.0.0.0/8"),
			},
			func(r *http.Request) {
				r.RemoteAddr = "10.0.0.2:1234"
				r.Header.Set("X-Forwarded-For", "6.6.6.6, 203.0.113.7, 10.0.0.1")
			})
		if got != "6.6.6.6" {
			t.Errorf("want 6.6.6.6 (RealIP-mutated peer gates the trust check), got %q", got)
		}
	})
}

// runTrusted invokes ClientIPFromTrustedProxies with the given CIDRs and the
// request constructed by buildReq, returning the client IP captured in the
// handler (or "" if none was set).
func runTrusted(t *testing.T, cidrs []string, buildReq func(*http.Request)) string {
	t.Helper()
	return run(t, ClientIPFromTrustedProxies(cidrs...), buildReq)
}
