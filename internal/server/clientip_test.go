package server

import (
	"net/http/httptest"
	"testing"
)

func TestClientIPUsesForwardedAddressOnlyFromTrustedProxy(t *testing.T) {
	cases := []struct {
		name   string
		remote string
		header string
		want   string
	}{
		{"direct public client", "203.0.113.9:5555", "198.51.100.7", "203.0.113.9"},
		{"loopback proxy", "127.0.0.1:5555", "198.51.100.7", "198.51.100.7"},
		{"private proxy", "192.168.1.20:5555", "198.51.100.7", "198.51.100.7"},
		{"tailscale proxy", "100.77.201.125:5555", "198.51.100.7", "198.51.100.7"},
		{"proxy without header", "127.0.0.1:5555", "", "127.0.0.1"},
		{"proxy with private forwarded address", "127.0.0.1:5555", "10.0.0.5", "127.0.0.1"},
		{"proxy with malformed forwarded address", "127.0.0.1:5555", "not-an-ip", "127.0.0.1"},
		{"ipv6 direct client", "[2001:db8::1]:5555", "", "2001:db8::1"},
		{"ipv6 proxy", "[::1]:5555", "198.51.100.7", "198.51.100.7"},
		{"remote without port", "127.0.0.1", "", "127.0.0.1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("POST", "/api/login", nil)
			r.RemoteAddr = tc.remote
			if tc.header != "" {
				r.Header.Set("CF-Connecting-IP", tc.header)
			}
			if got := clientIP(r); got != tc.want {
				t.Fatalf("clientIP = %q, want %q", got, tc.want)
			}
		})
	}
}
