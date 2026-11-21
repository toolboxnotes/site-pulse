package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Endpoints are variables so tests can point them at a fake server.
var (
	googleAuthURL     = "https://accounts.google.com/o/oauth2/v2/auth"
	googleTokenURL    = "https://oauth2.googleapis.com/token"
	googleRevokeURL   = "https://oauth2.googleapis.com/revoke"
	googleUserinfoURL = "https://openidconnect.googleapis.com/v1/userinfo"
)

const scopeWebmasters = "https://www.googleapis.com/auth/webmasters"

var googleScopes = []string{
	"openid",
	"https://www.googleapis.com/auth/userinfo.email",
	"https://www.googleapis.com/auth/analytics.readonly",
	// full Search Console scope (v1.3.7): lets Site Pulse resubmit sitemaps when new
	// pages appear. Everything else it does with Search Console is still read-only.
	scopeWebmasters,
	"https://www.googleapis.com/auth/adsense.readonly",
}

var ErrNotSignedIn = errors.New("not_signed_in")
var ErrTokenRevoked = errors.New("token_revoked")

type pendingAuth struct {
	verifier string
	redirect string
	created  time.Time
}

var (
	pendingMu sync.Mutex
	pending   = map[string]pendingAuth{}
)

func randString(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// parseClientJSON accepts the JSON downloaded from Google Cloud ("installed" or "web").
func parseClientJSON(b []byte) (id, secret string, err error) {
	var raw map[string]struct {
		ClientID     string `json:"client_id"`
		ClientSecret string `json:"client_secret"`
	}
	if err = json.Unmarshal(b, &raw); err != nil {
		return "", "", fmt.Errorf("invalid JSON: %w", err)
	}
	for _, k := range []string{"installed", "web"} {
		if c, ok := raw[k]; ok && c.ClientID != "" {
			return c.ClientID, c.ClientSecret, nil
		}
	}
	return "", "", errors.New("client_id not found")
}

func (a *App) authStartURL(port int) (string, error) {
	a.store.mu.Lock()
	cid := a.store.Config.ClientID
	a.store.mu.Unlock()
	if cid == "" {
		return "", errors.New("no_client")
	}
	state := randString(24)
	verifier := randString(48)
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])
	redirect := fmt.Sprintf("http://127.0.0.1:%d/", port)
	pendingMu.Lock()
	for k, p := range pending {
		if time.Since(p.created) > 30*time.Minute {
			delete(pending, k)
		}
	}
	pending[state] = pendingAuth{verifier: verifier, redirect: redirect, created: time.Now()}
	pendingMu.Unlock()
	q := url.Values{
		"client_id":             {cid},
		"redirect_uri":          {redirect},
		"response_type":         {"code"},
		"scope":                 {strings.Join(googleScopes, " ")},
		"access_type":           {"offline"},
		"prompt":                {"consent"},
		"state":                 {state},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
	}
	return googleAuthURL + "?" + q.Encode(), nil
}

type tokenResp struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int64  `json:"expires_in"`
	Scope        string `json:"scope"`
	Error        string `json:"error"`
	ErrorDesc    string `json:"error_description"`
}

func postForm(ctx context.Context, u string, v url.Values) (*tokenResp, error) {
	req, _ := http.NewRequestWithContext(ctx, "POST", u, strings.NewReader(v.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	res, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	var t tokenResp
	b, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	json.Unmarshal(b, &t)
	if res.StatusCode != 200 || t.Error != "" {
		if t.Error == "invalid_grant" {
			return nil, ErrTokenRevoked
		}
		msg := t.Error
		if t.ErrorDesc != "" {
			msg += ": " + t.ErrorDesc
		}
		if msg == "" {
			msg = res.Status
		}
		return nil, errors.New(msg)
	}
	return &t, nil
}

func (a *App) finishAuth(ctx context.Context, state, code string) error {
	pendingMu.Lock()
	p, ok := pending[state]
	delete(pending, state)
	authReturnedAt = time.Now()
	pendingMu.Unlock()
	if !ok {
		return errors.New("invalid or expired state")
	}
	a.store.mu.Lock()
	cid, sec := a.store.Config.ClientID, a.store.Config.ClientSecret
	a.store.mu.Unlock()
	t, err := postForm(ctx, googleTokenURL, url.Values{
		"code":          {code},
		"client_id":     {cid},
		"client_secret": {sec},
		"redirect_uri":  {p.redirect},
		"grant_type":    {"authorization_code"},
		"code_verifier": {p.verifier},
	})
	if err != nil {
		return err
	}
	if t.RefreshToken == "" {
		return errors.New("no refresh token returned")
	}
	tok := &Token{AccessToken: t.AccessToken, RefreshToken: t.RefreshToken,
		Expiry: time.Now().Unix() + t.ExpiresIn - 60, Scope: t.Scope}
	// who signed in
	var ui struct {
		Email string `json:"email"`
	}
	if err := getJSON(ctx, t.AccessToken, googleUserinfoURL, &ui); err == nil {
		tok.Email = ui.Email
	}
	a.store.mu.Lock()
	a.store.Token = tok
	err = a.store.saveToken()
	a.store.mu.Unlock()
	return err
}

// accessToken returns a valid access token, refreshing if needed.
func (a *App) accessToken(ctx context.Context) (string, error) {
	a.tokMu.Lock()
	defer a.tokMu.Unlock()
	a.store.mu.Lock()
	tok := a.store.Token
	cid, sec := a.store.Config.ClientID, a.store.Config.ClientSecret
	a.store.mu.Unlock()
	if tok == nil {
		return "", ErrNotSignedIn
	}
	if tok.AccessToken != "" && time.Now().Unix() < tok.Expiry {
		return tok.AccessToken, nil
	}
	t, err := postForm(ctx, googleTokenURL, url.Values{
		"client_id":     {cid},
		"client_secret": {sec},
		"refresh_token": {tok.RefreshToken},
		"grant_type":    {"refresh_token"},
	})
	if err != nil {
		return "", err
	}
	a.store.mu.Lock()
	nt := *tok
	nt.AccessToken = t.AccessToken
	nt.Expiry = time.Now().Unix() + t.ExpiresIn - 60
	if t.Scope != "" {
		nt.Scope = t.Scope
	}
	a.store.Token = &nt
	a.store.saveToken()
	a.store.mu.Unlock()
	return t.AccessToken, nil
}

func (a *App) signOut(ctx context.Context) {
	a.store.mu.Lock()
	tok := a.store.Token
	a.store.Token = nil
	a.store.saveToken()
	a.store.mu.Unlock()
	if tok != nil {
		postForm(ctx, googleRevokeURL, url.Values{"token": {tok.RefreshToken}})
	}
}

// ---------- authenticated JSON helpers ----------

type APIError struct {
	Status  int
	Message string
}

func (e *APIError) Error() string { return fmt.Sprintf("HTTP %d: %s", e.Status, e.Message) }

func doJSON(ctx context.Context, token, method, u string, body any, out any) error {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(res.Body, 32<<20))
	if res.StatusCode >= 300 {
		var ge struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		json.Unmarshal(b, &ge)
		msg := ge.Error.Message
		if msg == "" {
			msg = strings.TrimSpace(string(b))
			if len(msg) > 300 {
				msg = msg[:300]
			}
		}
		return &APIError{Status: res.StatusCode, Message: msg}
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(b, out)
}

func getJSON(ctx context.Context, token, u string, out any) error {
	return doJSON(ctx, token, "GET", u, nil, out)
}

// canSubmitSitemaps reports whether the signed-in token carries the full Search Console
// scope. Tokens from Site Pulse 1.3.6 and earlier are read-only until the user signs in again.
func (a *App) canSubmitSitemaps() bool {
	a.store.mu.Lock()
	defer a.store.mu.Unlock()
	if a.store.Token == nil {
		return false
	}
	for _, sc := range strings.Fields(a.store.Token.Scope) {
		if sc == scopeWebmasters {
			return true
		}
	}
	return false
}

// signingIn reports whether a Google sign-in was started in the last 10 minutes and has not
// come back yet. While the window shows Google's pages it is not connected to Site Pulse, so
// the app must not quit then (otherwise Google's redirect back finds nothing listening).
var authReturnedAt time.Time // when Google last redirected back (guarded by pendingMu)

func signingIn() bool {
	pendingMu.Lock()
	defer pendingMu.Unlock()
	if time.Since(authReturnedAt) < 30*time.Second { // give the page time to reconnect
		return true
	}
	for _, p := range pending {
		if time.Since(p.created) < 10*time.Minute {
			return true
		}
	}
	return false
}
