package platform

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestQRLoginEstablishesIndependentSession(t *testing.T) {
	confirmed := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v3/user/login/app-web-login":
			var input map[string]string
			json.NewDecoder(r.Body).Decode(&input)
			if input["token"] != "fixture-token" {
				t.Error("wrong login token")
			}
			http.SetCookie(w, &http.Cookie{Name: "csrftoken", Value: "fixture-csrf", Path: "/"})
			w.Write([]byte(`{"code":0,"data":{"UserID":123,"Auth":"fixture-auth"}}`))
		case "/pc/web_login":
			if r.Header.Get("X-CSRFToken") != "fixture-csrf" {
				t.Error("missing csrf")
			}
			var input map[string]any
			json.NewDecoder(r.Body).Decode(&input)
			if input["UserID"] != float64(123) || input["Auth"] != "fixture-auth" {
				t.Error("wrong confirmation payload")
			}
			confirmed = true
			http.SetCookie(w, &http.Cookie{Name: "sessionid", Value: "fixture-session", Path: "/", HttpOnly: true})
			w.Write([]byte(`{"success":true}`))
		case "/api/v3/user/basic-info":
			if cookie, err := r.Cookie("sessionid"); err != nil || cookie.Value != "fixture-session" {
				t.Error("independent validator did not receive QR session")
			}
			w.Write([]byte(`{"code":0,"data":{"id":123}}`))
		default:
			t.Error("unexpected endpoint")
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	flow := &RainLogin{base: rainOrigin, client: &http.Client{Transport: rainLocalTransport(srv.URL)}, token: "fixture-token"}
	cookie, err := flow.Wait(context.Background())
	if err != nil || !confirmed || !json.Valid([]byte(cookie)) {
		t.Fatal("login confirmation failed", err)
	}
	adapter := NewRain(cookie)
	adapter.client.Transport = rainLocalTransport(srv.URL)
	if err := adapter.Validate(context.Background()); err != nil {
		t.Fatal("exported QR session failed independent validation", err)
	}
}

func TestQRLoginRejectsMissingSessionAndRefusedLogin(t *testing.T) {
	for _, body := range []string{`{"code":1,"data":{}}`, `{"code":0,"data":{}}`} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(body)) }))
		flow := &RainLogin{base: rainOrigin, client: &http.Client{Transport: rainLocalTransport(srv.URL)}, token: "fixture-token"}
		_, err := flow.Wait(context.Background())
		srv.Close()
		if err == nil {
			t.Fatal("unconfirmed login accepted")
		}
		if strings.Contains(err.Error(), "fixture-token") {
			t.Fatal("token leaked")
		}
	}
}

func TestRainQRRejectsMissingSuccessCode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: "sessionid", Value: "fixture-session", Path: "/"})
		w.Write([]byte(`{"data":{}}`))
	}))
	defer srv.Close()
	flow := &RainLogin{base: rainOrigin, client: &http.Client{Transport: rainLocalTransport(srv.URL)}, token: "fixture-token"}
	if _, err := flow.Wait(context.Background()); err == nil {
		t.Fatal("cookie alone treated as a successful login")
	}
}
