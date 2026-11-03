package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"sync"
	"time"
)

var (
	errSiloAPIKeyMissing  = errors.New("the Silo admin API key is not configured")
	errSiloAPIKeyRejected = errors.New("Silo rejected the admin API key; it must be an unscoped key owned by an admin account")
)

type siloProfile struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// siloClient calls Silo's own API with the admin-configured API key.
type siloClient struct {
	http *http.Client

	mu    sync.Mutex
	cache map[int]cachedProfiles
}

type cachedProfiles struct {
	profiles []siloProfile
	at       time.Time
}

const profileCacheTTL = 30 * time.Second

func newSiloClient() *siloClient {
	return &siloClient{http: &http.Client{Timeout: 10 * time.Second}, cache: map[int]cachedProfiles{}}
}

// Invalidate drops cached lookups, e.g. after the API key changes.
func (c *siloClient) Invalidate() {
	c.mu.Lock()
	c.cache = map[int]cachedProfiles{}
	c.mu.Unlock()
}

// UserProfiles lists the profiles of one Silo account.
func (c *siloClient) UserProfiles(ctx context.Context, baseURL, apiKey string, userID int) ([]siloProfile, error) {
	if apiKey == "" {
		return nil, errSiloAPIKeyMissing
	}
	c.mu.Lock()
	if cached, ok := c.cache[userID]; ok && time.Since(cached.at) < profileCacheTTL {
		c.mu.Unlock()
		return cached.profiles, nil
	}
	c.mu.Unlock()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		baseURL+"/api/v2/admin/users/"+strconv.Itoa(userID)+"/profiles", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("list profiles: %w", err)
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusUnauthorized, resp.StatusCode == http.StatusForbidden:
		return nil, errSiloAPIKeyRejected
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("list profiles: Silo returned HTTP %d", resp.StatusCode)
	}

	var body struct {
		Items []siloProfile `json:"items"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body); err != nil {
		return nil, fmt.Errorf("list profiles: %w", err)
	}

	c.mu.Lock()
	c.cache[userID] = cachedProfiles{profiles: body.Items, at: time.Now()}
	c.mu.Unlock()
	return body.Items, nil
}
