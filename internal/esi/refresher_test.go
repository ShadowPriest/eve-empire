package esi

import (
	"testing"
	"time"
)

func TestKindOf(t *testing.T) {
	cases := map[string]string{
		baseURL + "/characters/123/online/":                     "online",
		baseURL + "/characters/123/online/?language=ru":         "online",
		baseURL + "/characters/123/wallet/":                     "wallet",
		baseURL + "/characters/123/wallet/journal/?page=2":      "journal",
		baseURL + "/characters/123/":                            "char_public",
		baseURL + "/characters/123/mail/labels/":                "mail_labels",
		baseURL + "/characters/123/mail/456/":                   "mail_body",
		baseURL + "/characters/123/mail/":                       "mail",
		baseURL + "/corporations/9/wallets/3/journal/":          "corp_journal",
		compatBase + "/corporations/9/projects?limit=20":        "corp_projects",
		baseURL + "/markets/10000002/orders/?type_id=34&page=1": "market_orders",
		baseURL + "/universe/types/34/":                         "sde",
		baseURL + "/universe/structures/1000000000001/":         "universe",
		baseURL + "/status/":                                    "other",
	}
	for url, want := range cases {
		if got := kindOf(url).Name; got != want {
			t.Errorf("%s: kind %q, want %q", url, got, want)
		}
	}
}

func TestScheduleTiers(t *testing.T) {
	r := newRefresher(&state{})
	r.mode = ModeFull
	now := time.Now()
	ttl := 2 * time.Minute
	e := &entry{
		url: baseURL + "/characters/1/skills/", kind: kindOf(baseURL + "/characters/1/skills/"),
		expires: now.Add(-time.Second), ttl: ttl, lastRead: now,
	}

	// Warm: skills default to 3× — due two more TTLs after Expires.
	tier, due, ok := r.schedule(e, now)
	if !ok || tier != 1 {
		t.Fatalf("warm entry: ok=%v tier=%d", ok, tier)
	}
	if want := e.expires.Add(2 * ttl); !due.Equal(want) {
		t.Errorf("warm due %v, want %v", due, want)
	}

	// Hot: a page read makes it due right at Expires, multiplier ignored.
	e.hotUntil = now.Add(hotWindow)
	tier, due, ok = r.schedule(e, now)
	if !ok || tier != 0 || !due.Equal(e.expires) {
		t.Errorf("hot entry: ok=%v tier=%d due=%v", ok, tier, due)
	}

	// Demand mode: nothing warm is scheduled, hot still is.
	r.mode = ModeDemand
	if _, _, ok := r.schedule(e, now); !ok {
		t.Error("demand mode dropped a hot entry")
	}
	e.hotUntil = time.Time{}
	if _, _, ok := r.schedule(e, now); ok {
		t.Error("demand mode scheduled a warm entry")
	}
	r.mode = ModeFull

	// Multiplier 0 = on demand only; a stale-for-days entry drops out.
	r.mult["skills"] = 0
	if _, _, ok := r.schedule(e, now); ok {
		t.Error("mult 0 still scheduled")
	}
	delete(r.mult, "skills")
	e.lastRead = now.Add(-warmWindow - time.Hour)
	if _, _, ok := r.schedule(e, now); ok {
		t.Error("entry unread for days still scheduled")
	}
	e.lastRead = now

	// Failures push the due moment out by the backoff (hot: otherwise the
	// warm due two TTLs away would already be later than the backoff).
	e.hotUntil = now.Add(hotWindow)
	e.failures = 2
	e.lastTry = now
	_, due, _ = r.schedule(e, now)
	if want := now.Add(2 * backoffMin); !due.Equal(want) {
		t.Errorf("backoff due %v, want %v", due, want)
	}
}

func TestPickPrefersHotThenEarliest(t *testing.T) {
	r := newRefresher(&state{})
	r.mode = ModeFull
	now := time.Now()
	mk := func(path string, expires time.Time, hot bool) *entry {
		e := &entry{url: baseURL + path, kind: kindOf(baseURL + path),
			expires: expires, ttl: time.Minute, lastRead: now}
		if hot {
			e.hotUntil = now.Add(hotWindow)
		}
		r.entries[e.url] = e
		return e
	}
	warmOld := mk("/characters/1/wallet/", now.Add(-time.Hour), false)
	warmNew := mk("/characters/2/wallet/", now.Add(-time.Minute), false)
	hot := mk("/characters/3/wallet/", now.Add(-time.Second), true)
	future := mk("/characters/4/wallet/", now.Add(time.Minute), true)
	// An older page's hot entry, expired long ago: the page being looked
	// at now (read more recently) still goes first.
	hotOld := mk("/characters/5/wallet/", now.Add(-time.Hour), true)
	hotOld.lastRead = now.Add(-time.Minute)

	if got := r.pick(now); got != hot {
		t.Fatalf("first pick %v, want the current page's hot entry", got)
	}
	if got := r.pick(now); got != hotOld {
		t.Fatalf("second pick %v, want the older page's hot entry", got)
	}
	if got := r.pick(now); got != warmOld {
		t.Fatalf("third pick %v, want the oldest warm entry", got)
	}
	if got := r.pick(now); got != warmNew {
		t.Fatalf("fourth pick %v, want the newer warm entry", got)
	}
	if got := r.pick(now); got != nil {
		t.Fatalf("fifth pick %v, want nothing (only %s is left and it is not due)", got, future.url)
	}

	// The error budget pauses everything.
	warmOld.inflight = false
	r.budget(5, 30, false)
	if got := r.pick(now); got != nil {
		t.Fatalf("pick during pause returned %v", got)
	}
}

func TestStaticKindsOnlyFillGaps(t *testing.T) {
	r := newRefresher(&state{})
	r.mode = ModeFull
	url := baseURL + "/universe/types/34/"
	expired := cacheEntry{expires: time.Now().Add(-time.Hour), fetched: time.Now().Add(-2 * time.Hour)}

	// A body exists: a page read neither makes it hot nor reports stale.
	if r.touch(url, 0, false, expired, true, true) {
		t.Error("static entry with a body reported pending")
	}
	if _, _, ok := r.schedule(r.entries[url], time.Now()); ok {
		t.Error("static entry with a body was scheduled")
	}

	// Nothing cached: fetched once, like any missing entry.
	missing := baseURL + "/universe/types/35/"
	if !r.touch(missing, 0, false, cacheEntry{}, false, true) {
		t.Error("missing static entry not pending")
	}
	if tier, _, ok := r.schedule(r.entries[missing], time.Now()); !ok || tier != 0 {
		t.Errorf("missing static entry: ok=%v tier=%d, want hot", ok, tier)
	}

	// An open page (SSE subscription) watching a static route with a
	// body does not make it hot either.
	r.SetWatched(map[string]struct{}{url: {}})
	if _, _, ok := r.schedule(r.entries[url], time.Now()); ok {
		t.Error("watched static entry with a body was scheduled")
	}
}

func TestWatchedIsHot(t *testing.T) {
	r := newRefresher(&state{})
	r.mode = ModeFull
	now := time.Now()
	url := baseURL + "/characters/1/skills/"
	e := &entry{url: url, kind: kindOf(url), expires: now.Add(-time.Second), ttl: 2 * time.Minute, lastRead: now}
	r.entries[url] = e
	if tier, _, _ := r.schedule(e, now); tier != 1 {
		t.Fatalf("unwatched entry tier %d, want warm", tier)
	}
	r.SetWatched(map[string]struct{}{url: {}})
	if tier, due, ok := r.schedule(e, now); !ok || tier != 0 || !due.Equal(e.expires) {
		t.Errorf("watched entry: ok=%v tier=%d due=%v, want hot at Expires", ok, tier, due)
	}
	r.SetWatched(map[string]struct{}{})
	if tier, _, _ := r.schedule(e, now); tier != 1 {
		t.Errorf("entry stayed hot after the page closed")
	}
}

func TestBoost(t *testing.T) {
	r := newRefresher(&state{})
	r.mode = ModeFull
	now := time.Now()
	mk := func(path string, expires time.Time) *entry {
		e := &entry{url: baseURL + path, kind: kindOf(baseURL + path),
			expires: expires, ttl: time.Minute, lastRead: now.Add(-time.Hour)}
		r.entries[e.url] = e
		return e
	}
	expired := mk("/characters/1/wallet/", now.Add(-time.Minute))
	expired.failures = 3
	fresh := mk("/characters/1/skills/", now.Add(40*time.Second))
	sde := mk("/universe/types/34/", now.Add(-time.Hour))

	boosted, next := r.Boost([]string{expired.url, fresh.url, sde.url, "unknown"})
	if boosted != 1 {
		t.Errorf("boosted %d, want 1 (only the expired non-static route)", boosted)
	}
	if next < 39*time.Second || next > 40*time.Second {
		t.Errorf("next %v, want ~40s (the fresh route's Expires)", next)
	}
	if expired.failures != 0 || !expired.hotUntil.After(now) {
		t.Error("boosted route not hot or backoff not cleared")
	}
	if tier, _, ok := r.schedule(expired, now); !ok || tier != 0 {
		t.Errorf("boosted route: ok=%v tier=%d, want hot", ok, tier)
	}
}

func TestBackoff(t *testing.T) {
	if backoff(1) != backoffMin || backoff(2) != 2*backoffMin || backoff(10) != backoffMax {
		t.Errorf("backoff series wrong: %v %v %v", backoff(1), backoff(2), backoff(10))
	}
}
