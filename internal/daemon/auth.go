package daemon

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/rpajarola/mirrortools/internal/config"
)

const (
	sessionCookieName = "mirrord_session"
	sessionDuration   = 14 * 24 * time.Hour
)

// UserContextKey is used to store authenticated user info in context.
type userContextKey struct{}

// GetUser returns the authenticated username or email from ctx.
func GetUser(ctx context.Context) string {
	if u, ok := ctx.Value(userContextKey{}).(string); ok {
		return u
	}
	return ""
}

// Session represents an authenticated session.
type Session struct {
	Token     string
	User      string
	CreatedAt time.Time
	ExpiresAt time.Time
}

// AuthManager coordinates password and OAuth authentication.
type AuthManager struct {
	cfg          config.AuthConfig
	password     string
	clientSecret string

	mu           sync.RWMutex
	sessions     map[string]*Session
	oauthStates  map[string]time.Time
	allowedUsers map[string]struct{}

	// Resolved OAuth endpoints
	authURL     string
	tokenURL    string
	userinfoURL string
	scopes      string
}

// NewAuthManager initializes authentication from configuration.
func NewAuthManager(cfg config.AuthConfig) (*AuthManager, error) {
	pw, err := cfg.PasswordValue()
	if err != nil {
		return nil, err
	}
	secret, err := cfg.OAuth.ClientSecretValue()
	if err != nil {
		return nil, err
	}

	am := &AuthManager{
		cfg:          cfg,
		password:     pw,
		clientSecret: secret,
		sessions:     make(map[string]*Session),
		oauthStates:  make(map[string]time.Time),
		allowedUsers: make(map[string]struct{}),
	}

	for _, u := range cfg.OAuth.AllowedUsers {
		am.allowedUsers[strings.ToLower(strings.TrimSpace(u))] = struct{}{}
	}

	if cfg.OAuth.Enabled() {
		am.resolveOAuthEndpoints()
	}

	return am, nil
}

func (a *AuthManager) resolveOAuthEndpoints() {
	o := a.cfg.OAuth
	provider := strings.ToLower(o.Provider)

	switch provider {
	case "google":
		a.authURL = "https://accounts.google.com/o/oauth2/v2/auth"
		a.tokenURL = "https://oauth2.googleapis.com/token"
		a.userinfoURL = "https://openidconnect.googleapis.com/v1/userinfo"
		if len(o.Scopes) == 0 {
			a.scopes = "openid email profile"
		}
	case "github":
		a.authURL = "https://github.com/login/oauth/authorize"
		a.tokenURL = "https://github.com/login/oauth/access_token"
		a.userinfoURL = "https://api.github.com/user"
		if len(o.Scopes) == 0 {
			a.scopes = "read:user user:email"
		}
	default:
		a.authURL = o.AuthURL
		a.tokenURL = o.TokenURL
		a.userinfoURL = o.UserInfoURL
	}

	if len(o.Scopes) > 0 {
		a.scopes = strings.Join(o.Scopes, " ")
	}

	// If Issuer is given, try OpenID Discovery if explicit endpoints are missing
	if o.Issuer != "" && (a.authURL == "" || a.tokenURL == "" || a.userinfoURL == "") {
		discURL := strings.TrimSuffix(o.Issuer, "/") + "/.well-known/openid-configuration"
		client := &http.Client{Timeout: 5 * time.Second}
		resp, err := client.Get(discURL)
		if err == nil && resp.StatusCode == http.StatusOK {
			defer resp.Body.Close()
			var doc struct {
				Auth     string `json:"authorization_endpoint"`
				Token    string `json:"token_endpoint"`
				Userinfo string `json:"userinfo_endpoint"`
			}
			if json.NewDecoder(resp.Body).Decode(&doc) == nil {
				if a.authURL == "" {
					a.authURL = doc.Auth
				}
				if a.tokenURL == "" {
					a.tokenURL = doc.Token
				}
				if a.userinfoURL == "" {
					a.userinfoURL = doc.Userinfo
				}
				if a.scopes == "" {
					a.scopes = "openid email profile"
				}
			}
		}
	}
}

// Enabled reports whether any authentication check is active.
func (a *AuthManager) Enabled() bool {
	return a != nil && a.cfg.Enabled()
}

// PasswordEnabled reports whether password login is available.
func (a *AuthManager) PasswordEnabled() bool {
	return a != nil && a.password != ""
}

// OAuthEnabled reports whether OAuth login is available.
func (a *AuthManager) OAuthEnabled() bool {
	return a != nil && a.cfg.OAuth.Enabled() && a.authURL != "" && a.tokenURL != ""
}

// ProviderName returns a user-friendly label for the OAuth provider.
func (a *AuthManager) ProviderName() string {
	if a == nil {
		return ""
	}
	p := a.cfg.OAuth.Provider
	if p == "" {
		return "Single Sign-On"
	}
	switch strings.ToLower(p) {
	case "google":
		return "Google"
	case "github":
		return "GitHub"
	default:
		return strings.ToUpper(p[:1]) + p[1:]
	}
}

// CheckPassword verifies the given password in constant time.
func (a *AuthManager) CheckPassword(pass string) bool {
	if a == nil || a.password == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(pass), []byte(a.password)) == 1
}

// CreateSession generates a new authenticated session for user.
func (a *AuthManager) CreateSession(user string) *http.Cookie {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	token := hex.EncodeToString(b)

	now := time.Now().UTC()
	sess := &Session{
		Token:     token,
		User:      user,
		CreatedAt: now,
		ExpiresAt: now.Add(sessionDuration),
	}

	a.mu.Lock()
	a.sessions[token] = sess
	a.mu.Unlock()

	return &http.Cookie{
		Name:     sessionCookieName,
		Value:    token,
		Path:     "/",
		Expires:  sess.ExpiresAt,
		MaxAge:   int(sessionDuration.Seconds()),
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	}
}

// InvalidateSession removes the session matching token.
func (a *AuthManager) InvalidateSession(token string) {
	a.mu.Lock()
	delete(a.sessions, token)
	a.mu.Unlock()
}

// LogoutCookie returns an expired cookie to clear the session.
func (a *AuthManager) LogoutCookie() *http.Cookie {
	return &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/",
		Expires:  time.Unix(0, 0),
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	}
}

// ValidateRequest checks if the request is authenticated via session cookie, Bearer token, or Basic auth.
func (a *AuthManager) ValidateRequest(r *http.Request) (string, bool) {
	if !a.Enabled() {
		return "anonymous", true
	}

	// 1. Check Bearer token or Basic auth in Authorization header
	authHeader := r.Header.Get("Authorization")
	if authHeader != "" {
		parts := strings.SplitN(authHeader, " ", 2)
		if len(parts) == 2 {
			switch strings.ToLower(parts[0]) {
			case "bearer":
				token := strings.TrimSpace(parts[1])
				// Bearer token matches password
				if a.CheckPassword(token) {
					return "api", true
				}
				// Or bearer token matches an active session
				a.mu.RLock()
				sess, ok := a.sessions[token]
				a.mu.RUnlock()
				if ok && time.Now().UTC().Before(sess.ExpiresAt) {
					return sess.User, true
				}
			case "basic":
				u, p, ok := r.BasicAuth()
				if ok && a.CheckPassword(p) {
					if u == "" {
						u = "api"
					}
					return u, true
				}
			}
		}
	}

	// 2. Check session cookie
	cookie, err := r.Cookie(sessionCookieName)
	if err == nil && cookie.Value != "" {
		a.mu.RLock()
		sess, ok := a.sessions[cookie.Value]
		a.mu.RUnlock()
		if ok {
			if time.Now().UTC().Before(sess.ExpiresAt) {
				return sess.User, true
			}
			a.InvalidateSession(cookie.Value)
		}
	}

	return "", false
}

// OAuthStartURL generates a secure OAuth authorization URL with a random state parameter.
func (a *AuthManager) OAuthStartURL(r *http.Request) (string, error) {
	if !a.OAuthEnabled() {
		return "", errors.New("oauth is not configured")
	}

	b := make([]byte, 24)
	_, _ = rand.Read(b)
	state := hex.EncodeToString(b)

	a.mu.Lock()
	a.oauthStates[state] = time.Now().UTC().Add(10 * time.Minute)
	// Prune expired states
	for s, exp := range a.oauthStates {
		if time.Now().UTC().After(exp) {
			delete(a.oauthStates, s)
		}
	}
	a.mu.Unlock()

	redirectURI := a.cfg.OAuth.RedirectURL
	if redirectURI == "" {
		scheme := "http"
		if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
			scheme = "https"
		}
		redirectURI = fmt.Sprintf("%s://%s/auth/callback", scheme, r.Host)
	}

	vals := url.Values{}
	vals.Set("client_id", a.cfg.OAuth.ClientID)
	vals.Set("redirect_uri", redirectURI)
	vals.Set("response_type", "code")
	vals.Set("scope", a.scopes)
	vals.Set("state", state)

	separator := "?"
	if strings.Contains(a.authURL, "?") {
		separator = "&"
	}
	return a.authURL + separator + vals.Encode(), nil
}

// HandleOAuthCallback exchanges code for an access token, fetches user info, and verifies user access.
func (a *AuthManager) HandleOAuthCallback(ctx context.Context, r *http.Request) (string, error) {
	q := r.URL.Query()
	code := q.Get("code")
	state := q.Get("state")

	if code == "" || state == "" {
		return "", errors.New("missing code or state in callback")
	}

	// Verify state
	a.mu.Lock()
	exp, ok := a.oauthStates[state]
	delete(a.oauthStates, state)
	a.mu.Unlock()

	if !ok || time.Now().UTC().After(exp) {
		return "", errors.New("invalid or expired oauth state parameter")
	}

	redirectURI := a.cfg.OAuth.RedirectURL
	if redirectURI == "" {
		scheme := "http"
		if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
			scheme = "https"
		}
		redirectURI = fmt.Sprintf("%s://%s/auth/callback", scheme, r.Host)
	}

	// Exchange code for token
	tokenVals := url.Values{}
	tokenVals.Set("grant_type", "authorization_code")
	tokenVals.Set("code", code)
	tokenVals.Set("redirect_uri", redirectURI)
	tokenVals.Set("client_id", a.cfg.OAuth.ClientID)
	tokenVals.Set("client_secret", a.clientSecret)

	tokenReq, err := http.NewRequestWithContext(ctx, http.MethodPost, a.tokenURL, strings.NewReader(tokenVals.Encode()))
	if err != nil {
		return "", err
	}
	tokenReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	tokenReq.Header.Set("Accept", "application/json")

	client := &http.Client{Timeout: 10 * time.Second}
	tokenResp, err := client.Do(tokenReq)
	if err != nil {
		return "", fmt.Errorf("exchanging oauth code: %w", err)
	}
	defer tokenResp.Body.Close()

	if tokenResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(tokenResp.Body)
		return "", fmt.Errorf("token endpoint returned status %d: %s", tokenResp.StatusCode, string(body))
	}

	var tokenData struct {
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
		IDToken     string `json:"id_token"`
	}
	if err := json.NewDecoder(tokenResp.Body).Decode(&tokenData); err != nil {
		return "", fmt.Errorf("decoding token response: %w", err)
	}
	if tokenData.AccessToken == "" {
		return "", errors.New("no access token in oauth response")
	}

	// Fetch userinfo
	userInfoReq, err := http.NewRequestWithContext(ctx, http.MethodGet, a.userinfoURL, nil)
	if err != nil {
		return "", err
	}
	userInfoReq.Header.Set("Authorization", "Bearer "+tokenData.AccessToken)
	userInfoReq.Header.Set("Accept", "application/json")

	userResp, err := client.Do(userInfoReq)
	if err != nil {
		return "", fmt.Errorf("fetching userinfo: %w", err)
	}
	defer userResp.Body.Close()

	if userResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(userResp.Body)
		return "", fmt.Errorf("userinfo endpoint returned status %d: %s", userResp.StatusCode, string(body))
	}

	var userPayload map[string]any
	if err := json.NewDecoder(userResp.Body).Decode(&userPayload); err != nil {
		return "", fmt.Errorf("decoding userinfo: %w", err)
	}

	// Extract identifier: email, login, preferred_username, or sub
	user := ""
	for _, field := range []string{"email", "login", "preferred_username", "sub", "name"} {
		if val, ok := userPayload[field].(string); ok && val != "" {
			user = val
			break
		}
	}
	if user == "" {
		user = "oauth_user"
	}

	// If AllowedUsers whitelist is configured, enforce it
	if len(a.allowedUsers) > 0 {
		lowerUser := strings.ToLower(user)
		if _, ok := a.allowedUsers[lowerUser]; !ok {
			return "", fmt.Errorf("user %q is not authorized to access this service", user)
		}
	}

	return user, nil
}
