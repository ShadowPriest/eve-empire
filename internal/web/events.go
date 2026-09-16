package web

// Live page updates over Server-Sent Events (TASKS.md, «ESI Refresher»,
// stage 2).
//
// The connection is the subscription: while a browser holds
// GET /events?page=<path+query>, the ESI routes that page read at its
// last render are "watched" — the refresher's hot tier. The server
// learns those routes by itself: the request-scoped ESI view records
// every URL a render touched (sidebar reads included, flagged as such).
// When the refresher fetches a body that actually differs from the
// cached one, every subscriber whose page depends on that URL gets one
// event (changes are batched for a moment first). The browser then
// re-requests the page — rendered from the warm cache in milliseconds —
// and swaps only the blocks whose HTML changed.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"eve-empire/internal/esi"
)

const (
	// eventDebounce batches changes: thirty alts' online flags landing
	// within a second become one event, one re-render.
	eventDebounce = 1500 * time.Millisecond
	// eventPing keeps idle connections alive through proxies and the
	// browser's own idle timeouts.
	eventPing = 20 * time.Second
	// maxPageDeps bounds the dependency map; pages nobody watches are
	// dropped first (they are re-recorded on their next render).
	maxPageDeps = 300
)

// depKey — ключ зависимостей страницы. Путь один и тот же у всех
// кабинетов (`/planets`, `/`), а читает каждый кабинет свои URL, поэтому
// ключ — пара (кабинет, страница): иначе чужой рендер подменял бы
// подписку и присылал события о чужих данных.
type depKey struct {
	user int64
	page string
}

type hub struct {
	reg *esi.Refresher

	mu sync.Mutex
	// deps: page key -> URL -> read by the page itself (true) or only by
	// the sidebar (false). Sidebar URLs still trigger events (the
	// sidebar must update live) but never become hot through a page.
	deps map[depKey]map[string]bool
	subs map[*subscriber]struct{}
}

type subscriber struct {
	page depKey
	ch   chan []byte

	mu    sync.Mutex
	kinds map[string]bool // kind titles changed since the last event
	timer *time.Timer
}

func newHub(reg *esi.Refresher) *hub {
	h := &hub{reg: reg, deps: map[depKey]map[string]bool{}, subs: map[*subscriber]struct{}{}}
	reg.OnChange(h.changed)
	return h
}

// events returns the hub, created on first use.
func (s *Server) events() *hub {
	s.hubOnce.Do(func() { s.hub = newHub(s.ESI.Refresher()) })
	return s.hub
}

// setDeps records what a page's last render read.
func (h *hub) setDeps(page depKey, deps map[string]bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, known := h.deps[page]; !known && len(h.deps) >= maxPageDeps {
		live := map[depKey]bool{}
		for s := range h.subs {
			live[s.page] = true
		}
		for p := range h.deps {
			if !live[p] {
				delete(h.deps, p)
			}
		}
	}
	h.deps[page] = deps
	for s := range h.subs {
		if s.page == page {
			h.rewatch()
			break
		}
	}
}

func (h *hub) subscribe(page depKey) *subscriber {
	s := &subscriber{page: page, ch: make(chan []byte, 4), kinds: map[string]bool{}}
	h.mu.Lock()
	h.subs[s] = struct{}{}
	h.rewatch()
	h.mu.Unlock()
	return s
}

func (h *hub) unsubscribe(s *subscriber) {
	h.mu.Lock()
	delete(h.subs, s)
	h.rewatch()
	h.mu.Unlock()
	s.mu.Lock()
	if s.timer != nil {
		s.timer.Stop()
	}
	s.mu.Unlock()
}

// rewatch hands the refresher the union of page-level URLs of every
// subscribed page. Called with h.mu held; the refresher never calls
// back into the hub while holding its own lock, so the order is safe.
func (h *hub) rewatch() {
	set := map[string]struct{}{}
	for s := range h.subs {
		for url, own := range h.deps[s.page] {
			if own {
				set[url] = struct{}{}
			}
		}
	}
	h.reg.SetWatched(set)
}

// depsOf lists a page's own routes (sidebar reads excluded) as last
// rendered — what "refresh now" may boost.
func (h *hub) depsOf(page depKey) []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []string
	for url, own := range h.deps[page] {
		if own {
			out = append(out, url)
		}
	}
	return out
}

// changed is the refresher's callback: a fetched body differs from the
// cached one.
func (h *hub) changed(url string, kind *esi.Kind) {
	h.mu.Lock()
	var hit []*subscriber
	for s := range h.subs {
		if _, ok := h.deps[s.page][url]; ok {
			hit = append(hit, s)
		}
	}
	h.mu.Unlock()
	for _, s := range hit {
		s.mark(kind.Title)
	}
}

func (s *subscriber) mark(title string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.kinds[title] = true
	if s.timer == nil {
		s.timer = time.AfterFunc(eventDebounce, s.flush)
	}
}

func (s *subscriber) flush() {
	s.mu.Lock()
	kinds := make([]string, 0, len(s.kinds))
	for k := range s.kinds {
		kinds = append(kinds, k)
	}
	s.kinds = map[string]bool{}
	s.timer = nil
	s.mu.Unlock()
	sort.Strings(kinds)
	msg, _ := json.Marshal(map[string]any{"kinds": kinds, "at": time.Now().Format("15:04:05")})
	select {
	case s.ch <- msg:
	default: // the browser is behind; it re-renders everything anyway
	}
}

// handleEvents streams change notifications for one page. The page key
// is what the browser sees as location.pathname + location.search, which
// is also the request URI the render recorded its dependencies under.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	page := r.URL.Query().Get("page")
	if !strings.HasPrefix(page, "/") {
		http.Error(w, "bad page", http.StatusBadRequest)
		return
	}
	user := userFrom(r)
	if user == nil {
		http.Error(w, "нет сессии", http.StatusUnauthorized)
		return
	}
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	hub := s.events()
	sub := hub.subscribe(depKey{user.ID, page})
	defer hub.unsubscribe(sub)

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, "retry: 3000\n\n")
	fl.Flush()

	ping := time.NewTicker(eventPing)
	defer ping.Stop()
	for {
		select {
		case msg := <-sub.ch:
			fmt.Fprintf(w, "data: %s\n\n", msg)
			fl.Flush()
		case <-ping.C:
			fmt.Fprint(w, ": ping\n\n")
			fl.Flush()
		case <-r.Context().Done():
			return
		}
	}
}
