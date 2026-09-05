// Package esi is a minimal client for the EVE Swagger Interface.
// It transparently refreshes access tokens, caches responses in memory
// and in SQLite (honouring the Expires header) and batch-resolves names.
//
// A Client can produce a StaleView: a request-scoped variant that serves
// expired cache entries instead of hitting the network and records
// whether any stale data was returned — the web layer uses this to
// render instantly from cache and revalidate asynchronously.
package esi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"eve-empire/internal/sso"
	"eve-empire/internal/store"
)

const (
	baseURL = "https://esi.evetech.net/latest"
	// compatBase serves new-style routes that only exist with an
	// X-Compatibility-Date header (e.g. corporation projects).
	compatBase = "https://esi.evetech.net"
	compatDate = "2026-01-01"
)

// maxParallelFetches bounds concurrent ESI network requests. A fresh
// sidebar sweep fires ~7 requests per alt; with dozens of alts the
// unbounded burst used to saturate the router's CPU (TLS + JSON) and
// its single SQLite connection with cache writes, freezing every other
// page until the sweep finished. The page is already rendered from the
// stale cache at that point, so the sweep may take its time.
const maxParallelFetches = 6

// state is shared between the primary client and its stale views.
type state struct {
	http      *http.Client
	userAgent string
	sso       *sso.Client
	store     *store.Store
	language  atomic.Value // string; "" or "en" = ESI default
	sem       chan struct{} // bounds concurrent network fetches

	mu       sync.Mutex
	cache    map[string]cacheEntry // URL -> cached response body (memory tier)
	names    map[int64]string      // entity id -> name (memory tier)
	inflight map[string]*inflightCall // URL -> network fetch in progress
}

// inflightCall coalesces concurrent fetches of one URL: the sidebar and
// the page ask for the same data, and a navigation mid-revalidation
// starts a second sweep over the same URLs. Only the first caller goes
// to the network; the rest wait for its result.
type inflightCall struct {
	done  chan struct{}
	body  []byte
	pages int
	err   error
}

type cacheEntry struct {
	body    []byte
	pages   int
	expires time.Time
}

type Client struct {
	st *state

	// allowStale: serve expired cache entries without revalidating.
	allowStale bool
	// staleHit is set when an expired (or missing-from-network) entry
	// was served; nil on the primary client.
	staleHit *atomic.Bool
}

func New(ssoClient *sso.Client, st *store.Store, userAgent string) *Client {
	// The default transport keeps only 2 idle connections per host, so a
	// burst of requests to esi.evetech.net over HTTP/1.1 would open (and
	// TLS-handshake) a fresh connection almost every time.
	transport := &http.Transport{
		Proxy:               http.ProxyFromEnvironment,
		ForceAttemptHTTP2:   true,
		MaxIdleConns:        maxParallelFetches + 4,
		MaxIdleConnsPerHost: maxParallelFetches,
		IdleConnTimeout:     90 * time.Second,
	}
	return &Client{st: &state{
		http:      &http.Client{Timeout: 30 * time.Second, Transport: transport},
		userAgent: userAgent,
		sso:       ssoClient,
		store:     st,
		sem:       make(chan struct{}, maxParallelFetches),
		cache:     map[string]cacheEntry{},
		names:     map[int64]string{},
		inflight:  map[string]*inflightCall{},
	}}
}

// SetLanguage switches the language ESI localizes responses to
// (en/de/fr/ja/ru/ko/es/zh). The language is part of every cache key,
// so switching it naturally refetches localized data.
func (c *Client) SetLanguage(lang string) { c.st.language.Store(lang) }

func (c *Client) language() string {
	if v, ok := c.st.language.Load().(string); ok && v != "en" {
		return v
	}
	return ""
}

// StaleView returns a request-scoped client that prefers cached data
// (fresh or expired) and never blocks on the network when a cache entry
// exists. The returned flag reports whether stale data was served.
func (c *Client) StaleView() (*Client, *atomic.Bool) {
	flag := &atomic.Bool{}
	return &Client{st: c.st, allowStale: true, staleHit: flag}, flag
}

func (c *Client) markStale() {
	if c.staleHit != nil {
		c.staleHit.Store(true)
	}
}

// accessToken returns a valid access token for the character,
// refreshing via the SSO when the cached one is about to expire.
func (c *Client) accessToken(characterID int64) (string, error) {
	tok, exp, err := c.st.store.AccessToken(characterID)
	if err != nil {
		return "", fmt.Errorf("no token for character %d: %w", characterID, err)
	}
	if time.Until(exp) > time.Minute {
		return tok, nil
	}

	rt, err := c.st.store.RefreshToken(characterID)
	if err != nil {
		return "", err
	}
	newTok, err := c.st.sso.Refresh(rt)
	if err != nil {
		return "", fmt.Errorf("refresh token for character %d: %w", characterID, err)
	}
	// EVE rotates refresh tokens — always store the returned one.
	exp = time.Now().Add(time.Duration(newTok.ExpiresIn) * time.Second)
	if err := c.st.store.UpdateTokens(characterID, newTok.RefreshToken, newTok.AccessToken, exp); err != nil {
		return "", err
	}
	return newTok.AccessToken, nil
}

// get performs a GET request (authenticated when characterID != 0).
// Cache lookup order: memory → SQLite → network. In stale mode an
// expired entry short-circuits the network entirely.
func (c *Client) get(characterID int64, path string, out any) (int, error) {
	return c.getURL(characterID, baseURL+path, false, out)
}

// getCompat requests a new-style route with the compatibility-date header.
func (c *Client) getCompat(characterID int64, path string, out any) (int, error) {
	return c.getURL(characterID, compatBase+path, true, out)
}

func (c *Client) getURL(characterID int64, url string, compat bool, out any) (int, error) {
	if lang := c.language(); lang != "" {
		if strings.Contains(url, "?") {
			url += "&language=" + lang
		} else {
			url += "?language=" + lang
		}
	}
	now := time.Now()

	// Memory tier.
	c.st.mu.Lock()
	entry, inMem := c.st.cache[url]
	c.st.mu.Unlock()

	// SQLite tier.
	if !inMem {
		if body, pages, expires, ok := c.st.store.CacheGet(url); ok {
			entry = cacheEntry{body: body, pages: pages, expires: expires}
			inMem = true
			c.st.mu.Lock()
			c.st.cache[url] = entry
			c.st.mu.Unlock()
		}
	}

	if inMem {
		if now.Before(entry.expires) {
			return entry.pages, json.Unmarshal(entry.body, out)
		}
		if c.allowStale {
			c.markStale()
			return entry.pages, json.Unmarshal(entry.body, out)
		}
	} else if c.allowStale {
		// Nothing cached at all — a stale view must not block on the
		// network; report stale so the caller triggers a revalidation.
		c.markStale()
		return 0, fmt.Errorf("нет кэша (данные загружаются)")
	}

	// Network — the first caller fetches, concurrent callers of the
	// same URL wait for its result instead of duplicating the request.
	c.st.mu.Lock()
	if call, ok := c.st.inflight[url]; ok {
		c.st.mu.Unlock()
		<-call.done
		if call.err != nil {
			return 0, call.err
		}
		return call.pages, json.Unmarshal(call.body, out)
	}
	call := &inflightCall{done: make(chan struct{})}
	c.st.inflight[url] = call
	c.st.mu.Unlock()

	call.body, call.pages, call.err = c.fetch(characterID, url, compat)

	c.st.mu.Lock()
	delete(c.st.inflight, url)
	c.st.mu.Unlock()
	close(call.done)

	if call.err != nil {
		return 0, call.err
	}
	return call.pages, json.Unmarshal(call.body, out)
}

// fetch performs the actual network request and stores the response in
// both cache tiers. The semaphore is taken around the request itself:
// token refresh happens before (it is the SSO host, not ESI), and the
// caller-side coalescing above already keeps duplicates out.
func (c *Client) fetch(characterID int64, url string, compat bool) ([]byte, int, error) {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("User-Agent", c.st.userAgent)
	req.Header.Set("Accept", "application/json")
	if compat {
		req.Header.Set("X-Compatibility-Date", compatDate)
	}
	if characterID != 0 {
		tok, err := c.accessToken(characterID)
		if err != nil {
			return nil, 0, err
		}
		req.Header.Set("Authorization", "Bearer "+tok)
	}

	c.st.sem <- struct{}{}
	resp, err := c.st.http.Do(req)
	if err != nil {
		<-c.st.sem
		return nil, 0, err
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	resp.Body.Close()
	<-c.st.sem
	if err != nil {
		return nil, 0, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, 0, fmt.Errorf("esi %s: %s: %s", url, resp.Status, truncate(body, 200))
	}

	pages := 1
	fmt.Sscanf(resp.Header.Get("X-Pages"), "%d", &pages)

	expires := time.Now().Add(30 * time.Second)
	if t, err := time.Parse(http.TimeFormat, resp.Header.Get("Expires")); err == nil {
		expires = t
	}
	c.st.mu.Lock()
	c.st.cache[url] = cacheEntry{body: body, pages: pages, expires: expires}
	c.st.mu.Unlock()
	c.st.store.CachePut(url, body, pages, expires)

	return body, pages, nil
}

// post performs an authenticated POST (no caching, e.g. UI actions).
func (c *Client) post(characterID int64, path string, body []byte) error {
	req, err := http.NewRequest("POST", baseURL+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", c.st.userAgent)
	req.Header.Set("Content-Type", "application/json")
	tok, err := c.accessToken(characterID)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+tok)

	resp, err := c.st.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return fmt.Errorf("esi %s: %s: %s", path, resp.Status, truncate(b, 200))
	}
	return nil
}

// call performs an uncached authenticated request of any method and
// returns the HTTP status alongside the error. Fleets are the only live
// part of ESI we touch: the reads are cached for five seconds and half
// the routes are writes, so both cache tiers are bypassed. Callers need
// the status because a 404 there is meaningful ("not in a fleet", "not
// the fleet boss"), not a failure.
func (c *Client) call(method string, characterID int64, path string, in, out any) (int, error) {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return 0, err
		}
		body = bytes.NewReader(b)
	}
	url := baseURL + path
	if lang := c.language(); lang != "" && method == "GET" {
		if strings.Contains(url, "?") {
			url += "&language=" + lang
		} else {
			url += "?language=" + lang
		}
	}

	req, err := http.NewRequest(method, url, body)
	if err != nil {
		return 0, err
	}
	req.Header.Set("User-Agent", c.st.userAgent)
	req.Header.Set("Accept", "application/json")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	tok, err := c.accessToken(characterID)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+tok)

	resp, err := c.st.http.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return resp.StatusCode, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return resp.StatusCode, fmt.Errorf("esi %s %s: %s: %s",
			method, path, resp.Status, truncate(raw, 200))
	}
	if out != nil && len(bytes.TrimSpace(raw)) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return resp.StatusCode, err
		}
	}
	return resp.StatusCode, nil
}

// Names resolves ids (types, systems, characters, ...) to names via
// POST /universe/names/. Names are static, so they cache forever in
// memory and in SQLite.
func (c *Client) Names(ids []int64) map[int64]string {
	out := map[int64]string{}
	var missing []int64

	c.st.mu.Lock()
	for _, id := range ids {
		if n, ok := c.st.names[id]; ok {
			out[id] = n
		} else if id != 0 {
			missing = append(missing, id)
		}
	}
	c.st.mu.Unlock()

	if len(missing) == 0 {
		return out
	}
	missing = dedupe(missing)

	// SQLite tier.
	fromDB := c.st.store.NamesGet(missing)
	if len(fromDB) > 0 {
		c.st.mu.Lock()
		for id, n := range fromDB {
			c.st.names[id] = n
			out[id] = n
		}
		c.st.mu.Unlock()
		var still []int64
		for _, id := range missing {
			if _, ok := fromDB[id]; !ok {
				still = append(still, id)
			}
		}
		missing = still
	}
	if len(missing) == 0 {
		return out
	}

	// Network (batches of 1000 — the endpoint limit).
	resolved := map[int64]string{}
	for start := 0; start < len(missing); start += 1000 {
		end := start + 1000
		if end > len(missing) {
			end = len(missing)
		}
		payload, _ := json.Marshal(missing[start:end])
		req, err := http.NewRequest("POST", baseURL+"/universe/names/", bytes.NewReader(payload))
		if err != nil {
			break
		}
		req.Header.Set("User-Agent", c.st.userAgent)
		req.Header.Set("Content-Type", "application/json")

		c.st.sem <- struct{}{}
		resp, err := c.st.http.Do(req)
		if err != nil {
			<-c.st.sem
			break
		}
		var batch []struct {
			ID   int64  `json:"id"`
			Name string `json:"name"`
		}
		ok := resp.StatusCode == http.StatusOK &&
			json.NewDecoder(resp.Body).Decode(&batch) == nil
		resp.Body.Close()
		<-c.st.sem
		if !ok {
			break
		}
		for _, r := range batch {
			resolved[r.ID] = r.Name
		}
	}

	if len(resolved) > 0 {
		c.st.store.NamesPut(resolved)
		c.st.mu.Lock()
		for id, n := range resolved {
			c.st.names[id] = n
			out[id] = n
		}
		c.st.mu.Unlock()
	}
	return out
}

func dedupe(ids []int64) []int64 {
	seen := map[int64]bool{}
	var out []int64
	for _, id := range ids {
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}

func truncate(b []byte, n int) string {
	if len(b) > n {
		return string(b[:n]) + "..."
	}
	return string(b)
}
