package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sync"
	"time"
)

// Pushover's monthly quota, per application token. Every send answers with
// X-Limit-App-Remaining; the plugin keeps the latest reading for each token
// and warns an admin on their page when it runs low. The readings live in the
// "quota" state key (all API servers share it) under a hash of the token, so
// no token is stored.

const (
	quotaKey = "quota"
	// quotaLowPercent is the share of the monthly limit under which the page
	// warns.
	quotaLowPercent = 10
	// quotaPersistEvery limits how often a token's reading is written to
	// state. Sends are frequent; the page only needs a recent number.
	quotaPersistEvery = time.Minute
	maxQuotaEntries   = 50
)

// quotaEntry is the last reading for one token. Reset is when Pushover next
// refills it (Unix seconds); once that has passed, the reading is out of date.
type quotaEntry struct {
	Limit     int       `json:"limit"`
	Remaining int       `json:"remaining"`
	Reset     int64     `json:"reset"`
	At        time.Time `json:"at"`
}

// low reports whether less than quotaLowPercent of the limit is left.
func (e quotaEntry) low() bool {
	return e.Limit > 0 && e.Remaining*100 < e.Limit*quotaLowPercent
}

// quotaTable is the "quota" key, by token hash.
type quotaTable map[string]quotaEntry

func (t quotaTable) entries() int { return len(t) }

func (t quotaTable) dropOldest(n int) {
	for _, k := range oldestFirst(t, func(e quotaEntry) time.Time { return e.At })[:min(n, len(t))] {
		delete(t, k)
	}
}

func tokenID(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:8])
}

// quotaStore remembers quota readings in this process and in state.
type quotaStore struct {
	state stateBackend
	now   func() time.Time

	mu        sync.Mutex
	mem       quotaTable
	persisted map[string]time.Time
	// wasLow notes tokens last seen under the warning line, so crossing it is
	// written at once.
	wasLow map[string]bool
}

func newQuotaStore(state stateBackend) *quotaStore {
	return &quotaStore{state: state, now: time.Now, mem: quotaTable{}, persisted: map[string]time.Time{}, wasLow: map[string]bool{}}
}

// record notes a reading for token. It writes to state at most once a minute
// per token, and at once when the token drops under the warning line. A state
// failure is ignored: the reading is only for the page.
func (s *quotaStore) record(ctx context.Context, token string, q quota) {
	if token == "" || q.Limit <= 0 {
		return
	}
	id := tokenID(token)
	now := s.now()
	entry := quotaEntry{Limit: q.Limit, Remaining: q.Remaining, Reset: q.Reset, At: now}

	s.mu.Lock()
	s.mem[id] = entry
	crossed := entry.low() && !s.wasLow[id]
	s.wasLow[id] = entry.low()
	last, seen := s.persisted[id]
	skip := seen && now.Sub(last) < quotaPersistEvery && !crossed
	if !skip {
		s.persisted[id] = now
	}
	s.mu.Unlock()
	if skip {
		return
	}

	// Two sends finishing together may both write; the later one wins, which
	// is the newer reading or close to it.
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), recordTimeout)
	defer cancel()
	table, raw, err := s.read(wctx)
	if err != nil {
		return
	}
	table[id] = entry
	if next, err := encodeCapped(table); err == nil && string(next) != string(raw) {
		_ = s.state.Write(wctx, quotaKey, next)
	}
}

func (s *quotaStore) read(ctx context.Context) (quotaTable, []byte, error) {
	raw, found, err := s.state.Read(ctx, quotaKey)
	if err != nil {
		return nil, nil, err
	}
	table := quotaTable{}
	if found && json.Unmarshal(raw, &table) != nil {
		table = quotaTable{}
	}
	return table, raw, nil
}

// get returns the newest reading for token, from this process or from state
// (another API server's). A reading from before Pushover's last reset says
// nothing about this month, so it isn't returned.
func (s *quotaStore) get(ctx context.Context, token string) (quotaEntry, bool) {
	if token == "" {
		return quotaEntry{}, false
	}
	id := tokenID(token)
	s.mu.Lock()
	best, ok := s.mem[id]
	s.mu.Unlock()
	if stored, _, err := s.read(ctx); err == nil {
		if e, found := stored[id]; found && (!ok || e.At.After(best.At)) {
			best, ok = e, true
		}
	}
	if !ok || (best.Reset > 0 && best.Reset <= s.now().Unix()) {
		return quotaEntry{}, false
	}
	return best, true
}

// quotaView is a reading as the page shows it.
type quotaView struct {
	Limit     int   `json:"limit"`
	Remaining int   `json:"remaining"`
	Reset     int64 `json:"reset,omitempty"`
	Low       bool  `json:"low"`
}

func (e quotaEntry) view() *quotaView {
	return &quotaView{Limit: e.Limit, Remaining: e.Remaining, Reset: e.Reset, Low: e.low()}
}
