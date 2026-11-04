package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/rpajarola/mirrortools/internal/config"
)

func TestAuthManagerPasswordAndSessions(t *testing.T) {
	am, err := NewAuthManager(config.AuthConfig{
		Password: "supersecretpassword123",
	})
	if err != nil {
		t.Fatalf("NewAuthManager: %v", err)
	}

	if !am.Enabled() || !am.PasswordEnabled() {
		t.Fatalf("expected password auth to be enabled")
	}
	if am.OAuthEnabled() {
		t.Fatalf("expected oauth to be disabled")
	}

	// 1. Check password
	if !am.CheckPassword("supersecretpassword123") {
		t.Errorf("expected password to match")
	}
	if am.CheckPassword("wrongpassword") {
		t.Errorf("expected wrong password to fail")
	}

	// 2. Validate unauthenticated request
	req := httptest.NewRequest("GET", "http://localhost:8080/api/v1/jobs", nil)
	if _, ok := am.ValidateRequest(req); ok {
		t.Errorf("unauthenticated request should fail validation")
	}

	// 3. Validate Bearer token matching password
	req = httptest.NewRequest("GET", "http://localhost:8080/api/v1/jobs", nil)
	req.Header.Set("Authorization", "Bearer supersecretpassword123")
	user, ok := am.ValidateRequest(req)
	if !ok || user != "api" {
		t.Errorf("bearer password auth failed: user=%q, ok=%v", user, ok)
	}

	// 4. Validate Basic Auth
	req = httptest.NewRequest("GET", "http://localhost:8080/api/v1/jobs", nil)
	req.SetBasicAuth("admin", "supersecretpassword123")
	user, ok = am.ValidateRequest(req)
	if !ok || user != "admin" {
		t.Errorf("basic auth failed: user=%q, ok=%v", user, ok)
	}

	// 5. Create Session & Cookie
	cookie := am.CreateSession("alice")
	if cookie == nil || cookie.Value == "" {
		t.Fatalf("expected valid session cookie")
	}

	req = httptest.NewRequest("GET", "http://localhost:8080/", nil)
	req.AddCookie(cookie)
	user, ok = am.ValidateRequest(req)
	if !ok || user != "alice" {
		t.Errorf("session cookie validation failed: user=%q, ok=%v", user, ok)
	}

	// 6. Invalidate Session (Logout)
	am.InvalidateSession(cookie.Value)
	if _, ok := am.ValidateRequest(req); ok {
		t.Errorf("invalidated session should fail validation")
	}
}

func TestAuthManagerOAuthFlow(t *testing.T) {
	// Mock OAuth Identity Provider
	var tokenRequests int
	var userinfoRequests int

	idpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			tokenRequests++
			_ = r.ParseForm()
			if r.FormValue("code") != "valid_test_code" {
				http.Error(w, "invalid code", http.StatusBadRequest)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]string{
				"access_token": "test_access_token_12345",
				"token_type":   "Bearer",
			})
		case "/userinfo":
			userinfoRequests++
			if r.Header.Get("Authorization") != "Bearer test_access_token_12345" {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]string{
				"email": "alice@example.com",
				"name":  "Alice Test",
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer idpServer.Close()

	am, err := NewAuthManager(config.AuthConfig{
		OAuth: config.OAuthConfig{
			Provider:     "generic",
			ClientID:     "my-client-id",
			ClientSecret: "my-client-secret",
			AuthURL:      idpServer.URL + "/authorize",
			TokenURL:     idpServer.URL + "/token",
			UserInfoURL:  idpServer.URL + "/userinfo",
			RedirectURL:  "http://localhost:8080/auth/callback",
			AllowedUsers: []string{"alice@example.com"},
		},
	})
	if err != nil {
		t.Fatalf("NewAuthManager: %v", err)
	}

	if !am.OAuthEnabled() {
		t.Fatalf("expected OAuth to be enabled")
	}

	// 1. Generate Start URL
	req := httptest.NewRequest("GET", "http://localhost:8080/auth/oauth/start", nil)
	startURL, err := am.OAuthStartURL(req)
	if err != nil {
		t.Fatalf("OAuthStartURL: %v", err)
	}

	u, err := url.Parse(startURL)
	if err != nil {
		t.Fatalf("parse startURL: %v", err)
	}
	state := u.Query().Get("state")
	if state == "" {
		t.Fatalf("expected state in startURL, got %s", startURL)
	}

	// 2. Handle Callback with valid code
	cbReq := httptest.NewRequest("GET", fmt.Sprintf("http://localhost:8080/auth/callback?code=valid_test_code&state=%s", state), nil)
	user, err := am.HandleOAuthCallback(context.Background(), cbReq)
	if err != nil {
		t.Fatalf("HandleOAuthCallback: %v", err)
	}
	if user != "alice@example.com" {
		t.Fatalf("expected user alice@example.com, got %s", user)
	}

	if tokenRequests != 1 || userinfoRequests != 1 {
		t.Errorf("tokenRequests=%d, userinfoRequests=%d", tokenRequests, userinfoRequests)
	}

	// 3. Callback with invalid / reused state should fail
	_, err = am.HandleOAuthCallback(context.Background(), cbReq)
	if err == nil {
		t.Fatalf("reused state should fail")
	}
}

func TestAuthManagerOAuthAllowedUsers(t *testing.T) {
	idpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			_ = json.NewEncoder(w).Encode(map[string]string{"access_token": "tok"})
		case "/userinfo":
			_ = json.NewEncoder(w).Encode(map[string]string{"email": "stranger@example.com"})
		}
	}))
	defer idpServer.Close()

	am, _ := NewAuthManager(config.AuthConfig{
		OAuth: config.OAuthConfig{
			ClientID:     "id",
			ClientSecret: "sec",
			AuthURL:      idpServer.URL + "/authorize",
			TokenURL:     idpServer.URL + "/token",
			UserInfoURL:  idpServer.URL + "/userinfo",
			AllowedUsers: []string{"admin@example.com"}, // stranger is not in this list
		},
	})

	req := httptest.NewRequest("GET", "http://localhost:8080/", nil)
	startURL, _ := am.OAuthStartURL(req)
	u, _ := url.Parse(startURL)
	state := u.Query().Get("state")

	cbReq := httptest.NewRequest("GET", fmt.Sprintf("http://localhost:8080/auth/callback?code=code123&state=%s", state), nil)
	_, err := am.HandleOAuthCallback(context.Background(), cbReq)
	if err == nil || !strings.Contains(err.Error(), "not authorized") {
		t.Fatalf("expected unauthorized error for non-whitelisted user, got %v", err)
	}
}
