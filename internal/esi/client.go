// Package esi is a minimal client for the EVE Swagger Interface.
// It transparently refreshes access tokens, caches responses in memory
// and in SQLite (honouring the Expires header) and batch-resolves names.
//
// A Client can produce a StaleView: a request-scoped variant that serves
// cache entries (fresh or expired) and never touches the network. Every
// read registers its URL with the ESI Refresher (refresher.go), which
// keeps the cache warm in the background; the view only records whether
// something stale was served so the page can poll for the refreshed
// render.
package esi

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
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
	language  atomic.Value  // string; "" or "en" = ESI default
	sem       chan struct{} // bounds concurrent network fetches

	mu       sync.Mutex
	cache    map[string]cacheEntry    // URL -> cached response body (memory tier)
	names    map[int64]string         // entity id -> name (memory tier)
	inflight map[string]*inflightCall // URL -> network fetch in progress
	// deadTokens remembers characters whose refresh token the SSO just
	// rejected (invalid_grant: issued by the other copy's application,
	// or revoked). Every URL of such an alt would otherwise hit the SSO
	// separately; one failure per character per deadTokenFor is enough.
	deadTokens map[int64]time.Time

	reg *Refresher // registry of every URL read; keeps the cache warm
}

// deadTokenFor is how long a rejected refresh token is not retried.
const deadTokenFor = 10 * time.Minute

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
	fetched time.Time // when the body was received; zero for pre-migration rows
}

// ErrNoToken marks a character this copy holds no token for.
var ErrNoToken = errors.New("нет токена")

type Client struct {
	st *state

	// allowStale: serve cache entries (fresh or expired) and never hit
	// the network; reads register with the refresher instead.
	allowStale bool
	// background: a stale view whose reads must not make entries hot
	// (the sidebar) and whose refresh errors are not the page's problem.
	background bool
	// view collects what happened during one request; nil on the
	// primary client.
	view *ViewStatus

	// lang переопределяет язык ESI для одного request-scoped view:
	// язык — часть URL (?language=ru), а значит и часть ключа кэша,
	// поэтому у каждого кабинета он может быть свой, не мешая ни кэшу,
	// ни фоновому обновлению (URL самоописателен).
	// langSet отличает «кабинет выбрал en» от «переопределения нет».
	lang    string
	langSet bool
}

// ViewStatus is what a request-scoped view reports back to the page.
type ViewStatus struct {
	// Page is the request URI the render belongs to; the web layer sets
	// it so the render can be recorded as that page's dependency set.
	Page string
	// UserID — кабинет, которому принадлежит рендер: зависимости страниц
	// в SSE-хабе ключуются парой (кабинет, страница), иначе чужой рендер
	// того же пути подменял бы подписку.
	UserID int64

	stale atomic.Bool
	mu    sync.Mutex
	errs  []string
	urls  map[string]bool // URL -> read by the page itself (true) or a background view (false)
}

// Deps lists every URL read during the request: true for the page's own
// reads, false for background (sidebar) ones.
func (v *ViewStatus) Deps() map[string]bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	out := make(map[string]bool, len(v.urls))
	for u, own := range v.urls {
		out[u] = own
	}
	return out
}

// Stale reports whether an expired or missing entry was served and the
// refresher has been asked for it, i.e. a re-render will be fresher.
func (v *ViewStatus) Stale() bool { return v.stale.Load() }

// Errors lists refresh failures behind the data served (an entry that is
// backing off after an ESI error); they replace the page's own network
// errors of the old strict path.
func (v *ViewStatus) Errors() []string {
	v.mu.Lock()
	defer v.mu.Unlock()
	return append([]string(nil), v.errs...)
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
	s := &state{
		http:       &http.Client{Timeout: 30 * time.Second, Transport: transport},
		userAgent:  userAgent,
		sso:        ssoClient,
		store:      st,
		sem:        make(chan struct{}, maxParallelFetches),
		cache:      map[string]cacheEntry{},
		names:      map[int64]string{},
		inflight:   map[string]*inflightCall{},
		deadTokens: map[int64]time.Time{},
	}
	s.reg = newRefresher(s)
	return &Client{st: s}
}

// SetLanguage switches the language ESI localizes responses to
// (en/de/fr/ja/ru/ko/es/zh). The language is part of every cache key,
// so switching it naturally refetches localized data.
func (c *Client) SetLanguage(lang string) { c.st.language.Store(lang) }

// WithLanguage возвращает копию клиента с языком кабинета. Применяется
// к request-scoped view (esiFor в web): язык попадает в URL, поэтому
// разные кабинеты просто читают разные ключи кэша, а фоновое обновление
// работает с любым URL как есть.
func (c *Client) WithLanguage(lang string) *Client {
	n := *c
	n.lang, n.langSet = lang, true
	return &n
}

func (c *Client) language() string {
	if c.langSet {
		if c.lang != "en" {
			return c.lang
		}
		return ""
	}
	if v, ok := c.st.language.Load().(string); ok && v != "en" {
		return v
	}
	return ""
}

// StaleView returns a request-scoped client that serves cached data
// (fresh or expired) and never blocks on the network. Its reads mark
// entries hot for the refresher. The status reports what was served.
func (c *Client) StaleView() (*Client, *ViewStatus) {
	v := &ViewStatus{}
	return &Client{st: c.st, allowStale: true, view: v}, v
}

// Background derives a view for data that is always on screen (the
// sidebar): same cache, same stale flag, but its reads stay in the warm
// tier and its refresh errors are not reported on the page.
func (c *Client) Background() *Client {
	if !c.allowStale {
		return c
	}
	n := *c
	n.background = true
	return &n
}

func (c *Client) markStale() {
	if c.view != nil {
		c.view.stale.Store(true)
	}
}

func (c *Client) noteRead(url string) {
	if c.view == nil {
		return
	}
	c.view.mu.Lock()
	if c.view.urls == nil {
		c.view.urls = map[string]bool{}
	}
	if !c.background {
		c.view.urls[url] = true
	} else if _, ok := c.view.urls[url]; !ok {
		c.view.urls[url] = false
	}
	c.view.mu.Unlock()
}

func (c *Client) noteErr(msg string) {
	if c.view == nil || c.background {
		return
	}
	c.view.mu.Lock()
	c.view.errs = append(c.view.errs, msg)
	c.view.mu.Unlock()
}

// accessToken returns a valid access token for the character,
// refreshing via the SSO when the cached one is about to expire.
func (c *Client) accessToken(characterID int64) (string, error) {
	tok, exp, err := c.st.store.AccessToken(characterID)
	if err != nil {
		return "", fmt.Errorf("%w для персонажа %d: %v", ErrNoToken, characterID, err)
	}
	if time.Until(exp) > time.Minute {
		return tok, nil
	}

	c.st.mu.Lock()
	dead := c.st.deadTokens[characterID]
	c.st.mu.Unlock()
	if time.Now().Before(dead) {
		return "", fmt.Errorf("%w: персонаж %d — токен отклонён SSO, нужен перелогин на /reauth", ErrNoToken, characterID)
	}

	rt, err := c.st.store.RefreshToken(characterID)
	if err != nil {
		return "", err
	}
	newTok, err := c.st.sso.Refresh(rt)
	if err != nil {
		// Usually invalid_grant: a token issued by the other copy's
		// application (see reauth.go). Remembered per character so the
		// other URLs of this alt fail fast instead of each asking the
		// SSO again; retried after deadTokenFor in case of a re-login.
		c.st.mu.Lock()
		c.st.deadTokens[characterID] = time.Now().Add(deadTokenFor)
		c.st.mu.Unlock()
		return "", fmt.Errorf("%w: обновление токена персонажа %d: %v", ErrNoToken, characterID, err)
	}
	c.st.mu.Lock()
	delete(c.st.deadTokens, characterID)
	c.st.mu.Unlock()
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
	// Дальше по URL ходит только сеть: кэш, реестр обновления и
	// склейка одновременных чтений работают по ключу (cacheKey).
	key := cacheKey(characterID, url)
	now := time.Now()

	// Memory tier.
	c.st.mu.Lock()
	entry, inMem := c.st.cache[key]
	c.st.mu.Unlock()

	// SQLite tier.
	if !inMem {
		if body, meta, ok := c.st.store.CacheGet(key); ok {
			entry = cacheEntry{body: body, pages: meta.Pages, expires: meta.Expires, fetched: meta.Fetched}
			inMem = true
			c.st.mu.Lock()
			c.st.cache[key] = entry
			c.st.mu.Unlock()
		}
	}

	// Every read registers with the refresher: a page view makes the
	// entry hot, everything else just keeps it warm. pending: a fetch is
	// imminent, so the page may call itself stale and poll for it.
	pending := c.st.reg.touch(key, characterID, compat, entry, inMem, c.allowStale && !c.background)
	c.noteRead(key)

	if inMem {
		if now.Before(entry.expires) {
			return entry.pages, json.Unmarshal(entry.body, out)
		}
		if c.allowStale {
			// Expired: if the refresher is backing off after an error, say
			// so instead of promising a fresher render. A background
			// (sidebar) read or a static kind is not marked stale: the
			// entry is warm (may legitimately wait several TTLs) or never
			// refetched, so a poll would only churn.
			if msg := c.st.reg.failing(key); msg != "" {
				c.noteErr(msg)
			} else if pending {
				c.markStale()
			}
			return entry.pages, json.Unmarshal(entry.body, out)
		}
	} else if c.allowStale {
		// Nothing cached at all: a stale view must not block on the
		// network. Report stale so the page polls for the refreshed render.
		if msg := c.st.reg.failing(key); msg != "" {
			return 0, fmt.Errorf("%s", msg)
		}
		c.markStale()
		return 0, fmt.Errorf("нет кэша (данные загружаются)")
	}

	body, pages, err := c.load(characterID, key, compat)
	if err != nil {
		return 0, err
	}
	return pages, json.Unmarshal(body, out)
}

// ── ключ кэша ────────────────────────────────────────────────────────

// corpKeyMark — хвост ключа кэша с персонажем: «…#c=<id>».
//
// Ответ корпоративной ручки зависит не только от URL, но и от ролей
// персонажа, чьим токеном её спросили: у одного Director, у другого нет
// доступа вовсе. Пока ключом был голый URL, кабинет без роли получал из
// `esi_cache` кошельки корпорации, добытые чужим токеном. Персонаж
// попадает в ключ только там, где это правда нужно (`/corporations/`):
// в остальных авторизованных URL он и так стоит в пути.
const corpKeyMark = "#c="

// cacheKey — под каким ключом эта пара (персонаж, URL) живёт в кэше,
// реестре обновления и склейке одновременных чтений.
func cacheKey(characterID int64, rawURL string) string {
	if characterID == 0 || !strings.Contains(rawURL, "/corporations/") {
		return rawURL
	}
	return rawURL + corpKeyMark + strconv.FormatInt(characterID, 10)
}

// keyURL снимает с ключа хвост персонажа: в сеть идёт настоящий адрес.
// Так же ESI Refresher рефетчит свои записи — ключ у него в `entry.url`,
// персонаж рядом, в `entry.charID`.
func keyURL(key string) string {
	if i := strings.Index(key, corpKeyMark); i >= 0 {
		return key[:i]
	}
	return key
}

// load performs the network fetch with in-flight coalescing: the first
// caller fetches, concurrent callers of the same key wait for its result
// instead of duplicating the request.
func (c *Client) load(characterID int64, key string, compat bool) ([]byte, int, error) {
	c.st.mu.Lock()
	if call, ok := c.st.inflight[key]; ok {
		c.st.mu.Unlock()
		<-call.done
		return call.body, call.pages, call.err
	}
	call := &inflightCall{done: make(chan struct{})}
	c.st.inflight[key] = call
	c.st.mu.Unlock()

	call.body, call.pages, call.err = c.fetch(characterID, key, compat)

	c.st.mu.Lock()
	delete(c.st.inflight, key)
	c.st.mu.Unlock()
	close(call.done)
	return call.body, call.pages, call.err
}

// fetch performs the actual network request and stores the response in
// both cache tiers. The semaphore is taken around the request itself:
// token refresh happens before (it is the SSO host, not ESI), and the
// caller-side coalescing above already keeps duplicates out.
func (c *Client) fetch(characterID int64, key string, compat bool) ([]byte, int, error) {
	url := keyURL(key)
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
	remain, reset := errorBudget(resp)
	c.st.reg.budget(remain, reset, resp.StatusCode == 420)
	if resp.StatusCode != http.StatusOK {
		return nil, 0, fmt.Errorf("esi %s: %s: %s", url, resp.Status, truncate(body, 200))
	}

	pages := 1
	fmt.Sscanf(resp.Header.Get("X-Pages"), "%d", &pages)

	fetched := time.Now()
	expires := fetched.Add(30 * time.Second)
	if t, err := time.Parse(http.TimeFormat, resp.Header.Get("Expires")); err == nil {
		expires = t
	}
	c.st.mu.Lock()
	old, had := c.st.cache[key]
	c.st.cache[key] = cacheEntry{body: body, pages: pages, expires: expires, fetched: fetched}
	c.st.mu.Unlock()
	// Most refreshes bring back the same bytes (a sleeping alt's wallet
	// does not move): then only the deadline is renewed — no blob write,
	// no page event.
	changed := !had || !bytes.Equal(old.body, body)
	if changed {
		c.st.store.CachePut(key, body, store.CacheMeta{
			CharID: characterID, Compat: compat, Pages: pages, Expires: expires, Fetched: fetched,
		})
	} else {
		c.st.store.CacheRenew(key, expires, fetched)
	}
	c.st.reg.observe(key, characterID, compat, expires, fetched)
	if changed {
		c.st.reg.notify(key)
	}

	return body, pages, nil
}

// errorBudget reads the ESI error-limit headers (errors left in the
// current window and seconds until it resets). Missing headers read as
// a full budget.
func errorBudget(resp *http.Response) (remain, reset int) {
	remain = 100
	fmt.Sscanf(resp.Header.Get("X-ESI-Error-Limit-Remain"), "%d", &remain)
	fmt.Sscanf(resp.Header.Get("X-ESI-Error-Limit-Reset"), "%d", &reset)
	return remain, reset
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
