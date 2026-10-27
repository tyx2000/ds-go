package model

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func business(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": map[string]any{"biz_code": 0, "biz_data": value}})
}

func TestAccountPKCECommitBeforeCallbackRedirect(t *testing.T) {
	initialized := make(chan map[string]string, 1)
	var saved atomic.Bool
	var exchangeCalls atomic.Int32
	var platform *httptest.Server
	platform = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-client-platform") != "web" || r.Header.Get("x-client-locale") != "zh_CN" || r.Header.Get("x-client-timezone-offset") != "28800" {
			t.Error("incorrect client metadata")
		}
		switch r.URL.Path {
		case "/auth-api/v0/dsh/auth_init":
			var body map[string]string
			if json.NewDecoder(r.Body).Decode(&body) != nil {
				t.Error("invalid body")
			}
			if body["code_challenge_method"] != "S256" || body["login_source"] != "web" || body["locale"] != "zh_CN" {
				t.Error("invalid init")
			}
			initialized <- body
			business(w, map[string]any{"authorize_url": platform.URL + "/dsh/authorize?ticket=opaque", "authorize_id": "attempt-1", "expires_in": 30})
		case "/auth-api/v0/dsh/auth_exchange":
			exchangeCalls.Add(1)
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["code"] != "auth-code" || len(body["code_verifier"]) < 43 || !validUUID(body["device_id"]) {
				t.Error("invalid exchange")
			}
			if r.Header.Get("x-dsh-auth-token") != "" {
				t.Error("exchange must not carry old account token")
			}
			business(w, map[string]any{"token": "opaque-grant", "authorized_url": platform.URL + "/dsh/authorized?retained=value"})
		case "/auth-api/v0/users/current":
			if r.Method != "GET" || r.Header.Get("x-dsh-auth-token") != "opaque-grant" {
				t.Error("invalid current auth")
			}
			business(w, map[string]any{"id": "uid1", "email": "e***@example.com", "mobile": "1***2", "id_profile": map[string]any{"name": "User", "picture": nil}, "token": "must-not-appear"})
		case "/auth-api/v0/users/logout":
			if r.Method != "POST" || r.Header.Get("x-dsh-auth-token") != "opaque-grant" {
				t.Error("invalid logout")
			}
			business(w, nil)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer platform.Close()
	account, err := NewAccount(AccountOptions{PlatformOrigin: platform.URL, AllowLoopbackHTTP: true, Locale: "zh-CN", TimezoneOffsetSeconds: 28800, ClientVersion: "0.1.0"})
	if err != nil {
		t.Fatal(err)
	}
	browserDone := make(chan error, 1)
	var challenge string
	grant, err := account.Login(context.Background(), func(authorizeURL string) error {
		if authorizeURL != platform.URL+"/dsh/authorize?ticket=opaque" {
			t.Error("unexpected browser destination")
		}
		init := <-initialized
		challenge = init["code_challenge"]
		go func() {
			client := &http.Client{Timeout: 2 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
			bad, err := client.Get(init["redirect_uri"] + "?state=wrong&code=auth-code")
			if err != nil {
				browserDone <- err
				return
			}
			bad.Body.Close()
			if bad.StatusCode != 400 {
				browserDone <- fmt.Errorf("bad state accepted")
				return
			}
			callback := init["redirect_uri"] + "?state=" + url.QueryEscape(init["state"]) + "&code=auth-code"
			response, err := client.Get(callback)
			if err != nil {
				browserDone <- err
				return
			}
			defer response.Body.Close()
			if response.StatusCode != 302 || !saved.Load() {
				browserDone <- fmt.Errorf("redirect preceded commit")
				return
			}
			location, _ := url.Parse(response.Header.Get("Location"))
			if location.Path != "/dsh/authorized" || location.Query().Get("login_source") != "web" || location.Query().Get("retained") != "value" {
				browserDone <- fmt.Errorf("invalid completion location")
				return
			}
			browserDone <- nil
		}()
		return nil
	}, func(grant AccountGrant) error {
		if grant.Token != "opaque-grant" || grant.Issuer != platform.URL || grant.Version != 1 {
			t.Error("invalid stored grant")
		}
		saved.Store(true)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := <-browserDone; err != nil {
		t.Fatal(err)
	}
	if grant.Token != "opaque-grant" || exchangeCalls.Load() != 1 || challenge == "" {
		t.Fatal("missing exchange")
	}
	profile, err := account.Profile(context.Background(), grant)
	if err != nil || profile.ID != "uid1" || profile.Name != "User" || profile.Contact != "1***2" {
		t.Fatalf("profile %#v %v", profile, err)
	}
	encoded, _ := json.Marshal(profile)
	if strings.Contains(string(encoded), "must-not-appear") {
		t.Fatal("profile leaked extra fields")
	}
	if err := account.Logout(context.Background(), grant); err != nil {
		t.Fatal(err)
	}
	grant.Issuer = "https://other.example"
	if err := account.Logout(context.Background(), grant); err == nil {
		t.Fatal("foreign grant accepted")
	}
}

func TestAccountPKCEProofAndCancellation(t *testing.T) {
	var challenge string
	canceled := make(chan struct{}, 1)
	initialized := make(chan map[string]string, 1)
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		if r.URL.Path == "/auth-api/v0/dsh/auth_init" {
			challenge = body["code_challenge"]
			initialized <- body
			business(w, map[string]any{"authorize_url": server.URL + "/dsh/authorize", "authorize_id": "id", "expires_in": 30})
			return
		}
		if r.URL.Path == "/auth-api/v0/dsh/auth_cancel" {
			digest := sha256.Sum256([]byte(body["code_verifier"]))
			if body["authorize_id"] != "id" || base64.RawURLEncoding.EncodeToString(digest[:]) != challenge || r.Header.Get("x-dsh-auth-token") != "" {
				t.Error("invalid cancellation proof")
			}
			business(w, nil)
			canceled <- struct{}{}
			return
		}
		t.Errorf("unexpected request %s", r.URL.Path)
		w.WriteHeader(400)
	}))
	defer server.Close()
	account, _ := NewAccount(AccountOptions{PlatformOrigin: server.URL, AllowLoopbackHTTP: true})
	ctx, cancel := context.WithCancel(context.Background())
	_, err := account.Login(ctx, func(string) error { <-initialized; cancel(); return nil }, func(AccountGrant) error { t.Error("canceled login saved grant"); return nil })
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error %v", err)
	}
	select {
	case <-canceled:
	case <-time.After(2 * time.Second):
		t.Fatal("missing remote cancellation")
	}
}

func TestAccountRejectsUnsafeOriginsAndAuthorizationURLs(t *testing.T) {
	for _, origin := range []string{"http://platform.deepseek.com", "https://user:password@platform.deepseek.com", "https://platform.deepseek.com/path"} {
		if _, err := NewAccount(AccountOptions{PlatformOrigin: origin, AllowLoopbackHTTP: true}); err == nil {
			t.Fatalf("accepted %s", origin)
		}
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		business(w, map[string]any{"authorize_url": "https://evil.example/dsh/authorize?token=secret", "authorize_id": "id", "expires_in": 30})
	}))
	defer server.Close()
	account, _ := NewAccount(AccountOptions{PlatformOrigin: server.URL, AllowLoopbackHTTP: true})
	_, err := account.Login(context.Background(), func(string) error { t.Error("unsafe URL exposed"); return nil }, func(AccountGrant) error { t.Error("saved unsafe grant"); return nil })
	assertCode(t, err, "ACCOUNT_PROTOCOL")
	if strings.Contains(err.Error(), "secret") {
		t.Fatal("leaked provider URL")
	}
}
