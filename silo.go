package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"
)

// Errors for the read-only Silo key. Messages never include the key.
var (
	errSiloKeyMissing  = errors.New("no read-only Silo key is configured")
	errSiloKeyRejected = errors.New("Silo rejected the read-only key (HTTP 401)")
	errSiloKeyScope    = errors.New("the read-only Silo key isn't allowed to call this route (HTTP 403)")
)

type siloProfile struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// siloClient is a read-only client for Silo's admin API, using the
// admin:monitor:read key. It only sends GET requests.
type siloClient struct {
	http    *http.Client
	timeout time.Duration
	now     func() time.Time

	mu    sync.Mutex
	cache map[int]cachedProfiles
}

// cachedProfiles is one account's profile list, or the key error that
// stopped it. Network errors aren't cached.
type cachedProfiles struct {
	profiles []siloProfile
	err      error
	at       time.Time
}

const (
	profileCacheTTL = 30 * time.Second
	siloCallTimeout = 5 * time.Second
)

func newSiloClient() *siloClient {
	return &siloClient{
		http:    &http.Client{Timeout: 10 * time.Second},
		timeout: siloCallTimeout,
		now:     time.Now,
		cache:   map[int]cachedProfiles{},
	}
}

// Invalidate drops cached lookups, e.g. after the key changes.
func (c *siloClient) Invalidate() {
	c.mu.Lock()
	c.cache = map[int]cachedProfiles{}
	c.mu.Unlock()
}

// AccountProfiles lists the profiles of one Silo account
// (GET /api/v2/admin/users/{id}/profiles, allowed by admin:monitor:read).
func (c *siloClient) AccountProfiles(ctx context.Context, baseURL, apiKey string, userID int) ([]siloProfile, error) {
	if apiKey == "" {
		return nil, errSiloKeyMissing
	}
	c.mu.Lock()
	if cached, ok := c.cache[userID]; ok && c.now().Sub(cached.at) < profileCacheTTL {
		c.mu.Unlock()
		return cached.profiles, cached.err
	}
	c.mu.Unlock()

	var body struct {
		Items []siloProfile `json:"items"`
	}
	err := c.get(ctx, baseURL, apiKey, "/api/v2/admin/users/"+strconv.Itoa(userID)+"/profiles", &body)
	if err != nil && !errors.Is(err, errSiloKeyRejected) && !errors.Is(err, errSiloKeyScope) {
		return nil, fmt.Errorf("list profiles: %w", err)
	}
	c.mu.Lock()
	c.cache[userID] = cachedProfiles{profiles: body.Items, err: err, at: c.now()}
	c.mu.Unlock()
	return body.Items, err
}

// get calls one GET route with the key and decodes the JSON answer.
func (c *siloClient) get(ctx context.Context, baseURL, apiKey, path string, out any) error {
	if baseURL == "" {
		return errors.New("Silo's internal URL is not known yet")
	}
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		// Keep the cause, not the full request line.
		var uerr *url.Error
		if errors.As(err, &uerr) {
			err = uerr.Err
		}
		return err
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		return errSiloKeyRejected
	case resp.StatusCode == http.StatusForbidden:
		return errSiloKeyScope
	case resp.StatusCode != http.StatusOK:
		return fmt.Errorf("Silo returned HTTP %d", resp.StatusCode)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(out); err != nil {
		return fmt.Errorf("decode Silo response: %w", err)
	}
	return nil
}
