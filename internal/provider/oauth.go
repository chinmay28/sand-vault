package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// tokenSource exchanges a long-lived refresh token for short-lived access
// tokens and caches the result until shortly before it expires. Both Google
// Drive and Dropbox use the same OAuth2 refresh grant, so they share this.
type tokenSource struct {
	tokenURL     string
	clientID     string
	clientSecret string
	refreshToken string

	// staticToken short-circuits refreshing entirely, for backends that also
	// accept a directly supplied access token.
	staticToken string

	mu        sync.Mutex
	token     string
	expiresAt time.Time
}

// token returns a valid access token, refreshing if the cached one is missing
// or within a minute of expiring.
func (ts *tokenSource) accessToken(ctx context.Context) (string, error) {
	if ts.staticToken != "" && ts.refreshToken == "" {
		return ts.staticToken, nil
	}

	ts.mu.Lock()
	defer ts.mu.Unlock()

	if ts.token != "" && time.Now().Before(ts.expiresAt.Add(-time.Minute)) {
		return ts.token, nil
	}

	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", ts.refreshToken)
	form.Set("client_id", ts.clientID)
	form.Set("client_secret", ts.clientSecret)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, ts.tokenURL,
		strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("refreshing access token: %w", err)
	}
	defer drainAndClose(resp)
	if !isSuccess(resp.StatusCode) {
		return "", httpError("refreshing access token", resp)
	}

	var payload struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int64  `json:"expires_in"`
	}
	body, err := readAllBody(resp)
	if err != nil {
		return "", err
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return "", fmt.Errorf("parsing token response: %w", err)
	}
	if payload.AccessToken == "" {
		return "", fmt.Errorf("token endpoint returned no access token")
	}

	ts.token = payload.AccessToken
	if payload.ExpiresIn > 0 {
		ts.expiresAt = time.Now().Add(time.Duration(payload.ExpiresIn) * time.Second)
	} else {
		ts.expiresAt = time.Now().Add(time.Hour)
	}
	return ts.token, nil
}

// authorize attaches a bearer token to a request.
func (ts *tokenSource) authorize(ctx context.Context, req *http.Request) error {
	token, err := ts.accessToken(ctx)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	return nil
}
