package web

// The ESI Refresher panel (TASKS.md, «ESI Refresher», stage 3): what is
// being fetched, how often, with what result — and the knobs: a
// multiplier per route kind, and "refresh now" for the page on screen.

import (
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"eve-empire/internal/collect"
	"eve-empire/internal/esi"
)

type refKindRow struct {
	Kind         *esi.Kind
	Mult         float64
	Entries      int
	TTL          string // typical ESI cache time of this kind
	Period       string // TTL × multiplier; "по запросу" / "статично"
	Fetches      int64
	Changed      int64
	Unchanged    int64
	UnchangedPct int
	Errors       int64
	AvgMs        int64
	LastFetch    string
	NextDue      string
	Failing      int
	Rows         []refEntryRow
}

type refEntryRow struct {
	Who      string
	Path     string
	Tier     string
	Expires  string
	Due      string
	Failures int
	LastErr  string
	Inflight bool
}

type refTaskRow struct {
	Task    string
	Every   string
	LastTry string
	LastOK  string
	Note    string
	Stale   bool // last success older than two intervals
}

// Панель ESI Refresher показывает работу всего инстанса (все
// персонажи, общий кэш, общие множители), поэтому она админская. Не-админ
// получает 404: существование служебной страницы ему знать незачем.
func (s *Server) handleRefresherPage(w http.ResponseWriter, r *http.Request) {
	if !isAdmin(r) {
		http.NotFound(w, r)
		return
	}
	ec, view := s.esiFor(r)
	data, _, err := s.shell(r, ec, 0, "")
	if err != nil {
		httpError(w, "loading characters", err)
		return
	}
	reg := s.ESI.Refresher()
	now := time.Now()
	snap := reg.Snapshot()

	names := map[int64]string{}
	if chars, err := s.Store.AllCharacters(); err == nil {
		for _, ch := range chars {
			names[ch.ID] = ch.Name
		}
	}

	// Group registry rows by kind.
	byKind := map[string][]esi.EntryInfo{}
	for _, e := range reg.Entries("") {
		byKind[e.Kind] = append(byKind[e.Kind], e)
	}
	statsByKind := map[string]esi.KindSnapshot{}
	for _, ks := range snap.Kinds {
		statsByKind[ks.Kind.Name] = ks
	}

	var rows []refKindRow
	kinds := append([]*esi.Kind{}, esi.Kinds()...)
	kinds = append(kinds, esi.KindOther)
	for _, kd := range kinds {
		list := byKind[kd.Name]
		ks := statsByKind[kd.Name]
		row := refKindRow{Kind: kd, Mult: reg.Multiplier(kd), Entries: len(list)}
		row.Fetches, row.Changed, row.Errors = ks.Stats.Fetches, ks.Stats.Changed, ks.Stats.Errors
		row.Unchanged = row.Fetches - row.Changed - row.Errors
		if row.Unchanged < 0 {
			row.Unchanged = 0
		}
		if row.Fetches > 0 {
			row.UnchangedPct = int(100 * row.Unchanged / row.Fetches)
			row.AvgMs = ks.Stats.Elapsed.Milliseconds() / row.Fetches
		}
		if !ks.Stats.LastFetch.IsZero() {
			row.LastFetch = humanAgo(ks.Stats.LastFetch, now)
		}
		ttl := typicalTTL(list)
		if ttl > 0 {
			row.TTL = humanSpan(ttl)
		}
		switch {
		case kd.Static:
			row.Period = "статично"
		case row.Mult <= 0:
			row.Period = "по запросу"
		case ttl > 0:
			row.Period = humanSpan(time.Duration(float64(ttl) * row.Mult))
		}
		var next time.Time
		for _, e := range list {
			if e.Failures > 0 {
				row.Failing++
			}
			if !e.Due.IsZero() && (next.IsZero() || e.Due.Before(next)) {
				next = e.Due
			}
			if kd.Static {
				continue
			}
			er := refEntryRow{
				Who: names[e.CharID], Path: shortPath(e.URL), Tier: e.Tier,
				Failures: e.Failures, LastErr: e.LastErr, Inflight: e.Inflight,
			}
			if er.Who == "" && e.CharID != 0 {
				er.Who = strconv.FormatInt(e.CharID, 10)
			}
			if !e.Expires.IsZero() {
				er.Expires = humanIn(e.Expires, now)
			}
			if !e.Due.IsZero() {
				er.Due = humanIn(e.Due, now)
			}
			row.Rows = append(row.Rows, er)
		}
		if !next.IsZero() {
			row.NextDue = humanIn(next, now)
		}
		sort.Slice(row.Rows, func(i, j int) bool {
			if row.Rows[i].Who != row.Rows[j].Who {
				return row.Rows[i].Who < row.Rows[j].Who
			}
			return row.Rows[i].Path < row.Rows[j].Path
		})
		rows = append(rows, row)
	}

	// Background collectors: intervals from the task list, outcomes from
	// collector_run (the "панель фоновых задач" the accounting section
	// asked for).
	every := map[string]time.Duration{}
	for _, t := range collect.New(nil, nil, "").Tasks() {
		every[t.Name] = t.Every
	}
	var tasks []refTaskRow
	if statuses, err := s.Store.CollectorStatuses(); err == nil {
		for _, st := range statuses {
			tr := refTaskRow{Task: st.Task, Note: st.Note}
			if d := every[st.Task]; d > 0 {
				tr.Every = humanSpan(d)
				tr.Stale = !st.LastOK.IsZero() && now.Sub(st.LastOK) > 2*d
			}
			if !st.LastTry.IsZero() {
				tr.LastTry = humanAgo(st.LastTry, now)
			}
			if !st.LastOK.IsZero() {
				tr.LastOK = humanAgo(st.LastOK, now)
			}
			tasks = append(tasks, tr)
		}
	}

	data["Snap"] = snap
	data["Kinds"] = rows
	data["Tasks"] = tasks
	data["Uptime"] = humanSpan(now.Sub(snap.Started).Round(time.Second))
	data["BudgetSeen"] = ""
	if !snap.Budget.Seen.IsZero() {
		data["BudgetSeen"] = humanAgo(snap.Budget.Seen, now)
	}
	s.render(w, "refresher", data, view)
}

// handleRefresherMult stores a kind's multiplier: form fields kind, mult.
func (s *Server) handleRefresherMult(w http.ResponseWriter, r *http.Request) {
	if !isAdmin(r) {
		http.NotFound(w, r)
		return
	}
	kind := r.FormValue("kind")
	mult, err := strconv.ParseFloat(strings.TrimSpace(r.FormValue("mult")), 64)
	if err != nil || mult < 0 || mult > 1000 {
		writeJSON(w, map[string]any{"error": "множитель — число от 0 (только по запросу) до 1000"})
		return
	}
	known := false
	for _, kd := range esi.Kinds() {
		if kd.Name == kind {
			known = !kd.Static
		}
	}
	if kind == esi.KindOther.Name {
		known = true
	}
	if !known {
		writeJSON(w, map[string]any{"error": "неизвестный вид ручки"})
		return
	}
	if err := s.ESI.Refresher().SetMultiplier(kind, mult); err != nil {
		writeJSON(w, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, map[string]any{"ok": true, "mult": s.ESI.Refresher().Multiplier(kindByName(kind))})
}

// handleRefresherNow is the "refresh now" button of a page: its expired
// routes jump to the head of the queue. Routes still inside their Expires
// cannot be refreshed earlier (CCP serves the same snapshot), so the
// answer says how soon the next one can be.
func (s *Server) handleRefresherNow(w http.ResponseWriter, r *http.Request) {
	if !isAdmin(r) {
		http.NotFound(w, r)
		return
	}
	page := r.FormValue("page")
	if !strings.HasPrefix(page, "/") {
		http.Error(w, "bad page", http.StatusBadRequest)
		return
	}
	urls := s.events().depsOf(depKey{userFrom(r).ID, page})
	boosted, next := s.ESI.Refresher().Boost(urls)
	writeJSON(w, map[string]any{
		"boosted": boosted,
		"routes":  len(urls),
		"next_s":  int(next.Round(time.Second).Seconds()),
	})
}

func kindByName(name string) *esi.Kind {
	for _, kd := range esi.Kinds() {
		if kd.Name == name {
			return kd
		}
	}
	return esi.KindOther
}

// typicalTTL is the most common TTL among the entries (ESI's cache time
// for the kind; individual rows may differ while the migration settles).
func typicalTTL(list []esi.EntryInfo) time.Duration {
	count := map[int64]int{}
	var best int64
	for _, e := range list {
		sec := int64(e.TTL)
		if sec <= 0 {
			continue
		}
		count[sec]++
		if count[sec] > count[best] {
			best = sec
		}
	}
	return time.Duration(best) * time.Second
}

// shortPath strips the host, the /latest prefix and the language
// parameter from a cache URL.
func shortPath(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	p := strings.TrimPrefix(u.Path, "/latest")
	q := u.Query()
	q.Del("language")
	if enc := q.Encode(); enc != "" {
		p += "?" + enc
	}
	return p
}

// humanSpan renders a duration compactly: 30 с, 2 мин, 1 ч 30 мин, 3 д.
func humanSpan(d time.Duration) string {
	if d < 0 {
		d = -d
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%d с", int(d.Seconds()))
	case d < time.Hour:
		if s := int(d.Seconds()) % 60; s != 0 && d < 10*time.Minute {
			return fmt.Sprintf("%d мин %d с", int(d.Minutes()), s)
		}
		return fmt.Sprintf("%d мин", int(d.Minutes()))
	case d < 48*time.Hour:
		if m := int(d.Minutes()) % 60; m != 0 {
			return fmt.Sprintf("%d ч %d мин", int(d.Hours()), m)
		}
		return fmt.Sprintf("%d ч", int(d.Hours()))
	default:
		return fmt.Sprintf("%d д", int(d.Hours()/24))
	}
}

func humanAgo(t, now time.Time) string {
	return humanSpan(now.Sub(t)) + " назад"
}

func humanIn(t, now time.Time) string {
	if !t.After(now) {
		return "сейчас"
	}
	return "через " + humanSpan(t.Sub(now))
}
