package bark

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRejectUnsafeCredentialURLs(t *testing.T) {
	for _, server := range []string{"http://api.day.app", "https://user:pass@example.com", "https://example.com?token=x", "https://example.com#fragment", "bogus"} {
		if Validate(server, "test-key") == nil {
			t.Fatalf("accepted %s", server)
		}
	}
}

func TestSendRejectsInvalidConfigurationBeforeConnecting(t *testing.T) {
	if err := Send(context.Background(), "", "", "test", "test"); err == nil {
		t.Fatal("missing key accepted")
	}
}

func TestRejectRedirectWithoutForwardingSecret(t *testing.T) {
	// Use TLS so the exact production transport is exercised, with only the
	// test certificate trusted for this local server.
	hits := 0
	sink := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits++; w.Write([]byte(`{"code":200}`)) }))
	defer sink.Close()
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, sink.URL, http.StatusTemporaryRedirect)
	}))
	defer origin.Close()
	old := http.DefaultTransport
	http.DefaultTransport = origin.Client().Transport
	defer func() { http.DefaultTransport = old }()
	if err := Send(context.Background(), origin.URL, "test-key", "test", "test"); err == nil {
		t.Fatal("redirect accepted")
	}
	if hits != 0 {
		t.Fatal("key forwarded through redirect")
	}
}

func TestWakeOptionsAreSent(t *testing.T) {
	var payload map[string]any
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Error(err)
		}
		w.Write([]byte(`{"code":200}`))
	}))
	defer srv.Close()
	old := http.DefaultTransport
	http.DefaultTransport = srv.Client().Transport
	defer func() { http.DefaultTransport = old }()
	if err := SendWithOptions(context.Background(), srv.URL, "key", "title", "body", Wake()); err != nil {
		t.Fatal(err)
	}
	if payload["level"] != "critical" || payload["volume"] != float64(10) || payload["call"] != float64(1) {
		t.Fatalf("wake payload=%v", payload)
	}
	if payload["device_key"] != "key" || payload["title"] != "title" || payload["body"] != "body" || payload["group"] != "DueRadar" {
		t.Fatalf("base payload=%v", payload)
	}
	// Ordinary reminders must stay exactly as quiet as before.
	payload = nil
	if err := Send(context.Background(), srv.URL, "key", "title", "body"); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"level", "volume", "call", "sound"} {
		if _, present := payload[key]; present {
			t.Fatalf("ordinary send included %q: %v", key, payload)
		}
	}
}

func TestApplicationErrorIsNotDeliverySuccess(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{"code":400}`)) }))
	defer srv.Close()
	old := http.DefaultTransport
	http.DefaultTransport = srv.Client().Transport
	defer func() { http.DefaultTransport = old }()
	if err := Send(context.Background(), srv.URL, "key", "test", "test"); err == nil {
		t.Fatal("API rejection marked delivered")
	}
}
