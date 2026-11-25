package auth

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

const (
	// PathStart begins a sign-in; PathCallback is where the provider sends
	// the person back.
	PathStart    = "/auth/oidc/start"
	PathCallback = "/auth/oidc/callback"
	flowCookie   = "mandate_oidc"
	flowTtl      = 10 * time.Minute
)

// oidcClient runs the authorization code flow with PKCE against one
// provider. The provider's metadata is fetched on first use, so the server
// starts without reaching it.
type oidcClient struct {
	service *Service
	config  OidcConfig

	mu       sync.Mutex
	provider *oidc.Provider
	oauth    *oauth2.Config
}

func newOidcClient(service *Service, config OidcConfig) *oidcClient {
	if config.RedirectUrl == "" {
		config.RedirectUrl = strings.TrimSuffix(service.config.PublicUrl, "/") + PathCallback
	}
	return &oidcClient{service: service, config: config}
}

func (c *oidcClient) setup(ctx context.Context) (*oidc.Provider, *oauth2.Config, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.provider != nil {
		return c.provider, c.oauth, nil
	}
	provider, err := oidc.NewProvider(ctx, c.config.Issuer)
	if err != nil {
		return nil, nil, fmt.Errorf("oidc provider: %w", err)
	}
	c.provider = provider
	c.oauth = &oauth2.Config{
		ClientID: c.config.ClientId, ClientSecret: c.config.ClientSecret, Endpoint: provider.Endpoint(),
		RedirectURL: c.config.RedirectUrl, Scopes: []string{oidc.ScopeOpenID, "email", "profile"},
	}
	return c.provider, c.oauth, nil
}

// Mount adds the start and callback handlers.
func (s *Service) Mount(mux *http.ServeMux) {
	if s.oidc == nil {
		return
	}
	mux.HandleFunc(PathStart, s.oidc.start)
	mux.HandleFunc(PathCallback, s.oidc.callback)
}

// HasOidc says whether a provider is configured.
func (s *Service) HasOidc() bool {
	return s.oidc != nil
}

func (c *oidcClient) secure() bool {
	return strings.HasPrefix(c.service.config.PublicUrl, "https://")
}

func (c *oidcClient) start(w http.ResponseWriter, r *http.Request) {
	_, oauth, err := c.setup(r.Context())
	if err != nil {
		slog.Error("oidc start", "error", err)
		http.Error(w, "sign-in is not available right now", http.StatusBadGateway)
		return
	}
	state, err := random()
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	verifier := oauth2.GenerateVerifier()
	http.SetCookie(w, &http.Cookie{ //nolint:gosec // Secure follows the public URL; plain http on localhost cannot carry a Secure cookie
		Name: flowCookie, Value: state + "." + verifier, Path: "/auth/oidc/", HttpOnly: true, Secure: c.secure(),
		SameSite: http.SameSiteLaxMode, MaxAge: int(flowTtl.Seconds()),
	})
	http.Redirect(w, r, oauth.AuthCodeURL(state, oauth2.S256ChallengeOption(verifier)), http.StatusFound)
}

type claims struct {
	Email         string `json:"email"`
	EmailVerified bool   `json:"email_verified"`
	Name          string `json:"name"`
}

func (c *oidcClient) callback(w http.ResponseWriter, r *http.Request) {
	provider, oauth, err := c.setup(r.Context())
	if err != nil {
		slog.Error("oidc callback", "error", err)
		http.Error(w, "sign-in is not available right now", http.StatusBadGateway)
		return
	}
	cookie, err := r.Cookie(flowCookie)
	if err != nil {
		http.Error(w, "the sign-in took too long; start again", http.StatusBadRequest)
		return
	}
	state, verifier, ok := strings.Cut(cookie.Value, ".")
	if !ok || state == "" || r.URL.Query().Get("state") != state {
		http.Error(w, "the sign-in did not match; start again", http.StatusBadRequest)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: flowCookie, Value: "", Path: "/auth/oidc/", MaxAge: -1, HttpOnly: true, Secure: c.secure(), SameSite: http.SameSiteLaxMode}) //nolint:gosec // Secure follows the public URL; plain http on localhost cannot carry a Secure cookie
	if msg := r.URL.Query().Get("error"); msg != "" {
		http.Error(w, "the provider refused the sign-in", http.StatusUnauthorized)
		return
	}
	token, err := oauth.Exchange(r.Context(), r.URL.Query().Get("code"), oauth2.VerifierOption(verifier))
	if err != nil {
		slog.Warn("oidc exchange failed", "error", err)
		http.Error(w, "the sign-in could not be completed", http.StatusUnauthorized)
		return
	}
	rawId, ok := token.Extra("id_token").(string)
	if !ok {
		http.Error(w, "the provider sent no identity", http.StatusUnauthorized)
		return
	}
	idToken, err := provider.Verifier(&oidc.Config{ClientID: c.config.ClientId}).Verify(r.Context(), rawId)
	if err != nil {
		slog.Warn("oidc id token invalid", "error", err)
		http.Error(w, "the identity could not be verified", http.StatusUnauthorized)
		return
	}
	var who claims
	if err := idToken.Claims(&who); err != nil || who.Email == "" {
		http.Error(w, "the provider sent no email address", http.StatusUnauthorized)
		return
	}
	// An unverified address could be anyone's: a provider that lets people
	// type their own email must not let them sign in as an owner.
	if !who.EmailVerified {
		slog.Warn("oidc sign-in refused: email not verified")
		http.Error(w, "the provider has not verified the email address", http.StatusUnauthorized)
		return
	}
	session, err := c.service.createSession(r.Context(), who.Email, who.Name)
	if err != nil {
		slog.Error("oidc session", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	http.SetCookie(w, SessionCookie(session, c.service.config.SessionTtl, c.secure()))
	http.Redirect(w, r, "/", http.StatusFound)
}

// SessionCookie is the cookie that carries a session to the browser.
// Secure follows the public URL: plain http on localhost cannot carry a
// Secure cookie, and hosted deployments are https.
func SessionCookie(token string, ttl time.Duration, secure bool) *http.Cookie {
	return &http.Cookie{ //nolint:gosec // see above
		Name: CookieName, Value: token, Path: "/", HttpOnly: true, Secure: secure, SameSite: http.SameSiteLaxMode,
		MaxAge: int(ttl.Seconds()),
	}
}

// ClearedSessionCookie removes the session cookie.
func ClearedSessionCookie(secure bool) *http.Cookie {
	return &http.Cookie{Name: CookieName, Value: "", Path: "/", HttpOnly: true, Secure: secure, SameSite: http.SameSiteLaxMode, MaxAge: -1} //nolint:gosec // Secure follows the public URL
}

func random() (string, error) {
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		return "", errors.New("no randomness")
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}
