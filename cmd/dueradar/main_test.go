package main

import "testing"

func TestLocalHealthURLFollowsConfiguredListenAddress(t *testing.T) {
	for _, tc := range []struct {
		listen string
		want   string
	}{
		{"127.0.0.1:8080", "http://127.0.0.1:8080/healthz"},
		{"0.0.0.0:9090", "http://127.0.0.1:9090/healthz"},
		{":9000", "http://127.0.0.1:9000/healthz"},
		{"[::]:8081", "http://127.0.0.1:8081/healthz"},
		{"[::1]:8081", "http://[::1]:8081/healthz"},
		{" 127.0.0.1:8080 ", "http://127.0.0.1:8080/healthz"},
		{"not-an-address", ""},
	} {
		if got := localHealthURL(tc.listen); got != tc.want {
			t.Errorf("localHealthURL(%q) = %q, want %q", tc.listen, got, tc.want)
		}
	}
}
