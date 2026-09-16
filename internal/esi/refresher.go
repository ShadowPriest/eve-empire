package esi

// ESI Refresher keeps the cache warm so that page renders never touch
// the network (TASKS.md, «ESI Refresher»).
//
// Every URL the client has ever read is registered here with the
// character whose token fetched it, its Expires and TTL, and when it was
// last read. A single loop picks the entry that is due and dispatches it
// to a small worker pool. There is no FIFO: a FIFO with duplicates is
// exactly the request wave the page-driven revalidation used to cause.
//
// Two tiers. Hot entries belong to a page somebody is looking at right
// now (read by a page view within hotWindow) and are refetched the moment
// their Expires passes — earlier is pointless, CCP serves the same
// snapshot until then. Warm entries are everything else, the sidebar
// included, and are refetched at a per-kind multiple of their TTL: 1×
// means "as soon as it expires", 3× means "every third expiry", 0 means
// "never in the background, only when a page asks". The sidebar is warm
// by design: it is always on screen, so it must never outrank the page.

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Mode selects how much the refresher does on its own.
type Mode int

const (
	// ModeFull keeps every warm entry current in the background.
	ModeFull Mode = iota
	// ModeDemand refreshes hot entries only: pages still load without
	// waiting for the network, but nothing is polled while nobody looks.
	// The dev copy runs like this so two copies do not double the traffic.
	ModeDemand
)

const (
	// refresherWorkers bounds the refresher's share of the global
	// semaphore, leaving slots for on-demand calls (fleet, market depth).
	refresherWorkers = 4
	// hotWindow is how long a page read keeps an entry in the hot tier
	// (until SSE subscriptions replace this approximation).
	hotWindow = 2 * time.Minute
	// warmWindow drops entries nobody has read for this long out of the
	// background rotation; a later read puts them back.
	warmWindow = 72 * time.Hour
	// Failure backoff: 30 s doubling up to 10 minutes.
	backoffMin = 30 * time.Second
	backoffMax = 10 * time.Minute
	// budgetFloor pauses the refresher when ESI reports fewer errors left
	// in the current window than this.
	budgetFloor = 20
	// readFlushEvery persists last_read timestamps in one transaction.
	readFlushEvery = time.Minute
	reportEvery    = 10 * time.Minute
)

// Kind classifies a URL for multipliers and statistics.
type Kind struct {
	Name  string  // stable key (settings, panel)
	Title string  // human label
	Mult  float64 // default multiplier; 0 = on demand only
	// Static data never changes (SDE-style reference routes, mail bodies,
	// contract items): an expired body is as good as a fresh one, so it
	// is never refetched — only a missing entry is fetched, once.
	Static bool
	re     *regexp.Regexp
}

func k(name, title, re string, mult float64) *Kind {
	return &Kind{Name: name, Title: title, Mult: mult, re: regexp.MustCompile(re)}
}

func static(name, title, re string) *Kind {
	return &Kind{Name: name, Title: title, Static: true, re: regexp.MustCompile(re)}
}

// kinds is matched against the path (without /latest and the query) in
// order; the first hit wins.
var kinds = []*Kind{
	k("online", "онлайн", `^/characters/\d+/online/$`, 1),
	k("location", "местоположение и корабль", `^/characters/\d+/(location|ship)/$`, 0),
	k("wallet", "кошелёк", `^/characters/\d+/wallet/$`, 1),
	k("journal", "журнал кошелька", `^/characters/\d+/wallet/journal/$`, 1),
	k("transactions", "сделки", `^/characters/\d+/wallet/transactions/$`, 1),
	k("skills", "навыки", `^/characters/\d+/skills/$`, 3),
	k("skillqueue", "очередь навыков", `^/characters/\d+/skillqueue/$`, 1),
	k("attributes", "атрибуты", `^/characters/\d+/attributes/$`, 3),
	k("implants", "импланты", `^/characters/\d+/implants/$`, 3),
	k("clones", "клоны", `^/characters/\d+/clones/$`, 3),
	k("jobs", "работы", `^/characters/\d+/industry/jobs/$`, 1),
	k("blueprints", "чертежи", `^/characters/\d+/blueprints/$`, 1),
	k("assets", "имущество", `^/characters/\d+/assets/$`, 1),
	k("orders", "ордера", `^/characters/\d+/orders/$`, 1),
	k("orders_history", "история ордеров", `^/characters/\d+/orders/history/$`, 2),
	k("contracts", "контракты", `^/characters/\d+/contracts/$`, 1),
	static("contract_items", "состав контрактов", `^/characters/\d+/contracts/\d+/items/$`),
	k("mail_labels", "почта: метки", `^/characters/\d+/mail/labels/$`, 2),
	k("mail_lists", "почта: рассылки", `^/characters/\d+/mail/lists/$`, 0),
	static("mail_body", "тела писем", `^/characters/\d+/mail/\d+/$`),
	k("mail", "почта", `^/characters/\d+/mail/$`, 2),
	k("notifications", "уведомления", `^/characters/\d+/notifications/$`, 1),
	k("loyalty", "очки лояльности", `^/characters/\d+/loyalty/points/$`, 2),
	k("planets", "колонии", `^/characters/\d+/planets/$`, 1),
	k("planet", "планета", `^/characters/\d+/planets/\d+/$`, 1),
	k("mining", "добыча", `^/characters/\d+/mining/$`, 1),
	k("fleet_ref", "флот персонажа", `^/characters/\d+/fleet/$`, 0),
	k("search", "поиск", `^/characters/\d+/search/$`, 0),
	k("char_public", "карточка персонажа", `^/characters/\d+/$`, 1),
	k("corp_info", "корпорация", `^/corporations/\d+/$`, 1),
	k("corp_jobs", "работы корпорации", `^/corporations/\d+/industry/jobs/$`, 1),
	k("corp_assets", "имущество корпорации", `^/corporations/\d+/assets/$`, 1),
	k("corp_blueprints", "чертежи корпорации", `^/corporations/\d+/blueprints/$`, 1),
	k("corp_contracts", "контракты корпорации", `^/corporations/\d+/contracts/$`, 1),
	static("corp_contract_items", "состав контрактов корпорации", `^/corporations/\d+/contracts/\d+/items/$`),
	k("corp_wallets", "кошельки корпорации", `^/corporations/\d+/wallets/$`, 1),
	k("corp_journal", "журнал корпорации", `^/corporations/\d+/wallets/\d+/journal/$`, 1),
	k("corp_transactions", "сделки корпорации", `^/corporations/\d+/wallets/\d+/transactions/$`, 1),
	k("corp_divisions", "подразделения корпорации", `^/corporations/\d+/divisions/$`, 3),
	k("corp_structures", "структуры корпорации", `^/corporations/\d+/structures/$`, 1),
	k("corp_projects", "проекты корпорации", `^/corporations/\d+/projects`, 1),
	k("market_prices", "цены рынка", `^/markets/prices/$`, 1),
	k("market_orders", "стаканы рынка", `^/markets/\d+/orders/$`, 0),
	k("market_history", "история рынка", `^/markets/\d+/history/$`, 0),
	k("structure_market", "рынок структуры", `^/markets/structures/\d+/$`, 0),
	k("industry_systems", "индексы систем", `^/industry/systems/$`, 1),
	static("sde", "справочники", `^/universe/(types|groups|categories|systems|constellations|regions|stations|planets|moons)/`),
	k("universe", "структуры и поиск", `^/universe/`, 0),
	k("fleet", "флот", `^/fleets/`, 0),
}

// KindOther collects URLs no pattern matches; never refreshed on its own.
var KindOther = &Kind{Name: "other", Title: "прочее", Mult: 0}
var kindOther = KindOther

// Kinds lists every known kind in declaration order (for the panel).
func Kinds() []*Kind { return kinds }

func kindOf(raw string) *Kind {
	u, err := url.Parse(raw)
	if err != nil {
		return kindOther
	}
	path := strings.TrimPrefix(u.Path, "/latest")
	for _, kd := range kinds {
		if kd.re.MatchString(path) {
			return kd
		}
	}
	return kindOther
}

// entry is one registered URL.
type entry struct {
	url    string
	charID int64
	compat bool
	kind   *Kind

	expires time.Time
	ttl     time.Duration // Expires − fetched; 0 when unknown (pre-migration rows)

	lastRead      time.Time
	readPersisted time.Time // last_read value on disk
	hotUntil      time.Time

	lastTry  time.Time
	lastErr  string
	failures int
	inflight bool
}

// KindStats are per-kind counters since process start. Changed counts
// fetches whose body differed from the cache; the rest of the
// successful ones brought the same bytes back.
type KindStats struct {
	Fetches, Errors, Changed int64
	Bytes                    int64
	Elapsed                  time.Duration
	LastFetch                time.Time
}

// Budget is the last error-limit report ESI sent.
type Budget struct {
	Remain, Reset int
	Seen          time.Time
}

// Refresher owns the registry and the dispatch loop. One per Client.
type Refresher struct {
	st     *state
	client *Client // strict client used for network fetches

	mu         sync.Mutex
	mode       Mode
	entries    map[string]*entry
	mult       map[string]float64 // per-kind overrides from settings
	stats      map[string]*KindStats
	pauseUntil time.Time // ESI error budget nearly spent
	pauseWhy   string
	warnedChar map[int64]bool // dead-token complaint logged once per alt
	// watched: URLs some open page depends on (live SSE subscriptions,
	// web/events.go). Hot for as long as the page stays open.
	watched  map[string]struct{}
	onChange func(url string, kind *Kind)

	budgetLast Budget
	recent     []time.Time // fetch times of the last ten minutes (rate)
	started    time.Time

	workers chan struct{}
	wake    chan struct{}
}

func newRefresher(st *state) *Refresher {
	return &Refresher{
		st:         st,
		client:     &Client{st: st},
		entries:    map[string]*entry{},
		mult:       map[string]float64{},
		stats:      map[string]*KindStats{},
		warnedChar: map[int64]bool{},
		watched:    map[string]struct{}{},
		started:    time.Now(),
		workers:    make(chan struct{}, refresherWorkers),
		wake:       make(chan struct{}, 1),
	}
}

// Refresher returns the client's refresher (always present; idle until
// Start is called).
func (c *Client) Refresher() *Refresher { return c.st.reg }

// Start loads the registry from the cache table and runs the loop until
// ctx is cancelled. In-flight fetches finish on their own.
func (r *Refresher) Start(ctx context.Context, mode Mode) {
	r.mu.Lock()
	r.mode = mode
	r.mu.Unlock()
	r.loadSettings()
	r.load()
	go r.loop(ctx)
	name := "full"
	if mode == ModeDemand {
		name = "demand"
	}
	log.Printf("ESI Refresher: режим %s, в реестре ручек — %d", name, r.count())
}

func (r *Refresher) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.entries)
}

// load restores the registry from esi_cache metadata (no bodies). Rows
// written before the metadata columns existed have no fetched time and
// no character: they are skipped and re-registered on their next read.
func (r *Refresher) load() {
	rows := r.st.store.CacheMetas()
	now := time.Now()
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, m := range rows {
		if m.Fetched.IsZero() {
			continue
		}
		if _, ok := r.entries[m.URL]; ok {
			continue
		}
		e := &entry{
			url: m.URL, charID: m.CharID, compat: m.Compat, kind: kindOf(m.URL),
			expires: m.Expires, ttl: m.Expires.Sub(m.Fetched),
			lastRead: m.LastRead, readPersisted: m.LastRead,
		}
		if e.lastRead.IsZero() {
			e.lastRead = now // rows without last_read yet: give them one rotation
		}
		r.entries[m.URL] = e
	}
}

func (r *Refresher) loadSettings() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for key, val := range r.st.store.SettingsPrefix("refresher.mult.") {
		if f, err := strconv.ParseFloat(val, 64); err == nil && f >= 0 {
			r.mult[strings.TrimPrefix(key, "refresher.mult.")] = f
		}
	}
}

// SetMultiplier overrides a kind's multiplier and persists it. 0 = on
// demand only. Values between 0 and 1 are meaningless (Expires is the
// floor) and are clamped to 1.
func (r *Refresher) SetMultiplier(kind string, mult float64) error {
	if mult < 0 {
		mult = 0
	}
	if mult > 0 && mult < 1 {
		mult = 1
	}
	r.mu.Lock()
	r.mult[kind] = mult
	r.mu.Unlock()
	return r.st.store.SetSetting("refresher.mult."+kind, strconv.FormatFloat(mult, 'f', -1, 64))
}

// Multiplier reports the effective multiplier of a kind.
func (r *Refresher) Multiplier(kd *Kind) float64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.multOf(kd)
}

func (r *Refresher) multOf(kd *Kind) float64 {
	if m, ok := r.mult[kd.Name]; ok {
		return m
	}
	return kd.Mult
}

// SetWatched replaces the set of URLs open pages depend on; they are
// hot until replaced again.
func (r *Refresher) SetWatched(urls map[string]struct{}) {
	r.mu.Lock()
	r.watched = urls
	r.mu.Unlock()
	r.kick()
}

// OnChange installs the callback fired when a fetch brings a body that
// differs from the cached one. Called outside the refresher's lock.
func (r *Refresher) OnChange(fn func(url string, kind *Kind)) {
	r.mu.Lock()
	r.onChange = fn
	r.mu.Unlock()
}

func (r *Refresher) notify(url string) {
	r.mu.Lock()
	fn := r.onChange
	kd := kindOther
	if e, ok := r.entries[url]; ok {
		kd = e.kind
	}
	ks := r.stats[kd.Name]
	if ks == nil {
		ks = &KindStats{}
		r.stats[kd.Name] = ks
	}
	ks.Changed++
	r.mu.Unlock()
	if fn != nil {
		fn(url, kd)
	}
}

// touch registers (or updates) an entry on every cache read. hot marks
// the read as coming from a page view: the entry joins the hot tier for
// hotWindow. Sidebar and strict-client reads only refresh lastRead. A
// static kind with a body is never made hot — the body cannot go stale.
// It reports whether the entry is now hot and expired, i.e. a fetch is
// imminent and the page may call itself stale.
func (r *Refresher) touch(url string, charID int64, compat bool, ce cacheEntry, known bool, hot bool) bool {
	now := time.Now()
	r.mu.Lock()
	e, ok := r.entries[url]
	if !ok {
		e = &entry{url: url, kind: kindOf(url)}
		r.entries[url] = e
	}
	if charID != 0 {
		e.charID = charID
	}
	e.compat = compat
	if known {
		e.expires = ce.expires
		if !ce.fetched.IsZero() {
			e.ttl = ce.expires.Sub(ce.fetched)
		}
	}
	e.lastRead = now
	if e.kind.Static && known {
		hot = false
	}
	if hot {
		e.hotUntil = now.Add(hotWindow)
	}
	pending := hot && !now.Before(e.expires)
	r.mu.Unlock()
	if pending {
		r.kick()
	}
	return pending
}

// observe records the outcome of a successful network fetch made by
// anyone (page views, collectors, the refresher itself), so the registry
// always knows the real Expires.
func (r *Refresher) observe(url string, charID int64, compat bool, expires, fetched time.Time) {
	r.mu.Lock()
	e, ok := r.entries[url]
	if !ok {
		e = &entry{url: url, kind: kindOf(url), lastRead: fetched}
		r.entries[url] = e
	}
	if charID != 0 {
		e.charID = charID
	}
	e.compat = compat
	e.expires = expires
	e.ttl = expires.Sub(fetched)
	e.failures = 0
	e.lastErr = ""
	r.mu.Unlock()
}

// failing reports the last error of an entry that is currently backing
// off, so the page shows the error instead of waiting for a fetch that
// will not come. Empty when the entry is healthy or due for a retry.
func (r *Refresher) failing(url string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.entries[url]
	if !ok || e.failures == 0 {
		return ""
	}
	if time.Now().Before(e.lastTry.Add(backoff(e.failures))) {
		return e.kind.Title + ": " + e.lastErr
	}
	return ""
}

// budget consumes the ESI error-limit headers of a response. limited is
// set on a 420 (error limited) answer.
func (r *Refresher) budget(remain, reset int, limited bool) {
	if reset <= 0 {
		reset = 60
	}
	r.mu.Lock()
	r.budgetLast = Budget{Remain: remain, Reset: reset, Seen: time.Now()}
	r.mu.Unlock()
	if !limited && remain >= budgetFloor {
		return
	}
	until := time.Now().Add(time.Duration(reset) * time.Second)
	r.mu.Lock()
	if until.After(r.pauseUntil) {
		r.pauseUntil = until
		r.pauseWhy = fmt.Sprintf("бюджет ошибок ESI: осталось %d, сброс через %d с", remain, reset)
		log.Printf("ESI Refresher: пауза — %s", r.pauseWhy)
	}
	r.mu.Unlock()
}

func backoff(failures int) time.Duration {
	d := backoffMin
	for i := 1; i < failures && d < backoffMax; i++ {
		d *= 2
	}
	if d > backoffMax {
		d = backoffMax
	}
	return d
}

func (r *Refresher) kick() {
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

// schedule computes the tier and the moment an entry becomes due.
// ok=false means the entry is not refreshed in the background at all.
func (r *Refresher) schedule(e *entry, now time.Time) (tier int, due time.Time, ok bool) {
	if e.kind.Static && !e.expires.IsZero() {
		// Has a body: never refetched, however hot the page reading it.
		return 0, time.Time{}, false
	}
	_, watched := r.watched[e.url]
	if watched || e.hotUntil.After(now) {
		tier, due = 0, e.expires
	} else {
		if r.mode == ModeDemand || e.kind.Static {
			return 0, time.Time{}, false
		}
		mult := r.multOf(e.kind)
		if mult <= 0 || now.Sub(e.lastRead) > warmWindow {
			return 0, time.Time{}, false
		}
		tier = 1
		due = e.expires.Add(time.Duration(float64(e.ttl) * (mult - 1)))
	}
	if e.failures > 0 {
		if b := e.lastTry.Add(backoff(e.failures)); b.After(due) {
			due = b
		}
	}
	return tier, due, true
}

// pick chooses the best due entry: hot before warm. Among hot entries
// the most recently read wins (the page being looked at now re-reads
// its routes on every live update; the one visited a minute ago does
// not); among warm ones the longest overdue. A linear scan: a few
// thousand entries four times a second is nothing, and a heap would
// have to be re-keyed on every tier change.
func (r *Refresher) pick(now time.Time) *entry {
	r.mu.Lock()
	defer r.mu.Unlock()
	if now.Before(r.pauseUntil) {
		return nil
	}
	var best *entry
	var bestTier int
	var bestDue time.Time
	better := func(e *entry, tier int, due time.Time) bool {
		if best == nil || tier != bestTier {
			return best == nil || tier < bestTier
		}
		if tier == 0 {
			return e.lastRead.After(best.lastRead)
		}
		return due.Before(bestDue)
	}
	for _, e := range r.entries {
		if e.inflight {
			continue
		}
		tier, due, ok := r.schedule(e, now)
		if !ok || due.After(now) {
			continue
		}
		if better(e, tier, due) {
			best, bestTier, bestDue = e, tier, due
		}
	}
	if best != nil {
		best.inflight = true
	}
	return best
}

func (r *Refresher) loop(ctx context.Context) {
	tick := time.NewTicker(250 * time.Millisecond)
	flush := time.NewTicker(readFlushEvery)
	report := time.NewTicker(reportEvery)
	defer tick.Stop()
	defer flush.Stop()
	defer report.Stop()
	for {
		select {
		case <-ctx.Done():
			r.flushReads()
			return
		case <-r.wake:
			r.dispatch(ctx)
		case <-tick.C:
			r.dispatch(ctx)
		case <-flush.C:
			r.flushReads()
		case <-report.C:
			r.report()
		}
	}
}

// dispatch hands due entries to free workers.
func (r *Refresher) dispatch(ctx context.Context) {
	now := time.Now()
	for {
		select {
		case r.workers <- struct{}{}:
		default:
			return
		}
		e := r.pick(now)
		if e == nil {
			<-r.workers
			return
		}
		go func() {
			defer func() { <-r.workers }()
			r.refresh(ctx, e)
		}()
	}
}

func (r *Refresher) refresh(ctx context.Context, e *entry) {
	defer func() {
		r.mu.Lock()
		e.inflight = false
		r.mu.Unlock()
	}()
	if ctx.Err() != nil {
		return
	}
	// Someone else (a collector, a strict call) may have fetched it
	// meanwhile; observe() already updated expires, so just re-check.
	r.st.mu.Lock()
	ce, ok := r.st.cache[e.url]
	r.st.mu.Unlock()
	if ok && time.Now().Before(ce.expires) {
		return
	}

	started := time.Now()
	body, _, err := r.client.load(e.charID, e.url, e.compat)

	r.mu.Lock()
	ks := r.stats[e.kind.Name]
	if ks == nil {
		ks = &KindStats{}
		r.stats[e.kind.Name] = ks
	}
	ks.Fetches++
	ks.Elapsed += time.Since(started)
	ks.LastFetch = started
	e.lastTry = started
	r.recent = append(r.recent, started)
	if cut := started.Add(-10 * time.Minute); len(r.recent) > 0 && r.recent[0].Before(cut) {
		i := 0
		for i < len(r.recent) && r.recent[i].Before(cut) {
			i++
		}
		r.recent = append([]time.Time(nil), r.recent[i:]...)
	}
	if err != nil {
		ks.Errors++
		e.failures++
		e.lastErr = err.Error()
		if errors.Is(err, ErrNoToken) {
			// A dead token: park every URL of the alt at the maximum
			// backoff (retried every 10 minutes, so a re-login on /reauth
			// picks it up) and complain once per character, not per URL.
			e.failures = 8
			if !r.warnedChar[e.charID] {
				r.warnedChar[e.charID] = true
				log.Printf("ESI Refresher: %v", err)
			}
		} else if e.failures == 1 {
			// Once per streak: the panel (stage 3) will show the rest.
			log.Printf("ESI Refresher: %s — %v", e.kind.Title, err)
		}
	} else {
		ks.Bytes += int64(len(body))
		// observe() ran inside fetch: failures reset, expires updated.
	}
	r.mu.Unlock()
}

// flushReads persists last_read for entries whose on-disk value is more
// than ten minutes behind, in one transaction.
func (r *Refresher) flushReads() {
	reads := map[string]time.Time{}
	r.mu.Lock()
	for _, e := range r.entries {
		if e.lastRead.Sub(e.readPersisted) > 10*time.Minute {
			reads[e.url] = e.lastRead
			e.readPersisted = e.lastRead
		}
	}
	r.mu.Unlock()
	if len(reads) > 0 {
		r.st.store.CacheTouch(reads)
	}
}

// Snapshot is a point-in-time view for logs and the panel.
type Snapshot struct {
	Entries, Hot, Warm, Demand, Failing, Inflight int
	Watched                                       int // URLs open pages depend on
	Due                                           int // ready to fetch right now
	Paused                                        string
	Mode                                          string
	Workers                                       int
	Started                                       time.Time
	PerMin1, PerMin10                             float64 // fetch rate over the last 1 / 10 minutes
	Budget                                        Budget
	Kinds                                         []KindSnapshot
}

// KindSnapshot is one kind's row in the snapshot.
type KindSnapshot struct {
	Kind    *Kind
	Mult    float64
	Entries int
	Stats   KindStats
}

// Snapshot summarises the registry.
func (r *Refresher) Snapshot() Snapshot {
	now := time.Now()
	r.mu.Lock()
	defer r.mu.Unlock()
	var s Snapshot
	perKind := map[string]*KindSnapshot{}
	for _, e := range r.entries {
		s.Entries++
		if e.inflight {
			s.Inflight++
		}
		if e.failures > 0 {
			s.Failing++
		}
		tier, due, ok := r.schedule(e, now)
		switch {
		case !ok:
			s.Demand++
		case tier == 0:
			s.Hot++
		default:
			s.Warm++
		}
		if ok && !due.After(now) && !e.inflight {
			s.Due++
		}
		ksn := perKind[e.kind.Name]
		if ksn == nil {
			ksn = &KindSnapshot{Kind: e.kind, Mult: r.multOf(e.kind)}
			perKind[e.kind.Name] = ksn
		}
		ksn.Entries++
	}
	for name, st := range r.stats {
		if ksn := perKind[name]; ksn != nil {
			ksn.Stats = *st
		}
	}
	for _, ksn := range perKind {
		s.Kinds = append(s.Kinds, *ksn)
	}
	sort.Slice(s.Kinds, func(i, j int) bool { return s.Kinds[i].Kind.Name < s.Kinds[j].Kind.Name })
	if now.Before(r.pauseUntil) {
		s.Paused = r.pauseWhy
	}
	s.Watched = len(r.watched)
	s.Mode = "full"
	if r.mode == ModeDemand {
		s.Mode = "demand"
	}
	s.Workers = refresherWorkers
	s.Started = r.started
	s.Budget = r.budgetLast
	var n1, n10 int
	for _, t := range r.recent {
		if d := now.Sub(t); d <= time.Minute {
			n1++
			n10++
		} else if d <= 10*time.Minute {
			n10++
		}
	}
	s.PerMin1 = float64(n1)
	window := now.Sub(r.started)
	if window > 10*time.Minute {
		window = 10 * time.Minute
	}
	if window >= time.Minute {
		s.PerMin10 = float64(n10) / window.Minutes()
	}
	return s
}

// Boost is the "refresh now" button of a page: each of its routes that
// is past Expires jumps to the head of the queue (hot and most recently
// read), backoff cleared. Routes still inside their Expires cannot be
// refreshed earlier — CCP serves the same snapshot until then — so the
// soonest of them is reported as next.
func (r *Refresher) Boost(urls []string) (boosted int, next time.Duration) {
	now := time.Now()
	r.mu.Lock()
	for _, u := range urls {
		e, ok := r.entries[u]
		if !ok || (e.kind.Static && !e.expires.IsZero()) {
			continue
		}
		if now.Before(e.expires) {
			if d := e.expires.Sub(now); next == 0 || d < next {
				next = d
			}
			continue
		}
		e.hotUntil = now.Add(hotWindow)
		e.lastRead = now
		e.failures = 0
		boosted++
	}
	r.mu.Unlock()
	if boosted > 0 {
		r.kick()
	}
	return boosted, next
}

// EntryInfo is one registry row for the panel and /api/refresher.
type EntryInfo struct {
	URL      string    `json:"url"`
	Kind     string    `json:"kind"`
	Title    string    `json:"title"`
	CharID   int64     `json:"char_id"`
	Tier     string    `json:"tier"` // hot / warm / demand
	Expires  time.Time `json:"expires"`
	TTL      float64   `json:"ttl_s"`
	Due      time.Time `json:"due,omitempty"`
	LastRead time.Time `json:"last_read"`
	Failures int       `json:"failures,omitempty"`
	LastErr  string    `json:"last_err,omitempty"`
	Inflight bool      `json:"inflight,omitempty"`
}

// Entries lists registry rows; filter is "hot", "failing", "due" or ""
// (everything). Sorted by tier, then due.
func (r *Refresher) Entries(filter string) []EntryInfo {
	now := time.Now()
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []EntryInfo
	for _, e := range r.entries {
		tier, due, ok := r.schedule(e, now)
		name := "demand"
		if ok {
			name = []string{"hot", "warm"}[tier]
		}
		switch filter {
		case "hot":
			if name != "hot" {
				continue
			}
		case "failing":
			if e.failures == 0 {
				continue
			}
		case "due":
			if !ok || due.After(now) {
				continue
			}
		}
		info := EntryInfo{
			URL: e.url, Kind: e.kind.Name, Title: e.kind.Title, CharID: e.charID, Tier: name,
			Expires: e.expires, TTL: e.ttl.Seconds(), LastRead: e.lastRead,
			Failures: e.failures, LastErr: e.lastErr, Inflight: e.inflight,
		}
		if ok {
			info.Due = due
		}
		out = append(out, info)
	}
	rank := map[string]int{"hot": 0, "warm": 1, "demand": 2}
	sort.Slice(out, func(i, j int) bool {
		if rank[out[i].Tier] != rank[out[j].Tier] {
			return rank[out[i].Tier] < rank[out[j].Tier]
		}
		return out[i].Due.Before(out[j].Due)
	})
	return out
}

// report writes a summary line so the router log shows what the
// refresher costs before the panel (stage 3) exists.
func (r *Refresher) report() {
	s := r.Snapshot()
	var fetches, errs, bytes int64
	for _, ksn := range s.Kinds {
		fetches += ksn.Stats.Fetches
		errs += ksn.Stats.Errors
		bytes += ksn.Stats.Bytes
	}
	msg := fmt.Sprintf("ESI Refresher: ручек %d (горячих %d, тёплых %d, по запросу %d, с ошибками %d), с запуска запросов %d, ошибок %d, %.1f МБ",
		s.Entries, s.Hot, s.Warm, s.Demand, s.Failing, fetches, errs, float64(bytes)/1e6)
	if s.Paused != "" {
		msg += "; пауза: " + s.Paused
	}
	log.Print(msg)
}
