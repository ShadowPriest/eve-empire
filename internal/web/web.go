// Package web serves the cabinet UI and the SSO login/callback endpoints.
package web

import (
	"bytes"
	"crypto/rand"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html/template"
	"log"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"eve-empire/internal/esi"
	"eve-empire/internal/pi"
	"eve-empire/internal/sde"
	"eve-empire/internal/skillplan"
	"eve-empire/internal/sso"
	"eve-empire/internal/store"
)

//go:embed templates/*.html
var templateFS embed.FS

const stateCookie = "eve_sso_state"

// backCookie carries where to return after the SSO round-trip. Needed for
// the re-authorization page: after login the callback must come back to the
// list instead of the character card, otherwise 27 logins mean 27 manual
// navigations back.
const backCookie = "eve_sso_back"

type Server struct {
	SSO   *sso.Client
	ESI   *esi.Client
	Store *store.Store
	SDE   *sde.DB
	pages map[string]*template.Template
}

func New(ssoClient *sso.Client, esiClient *esi.Client, st *store.Store, sdeDB *sde.DB) (*Server, error) {
	funcs := template.FuncMap{
		"isk":   formatISK,
		"num":   formatNum,
		"until": humanUntil,
		"join": func(v any) string {
			if l, ok := v.([]string); ok {
				return strings.Join(l, "\n")
			}
			return ""
		},
		"jsonenc": func(v any) string {
			b, err := json.Marshal(v)
			if err != nil {
				return "{}"
			}
			return string(b)
		},
		"iskshort": iskShort,
		"iskraw":   func(v float64) string { return formatNum(int64(v)) },
		// m3 renders a volume: whole cubic metres, space-separated.
		"m3": func(v float64) string { return formatNum(int64(v + 0.5)) },
		// unitprice keeps the kopecks that ore prices live in.
		"unitprice": func(v float64) string {
			if v == 0 {
				return "—"
			}
			if v < 1000 {
				return strconv.FormatFloat(v, 'f', 2, 64)
			}
			return formatNum(int64(v + 0.5))
		},
		// metric formats a value the way its chart's unit demands.
		"metricval": func(m miningMetric, v float64) string {
			if m.Money {
				return iskShort(v)
			}
			return formatNum(int64(v + 0.5))
		},
		"mulf": func(a, b float64) float64 { return a * b },
		"sumf": func(nums ...float64) float64 {
			var t float64
			for _, n := range nums {
				t += n
			}
			return t
		},
		"float": func(v int64) float64 { return float64(v) },
		// indent turns a tree depth into the left padding of its row.
		"indent": func(depth int) string { return strconv.Itoa((depth - 1) * 22) },
		// qs rewrites the current query string: qs .Q "s" "refined" —
		// used by the ore table, whose entire state is in the URL.
		"qs": func(q map[string]string, kv ...string) string {
			next := make(map[string]string, len(q))
			for k, v := range q {
				next[k] = v
			}
			for i := 0; i+1 < len(kv); i += 2 {
				next[kv[i]] = kv[i+1]
			}
			keys := make([]string, 0, len(next))
			for k := range next {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			parts := make([]string, 0, len(keys))
			for _, k := range keys {
				if next[k] == "" {
					continue
				}
				parts = append(parts, k+"="+url.QueryEscape(next[k]))
			}
			if len(parts) == 0 {
				return "?"
			}
			return "?" + strings.Join(parts, "&")
		},
		// ifsort gives a sortable header its next direction: clicking a
		// value column starts with the biggest first, a name column A→Я.
		"ifsort": func(sort, dir, key string) string {
			if key == "name" {
				if sort == key && dir != "desc" {
					return "desc"
				}
				return ""
			}
			if sort == key && dir != "asc" {
				return "asc"
			}
			return "desc"
		},
		// sprate печатает скорость обучения: СП в минуту — та
		// единица, в которой это показывает и сама игра.
		"sprate": func(v float64) string {
			if v <= 0 {
				return ""
			}
			return strconv.FormatFloat(math.Round(v*10)/10, 'f', -1, 64) + " СП/мин"
		},
		"volstr": func(v float64) string {
			return strconv.FormatFloat(v, 'f', -1, 64) + " м³"
		},
		// pct formats a signed percentage: two decimals at most, and no
		// trailing zeros — a grade is "+5%", not "+5.00%".
		"pct": func(v float64) string {
			s := strconv.FormatFloat(math.Round(v*100)/100, 'f', -1, 64)
			if v > 0 {
				s = "+" + s
			}
			return s + "%"
		},
		// qty prints a material amount: fractions matter below ten.
		"qty": func(v float64) string {
			switch {
			case v == 0:
				return "—"
			case v < 10:
				return strconv.FormatFloat(v, 'f', 3, 64)
			case v < 1000:
				return strconv.FormatFloat(v, 'f', 1, 64)
			}
			return formatNum(int64(v + 0.5))
		},
		"sub": func(a, b int) int { return a - b },
		"add": func(nums ...int) int {
			t := 0
			for _, n := range nums {
				t += n
			}
			return t
		},
		"since": func(t, now time.Time) string { return humanDur(now.Sub(t)) },
		"dur":   humanDur,
		// dict packs named arguments for a {{template}} call.
		"dict": func(kv ...any) map[string]any {
			m := map[string]any{}
			for i := 0; i+1 < len(kv); i += 2 {
				key, _ := kv[i].(string)
				m[key] = kv[i+1]
			}
			return m
		},
		"tagsjoin": func(tags []string) string { return strings.Join(tags, ", ") },
		"tagsattr": func(tags []string) string { return strings.Join(tags, "|") },
		// item renders an icon + clickable name for a bare type id.
		"item": func(typeID int64, name string) template.HTML {
			return itemChip(chipData{TypeID: typeID, Name: name})
		},
		// chip is the universal item element: it accepts any known item
		// struct (asset line, owned blueprint, ...) and carries whatever
		// extra state that item has — copy artwork, ME/TE, runs.
		"chip": func(v any) template.HTML {
			switch it := v.(type) {
			case esi.AssetItem:
				return itemChip(chipData{it.TypeID, it.TypeName, it.IsCopy, it.ME, it.TE, it.Runs})
			case esi.OwnedBlueprint:
				return itemChip(chipData{it.TypeID, it.TypeName, it.IsCopy(), it.ME, it.TE, it.Runs})
			default:
				return template.HTML("")
			}
		},
		"roman": func(n int) string {
			r := []string{"", "I", "II", "III", "IV", "V"}
			if n >= 1 && n < len(r) {
				return r[n]
			}
			return strconv.Itoa(n)
		},
		// emptyslots дополняет ряд портретов до тройки — столько
		// слотов у аккаунта в игре, и пустые видно заглушками.
		"emptyslots": func(n int) []int {
			k := (3 - n%3) % 3
			if n == 0 {
				k = 3
			}
			return make([]int, k)
		},
		"seq": func(n int) []int {
			s := make([]int, n)
			for i := range s {
				s[i] = i + 1
			}
			return s
		},
		// squares is the five-per-skill level indicator EVE draws next to
		// a skill: white up to the level really trained, teal up to the
		// level the queue will reach.
		"squares": func(trained, target int) template.HTML {
			var b strings.Builder
			b.WriteString(`<span class="sq">`)
			for l := 1; l <= 5; l++ {
				switch {
				case l <= trained:
					b.WriteString(`<i class="on"></i>`)
				case l <= target:
					b.WriteString(`<i class="q"></i>`)
				default:
					b.WriteString(`<i></i>`)
				}
			}
			b.WriteString(`</span>`)
			return template.HTML(b.String())
		},
	}
	layout, err := template.New("").Funcs(funcs).ParseFS(templateFS, "templates/layout.html")
	if err != nil {
		return nil, err
	}
	pages := map[string]*template.Template{}
	for _, name := range []string{
		"welcome", "empire", "overview", "skills", "wallet", "industry", "market",
		"assets", "projects", "corp_info", "corp_industry", "corp_wallets", "corp_assets",
		"settings", "skill_planner", "clones", "blueprints", "planetary_tool", "planets",
		"empire_planets", "empire_wallets", "empire_training", "empire_industry", "fleet",
		"mining", "ore", "market_watch", "build", "orders", "mail", "empire_structures",
		"reauth", "accounting",
	} {
		t, err := template.Must(layout.Clone()).ParseFS(templateFS, "templates/"+name+".html")
		if err != nil {
			return nil, err
		}
		pages[name] = t
	}
	return &Server{SSO: ssoClient, ESI: esiClient, Store: st, SDE: sdeDB, pages: pages}, nil
}

func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.handleIndex)
	mux.HandleFunc("GET /settings", s.handleSettings)
	mux.HandleFunc("GET /reauth", s.handleReauth)
	mux.HandleFunc("GET /accounting", s.handleAccounting)
	mux.HandleFunc("POST /accounting/build", s.handleAccountingBuild)
	mux.HandleFunc("POST /accounting/recon", s.handleAccountingRecon)
	mux.HandleFunc("POST /settings/language", s.handleSetLanguage)
	mux.HandleFunc("GET /planets", s.handleEmpirePlanets)
	mux.HandleFunc("GET /mining", s.handleEmpireMining)
	mux.HandleFunc("GET /wallets", s.handleEmpireWallets)
	mux.HandleFunc("GET /training", s.handleEmpireTraining)
	mux.HandleFunc("GET /industry", s.handleEmpireIndustry)
	mux.HandleFunc("GET /structures", s.handleEmpireStructures)
	mux.HandleFunc("GET /tools/fleet", s.handleFleetTool)
	mux.HandleFunc("POST /tools/fleet/action", s.handleFleetAction)
	mux.HandleFunc("GET /tools/ore", s.handleOreTool)
	mux.HandleFunc("POST /tools/ore/refinery", s.handleRefineSettings)
	mux.HandleFunc("GET /tools/market", s.handleMarketWatch)
	mux.HandleFunc("GET /tools/orders", s.handleOrdersTool)
	mux.HandleFunc("GET /tools/build", s.handleBuildCalc)
	mux.HandleFunc("GET /tools/skill-planner", s.handleSkillPlanner)
	mux.HandleFunc("POST /tools/skill-planner", s.handleSkillPlanner)
	mux.HandleFunc("POST /tools/skill-planner/save", s.handleSkillPlanSave)
	mux.HandleFunc("POST /tools/skill-planner/delete", s.handleSkillPlanDelete)
	mux.HandleFunc("GET /tools/planetary", s.handlePlanetaryTool)
	mux.HandleFunc("POST /tools/planetary/import", s.handlePIImport)
	mux.HandleFunc("POST /tools/planetary/generate", s.handlePIGenerate)
	mux.HandleFunc("POST /tools/planetary/delete", s.handlePIDelete)
	mux.HandleFunc("POST /tools/planetary/refineries", s.handlePIRefineries)
	mux.HandleFunc("GET /login", s.handleLogin)
	mux.HandleFunc("GET /callback", s.handleCallback)
	mux.HandleFunc("GET /characters/{id}", s.handleOverview)
	mux.HandleFunc("GET /characters/{id}/skills", s.handleSkills)
	mux.HandleFunc("GET /characters/{id}/wallet", s.handleWallet)
	mux.HandleFunc("GET /characters/{id}/industry", s.handleIndustry)
	mux.HandleFunc("GET /characters/{id}/market", s.handleMarket)
	mux.HandleFunc("GET /characters/{id}/assets", s.handleAssets)
	mux.HandleFunc("GET /characters/{id}/clones", s.handleClones)
	mux.HandleFunc("GET /characters/{id}/blueprints", s.handleBlueprints)
	mux.HandleFunc("GET /characters/{id}/planets", s.handlePlanets)
	mux.HandleFunc("GET /characters/{id}/mail", s.handleMail)
	mux.HandleFunc("GET /api/mail/{id}/{mail}", s.handleMailJSON)
	mux.HandleFunc("POST /characters/{id}/account", s.handleSetAccount)
	mux.HandleFunc("POST /characters/{id}/tags", s.handleSetTags)
	mux.HandleFunc("POST /characters/{id}/delete", s.handleDelete)
	mux.HandleFunc("GET /corporations/{id}/info", s.handleCorpInfo)
	mux.HandleFunc("GET /corporations/{id}/projects", s.handleCorpProjects)
	mux.HandleFunc("GET /corporations/{id}/industry", s.handleCorpIndustry)
	mux.HandleFunc("GET /corporations/{id}/wallets", s.handleCorpWallets)
	mux.HandleFunc("GET /corporations/{id}/assets", s.handleCorpAssets)
	mux.HandleFunc("GET /api/market", s.handleMarketDepth)
	mux.HandleFunc("GET /api/type/{id}", s.handleTypeInfo)
	mux.HandleFunc("GET /icons/{id}", s.handleIcon)
	mux.HandleFunc("POST /sidebar/order", s.handleSidebarOrder)
	mux.HandleFunc("POST /bulk/waypoint", s.handleBulkWaypoint)
	return mux
}

// esiFor picks the ESI view for a request: the default is the stale
// view (instant render from cache); a revalidation request (X-Fresh
// header, sent by the page's own JS) uses the strict client.
func (s *Server) esiFor(r *http.Request) (*esi.Client, *atomic.Bool) {
	if r.Header.Get("X-Fresh") == "1" {
		return s.ESI, nil
	}
	return s.ESI.StaleView()
}

// ── shell data (sidebar + menu) ──────────────────────────────────────

type accountGroup struct {
	Key   string // raw account value ("" for unassigned) — used by drag&drop
	Name  string // display title
	Chars []sideChar
}

// sideChar enriches a stored character with the sidebar card data.
type sideChar struct {
	store.Character
	Wallet     float64
	Online     bool
	Lines      lineStats
	Trained    int64 // total skillpoints
	UnreadMail int64 // red badge on the portrait

	// Skill queue: the entry training right now plus the plan tail.
	QueueSkillID  int64
	QueueSkill    string
	QueueLevel    int
	QueueTrained  int // levels really trained (white squares); up to QueueLevel — queued
	QueueEnds     time.Time
	QueueLen      int       // entries still to finish
	QueuePlanEnds time.Time // when the whole queue runs dry
	QueuePaused   bool      // entries exist but nothing is training
}

// sideInfo gathers wallet / training / industry-line data for one
// sidebar card (all served from cache in stale mode).
func (s *Server) sideInfo(ec *esi.Client, ch store.Character) sideChar {
	sc := sideChar{Character: ch}

	var wg sync.WaitGroup
	var jobs []esi.IndustryJob
	var sheet *esi.SkillSheet
	var queue []esi.QueueEntry
	wg.Add(6)
	go func() {
		defer wg.Done()
		sc.Wallet, _ = ec.WalletBalance(ch.ID)
	}()
	go func() {
		defer wg.Done()
		if ml, err := ec.MailLabelList(ch.ID); err == nil && ml != nil {
			sc.UnreadMail = ml.TotalUnread
		}
	}()
	go func() {
		defer wg.Done()
		sc.Online, _ = ec.Online(ch.ID)
	}()
	go func() {
		defer wg.Done()
		queue, _ = ec.SkillQueue(ch.ID)
	}()
	go func() {
		defer wg.Done()
		sheet, _ = ec.Skills(ch.ID)
	}()
	go func() {
		defer wg.Done()
		personal, err := ec.IndustryJobs(ch.ID)
		if err == nil {
			jobs = personal
		}
		corpID, _, err := ec.CharacterPublic(ch.ID)
		if err != nil || corpID == 0 {
			return
		}
		corpJobs, err := ec.CorporationIndustryJobs(ch.ID, corpID)
		if err != nil {
			return
		}
		for _, cj := range corpJobs {
			if cj.InstallerID == ch.ID {
				jobs = append(jobs, cj.IndustryJob)
			}
		}
	}()
	wg.Wait()

	if sheet == nil {
		sheet = &esi.SkillSheet{}
	}
	sc.Lines = industryLines(sheet, jobs)
	sc.Trained = sheet.TotalSP

	// The head of the queue drives the sidebar card and the empire-wide
	// training list; the squares need the level really trained now.
	trained := map[int64]int{}
	for _, sk := range sheet.Skills {
		trained[sk.SkillID] = sk.TrainedLevel
	}
	now := time.Now()
	for _, q := range queue {
		if !q.FinishDate.IsZero() && q.FinishDate.Before(now) {
			continue // already completed
		}
		sc.QueueLen++
		if q.FinishDate.After(sc.QueuePlanEnds) {
			sc.QueuePlanEnds = q.FinishDate
		}
		if sc.QueueSkillID != 0 || q.FinishDate.IsZero() {
			continue
		}
		sc.QueueSkillID, sc.QueueSkill = q.SkillID, q.SkillName
		sc.QueueLevel, sc.QueueEnds = q.FinishedLevel, q.FinishDate
		sc.QueueTrained = q.FinishedLevel - 1
		if lv := trained[q.SkillID]; lv < sc.QueueTrained {
			sc.QueueTrained = lv
		}
	}
	sc.QueuePaused = sc.QueueLen > 0 && sc.QueueSkillID == 0
	return sc
}

type corpEntry struct {
	ID        int64
	Name      string
	ViaCharID int64 // any authed character belonging to this corp
}

// shell builds data every page shares: sidebar groups, corp list, selection.
// charSections — реально существующие подстраницы персонажа.
var charSections = map[string]bool{
	"skills": true, "wallet": true, "industry": true, "market": true,
	"assets": true, "clones": true, "blueprints": true, "planets": true,
	"mail": true,
}

func (s *Server) shell(ec *esi.Client, selectedID int64, section string) (map[string]any, *store.Character, error) {
	// Section — это подстраница ПЕРСОНАЖА: ссылки альтов в сайдбаре
	// ведут в тот же раздел, что открыт сейчас. Страница настроек
	// таковой не является, и /characters/{id}/settings отдавал 404.
	if !charSections[section] {
		section = ""
	}
	chars, err := s.Store.Characters()
	if err != nil {
		return nil, nil, err
	}

	// Sidebar card data per character, concurrently (cache-backed).
	sides := make([]sideChar, len(chars))
	var wg sync.WaitGroup
	for i, ch := range chars {
		wg.Add(1)
		go func(i int, ch store.Character) {
			defer wg.Done()
			sides[i] = s.sideInfo(ec, ch)
		}(i, ch)
	}
	wg.Wait()

	// Characters come pre-sorted by (account position, sort_order).
	var groups []accountGroup
	idx := map[string]int{}
	tagSet := map[string]bool{}
	for i, ch := range chars {
		gi, ok := idx[ch.Account]
		if !ok {
			gi = len(groups)
			idx[ch.Account] = gi
			name := ch.Account
			if name == "" {
				name = "Без аккаунта"
				if len(groups) == 0 {
					name = "Персонажи"
				}
			}
			groups = append(groups, accountGroup{Key: ch.Account, Name: name})
		}
		groups[gi].Chars = append(groups[gi].Chars, sides[i])
		for _, t := range ch.Tags {
			tagSet[t] = true
		}
	}
	var allTags []string
	for t := range tagSet {
		allTags = append(allTags, t)
	}
	sort.Strings(allTags)

	var selected *store.Character
	for i := range chars {
		if chars[i].ID == selectedID {
			selected = &chars[i]
			break
		}
	}

	return map[string]any{
		"Groups":       groups,
		"AllTags":      allTags,
		"Selected":     selected,
		"Section":      section,
		"Corporations": s.corporations(ec, chars),
		"Now":          time.Now(),
	}, selected, nil
}

// corporations collects distinct corporations across all characters
// (public info, cached by the ESI client).
func (s *Server) corporations(ec *esi.Client, chars []store.Character) []corpEntry {
	type result struct {
		charID int64
		corpID int64
		name   string
	}
	results := make([]result, len(chars))
	var wg sync.WaitGroup
	for i, ch := range chars {
		wg.Add(1)
		go func(i int, id int64) {
			defer wg.Done()
			corpID, name, err := ec.CharacterPublic(id)
			if err != nil {
				return
			}
			results[i] = result{charID: id, corpID: corpID, name: name}
		}(i, ch.ID)
	}
	wg.Wait()

	seen := map[int64]bool{}
	var corps []corpEntry
	for _, r := range results {
		if r.corpID == 0 || seen[r.corpID] {
			continue
		}
		seen[r.corpID] = true
		corps = append(corps, corpEntry{ID: r.corpID, Name: r.name, ViaCharID: r.charID})
	}
	sort.Slice(corps, func(i, j int) bool { return corps[i].Name < corps[j].Name })
	return corps
}

func (s *Server) render(w http.ResponseWriter, page string, data map[string]any, stale *atomic.Bool) {
	if stale != nil && stale.Load() {
		data["Stale"] = true
	}
	if err := s.pages[page].ExecuteTemplate(w, "layout.html", data); err != nil {
		log.Printf("render %s: %v", page, err)
	}
}

// ── character pages ──────────────────────────────────────────────────

// corpWalletRow is one corporation's wallet summary on the empire page.
type corpWalletRow struct {
	Corp      corpEntry
	Total     float64
	Note      string // e.g. no-access explanation
	OK        bool
	Divisions []corpDivision // filled only where the breakdown is shown
}

// corpDivision is one corporation wallet division.
type corpDivision struct {
	Division int
	Name     string
	Balance  float64
}

// handleSettings renders the settings page: character list with tag
// editor, add/delete, and (later) auth options.
func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	ec, stale := s.esiFor(r)
	data, _, err := s.shell(ec, 0, "settings")
	if err != nil {
		httpError(w, "loading characters", err)
		return
	}
	lang := s.Store.Setting("language")
	if lang == "" {
		lang = "en"
	}
	data["Language"] = lang
	data["Languages"] = esiLanguages
	s.render(w, "settings", data, stale)
}

// esiLanguages are the localizations ESI can serve.
var esiLanguages = []struct{ Code, Name string }{
	{"en", "English"},
	{"ru", "Русский"},
	{"de", "Deutsch"},
	{"fr", "Français"},
	{"es", "Español"},
	{"ja", "日本語"},
	{"ko", "한국어"},
	{"zh", "中文"},
}

func (s *Server) handleSetLanguage(w http.ResponseWriter, r *http.Request) {
	lang := r.FormValue("language")
	ok := false
	for _, l := range esiLanguages {
		if l.Code == lang {
			ok = true
			break
		}
	}
	if !ok {
		http.Error(w, "bad language", http.StatusBadRequest)
		return
	}
	if err := s.Store.SetSetting("language", lang); err != nil {
		httpError(w, "saving language", err)
		return
	}
	s.ESI.SetLanguage(lang)
	http.Redirect(w, r, "/settings", http.StatusFound)
}

// chipData is everything an item chip can carry into the info modal.
type chipData struct {
	TypeID int64
	Name   string
	IsCopy bool
	ME, TE int
	Runs   int64
}

// itemChip renders the universal icon+name element used across the UI.
func itemChip(d chipData) template.HTML {
	src := fmt.Sprintf("/icons/%d", d.TypeID)
	attrs := ""
	if d.IsCopy {
		src += "?bpc=1"
		attrs += ` data-bpc="1"`
	}
	if d.ME != 0 || d.TE != 0 {
		attrs += fmt.Sprintf(` data-me="%d" data-te="%d"`, d.ME, d.TE)
	}
	if d.Runs > 0 {
		attrs += fmt.Sprintf(` data-runs="%d"`, d.Runs)
	}
	esc := template.HTMLEscapeString(d.Name)
	return template.HTML(fmt.Sprintf(
		`<span class="itm" data-type="%d"%s title="%s"><img class="itmico" src="%s" alt="" loading="lazy"><span class="itmnm">%s</span></span>`,
		d.TypeID, attrs, esc, src, esc))
}

// handleIcon serves a type icon from the static database. Icons never
// change for a given type, so they are cached aggressively.
func (s *Server) handleIcon(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(strings.TrimSuffix(r.PathValue("id"), ".png"), 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	isCopy := r.URL.Query().Get("bpc") == "1"
	if png := s.SDE.Icon(id, isCopy); len(png) > 0 {
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		w.Write(png)
		return
	}
	// Fall back to the image CDN so the UI still shows something.
	variant := "icon"
	if isCopy {
		variant = "bpc"
	}
	http.Redirect(w, r, fmt.Sprintf("https://images.evetech.net/types/%d/%s?size=64", id, variant), http.StatusFound)
}

// handleTypeInfo backs the item info modal.
func (s *Server) handleTypeInfo(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	info := s.SDE.TypeInfo(id)
	// The client's "estimated value" is the global average price, not the
	// SDE base price (which is 0 for most modern items).
	estimated := info.BasePrice
	var adjusted float64
	if prices, err := s.ESI.MarketPrices(); err == nil {
		if p, ok := prices[id]; ok {
			if p.Average > 0 {
				estimated = p.Average
			}
			adjusted = p.Adjusted
		}
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if !info.Found {
		json.NewEncoder(w).Encode(map[string]any{
			"found": false,
			"error": "нет данных о предмете (sde.db не найдена или тип неизвестен)",
		})
		return
	}
	// Planetary items get an extra tab, like the client.
	var piData any
	if pi := s.SDE.PlanetaryInfo(id); pi.IsPlanetary {
		piData = pi
	}

	// Blueprints get their own card in the UI.
	var bpData any
	if s.SDE.IsBlueprint(id) {
		bp := s.SDE.Blueprint(id)
		bpData = map[string]any{
			"max_runs":   bp.MaxRuns,
			"activities": bp.Activities,
		}
	}

	type skillJSON struct {
		TypeID int64       `json:"type_id"`
		Name   string      `json:"name"`
		Level  int         `json:"level"`
		Nested []skillJSON `json:"nested,omitempty"`
	}
	var conv func([]sde.SkillReq) []skillJSON
	conv = func(in []sde.SkillReq) []skillJSON {
		out := make([]skillJSON, 0, len(in))
		for _, s := range in {
			out = append(out, skillJSON{s.TypeID, s.Name, s.Level, conv(s.Nested)})
		}
		return out
	}
	json.NewEncoder(w).Encode(map[string]any{
		"found":       true,
		"type_id":     info.TypeID,
		"name":        info.Name,
		"group":       info.GroupName,
		"description": sde.DescriptionHTML(info.Description),
		"volume":      info.Volume,
		"base_price":  estimated,
		"adjusted":    adjusted,
		"attrs":       info.Attrs,
		"skills":      conv(info.Skills),
		"variations":  info.Variations,
		"blueprint":   map[string]any{"type_id": info.BlueprintID, "name": info.BlueprintNm},
		"materials":   info.Materials,
		"bp":          bpData,
		"pi":          piData,
	})
}

// handlePlanetaryTool renders the full planetary production chain with
// prices, links and planet filters.
func (s *Server) handlePlanetaryTool(w http.ResponseWriter, r *http.Request) {
	ec, stale := s.esiFor(r)
	data, _, err := s.shell(ec, 0, "")
	if err != nil {
		httpError(w, "loading characters", err)
		return
	}
	chain := s.SDE.PlanetaryChain()

	prices, _ := s.ESI.MarketPrices()
	type nodeView struct {
		sde.PINode
		Price float64
	}
	view := map[string][]nodeView{}
	for _, tier := range chain.Tiers {
		for _, n := range chain.Nodes[tier] {
			view[tier] = append(view[tier], nodeView{PINode: n, Price: prices[n.TypeID].Average})
		}
	}

	// Recipes + prices go to the browser as JSON for the calculator.
	recipes := s.SDE.PIRecipes()
	type recJSON struct {
		sde.PIRecipe
		Price float64 `json:"p"`
	}
	calc := make(map[string]recJSON, len(recipes))
	for id, r := range recipes {
		calc[strconv.FormatInt(id, 10)] = recJSON{PIRecipe: r, Price: prices[id].Average}
	}
	calcJSON, _ := json.Marshal(calc)
	data["RecipesJSON"] = template.JS(calcJSON)

	data["Tiers"] = chain.Tiers
	data["TierNames"] = map[string]string{
		"P0": "Сырьё", "P1": "Базовые", "P2": "Очищенные", "P3": "Специализированные", "P4": "Продвинутые",
	}
	data["Nodes"] = view
	data["Planets"] = chain.Planets
	data["SDEReady"] = s.SDE.Available()

	// Stored colony templates + generator options.
	data["Templates"] = s.piViews()
	data["Tab"] = r.URL.Query().Get("tab")
	data["ImportErr"] = r.URL.Query().Get("err")
	data["Msg"] = r.URL.Query().Get("msg")
	var planetNames []string
	for name := range s.SDE.PlanetStructures() {
		planetNames = append(planetNames, name)
	}
	sort.Strings(planetNames)
	data["PlanetNames"] = planetNames

	s.render(w, "planetary_tool", data, stale)
}

// piTemplateView is a stored template plus its decoded summary.
type piTemplateView struct {
	store.PITemplate
	PlanetName string
	Product    string
	Factories  []piFactoryLine
	Imports    []string
	Pins       int
	Links      int
	Routes     int
	Err        string
}

type piFactoryLine struct {
	TypeID int64
	Name   string
	Tier   string
	Count  int
}

// piViews decodes stored templates for display.
func (s *Server) piViews() []piTemplateView {
	list, err := s.Store.PITemplates()
	if err != nil {
		return nil
	}
	out := make([]piTemplateView, 0, len(list))
	for _, t := range list {
		v := piTemplateView{PITemplate: t}
		tpl, err := pi.Parse([]byte(t.Payload))
		if err != nil {
			v.Err = err.Error()
			out = append(out, v)
			continue
		}
		sum := tpl.Describe(s.SDE.TierOf)
		v.Pins, v.Links, v.Routes = sum.Pins, sum.Links, sum.Routes
		for id, n := range sum.Factories {
			v.Factories = append(v.Factories, piFactoryLine{
				TypeID: id, Name: s.SDE.TypeNames([]int64{id})[id],
				Tier: s.SDE.TierOf(id), Count: n,
			})
		}
		sort.Slice(v.Factories, func(i, j int) bool {
			if v.Factories[i].Tier != v.Factories[j].Tier {
				return v.Factories[i].Tier > v.Factories[j].Tier
			}
			return v.Factories[i].Name < v.Factories[j].Name
		})
		names := s.SDE.TypeNames(sum.Imports)
		for _, id := range sum.Imports {
			if n := names[id]; n != "" {
				v.Imports = append(v.Imports, n)
			}
		}
		if sum.Final != 0 {
			v.Product = s.SDE.TypeNames([]int64{sum.Final})[sum.Final]
		}
		v.PlanetName = s.SDE.TypeNames([]int64{t.PlanetType})[t.PlanetType]
		out = append(out, v)
	}
	return out
}

func (s *Server) handlePIImport(w http.ResponseWriter, r *http.Request) {
	payload := strings.TrimSpace(r.FormValue("payload"))
	tpl, err := pi.Parse([]byte(payload))
	if err != nil {
		http.Redirect(w, r, "/tools/planetary?tab=templates&err="+url.QueryEscape(err.Error()), http.StatusFound)
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	if name == "" {
		name = tpl.Cmt
	}
	if name == "" {
		name = "Шаблон"
	}
	sum := tpl.Describe(s.SDE.TierOf)
	if _, err := s.Store.AddPITemplate(store.PITemplate{
		Name: name, PlanetType: tpl.Pln, ProductType: sum.Final,
		CmdCtrLv: tpl.CmdCtrLv, Payload: payload,
	}); err != nil {
		httpError(w, "saving template", err)
		return
	}
	http.Redirect(w, r, "/tools/planetary?tab=templates", http.StatusFound)
}

func (s *Server) handlePIDelete(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.FormValue("id"), 10, 64)
	if err := s.Store.DeletePITemplate(id); err != nil {
		httpError(w, "deleting template", err)
		return
	}
	http.Redirect(w, r, "/tools/planetary?tab=templates", http.StatusFound)
}

// handlePIGenerate builds a factory-planet template for a target
// commodity: one factory line per input tier, everything routed through
// a central launchpad.
func (s *Server) handlePIGenerate(w http.ResponseWriter, r *http.Request) {
	target, _ := strconv.ParseInt(r.FormValue("target"), 10, 64)
	planet := r.FormValue("planet")
	lines, _ := strconv.Atoi(r.FormValue("lines"))
	if lines <= 0 {
		lines = 1
	}
	structs := s.SDE.PlanetStructures()[planet]
	recipes := s.SDE.PIRecipes()
	rec, ok := recipes[target]
	if !ok || structs.Launchpad == 0 {
		http.Redirect(w, r, "/tools/planetary?tab=templates&err="+
			url.QueryEscape("не удалось собрать шаблон: нет данных о товаре или планете"), http.StatusFound)
		return
	}

	// The target line, plus one line per input that we can also make here.
	var factories []pi.GenFactory
	add := func(rc sde.PIRecipe, count int) {
		if len(rc.Inputs) == 0 {
			return
		}
		in := map[int64]int64{}
		for _, p := range rc.Inputs {
			in[p[0]] = p[1]
		}
		factories = append(factories, pi.GenFactory{
			Product: rc.TypeID, Count: count, Tier: rc.Tier,
			Inputs: in, Output: rc.OutQty,
		})
	}
	add(rec, lines)
	if r.FormValue("subs") == "1" {
		for _, p := range rec.Inputs {
			if sub, ok := recipes[p[0]]; ok && len(sub.Inputs) > 0 {
				add(sub, lines)
			}
		}
	}

	tpl := pi.Generate(pi.GenSpec{
		Name:     fmt.Sprintf("%s x%d (EVE Empire)", rec.Name, lines),
		CmdCtrLv: 5,
		Diameter: 4000,
		Struct: pi.PlanetStructures{
			PlanetType: structs.PlanetType, CommandCtr: structs.CommandCtr,
			Launchpad: structs.Launchpad, Storage: structs.Storage,
			BasicFactory: structs.BasicFactory, AdvFactory: structs.AdvFactory,
			HighTech: structs.HighTech,
		},
		Factories: factories,
	})
	payload, err := tpl.JSON()
	if err != nil {
		httpError(w, "generating template", err)
		return
	}
	if _, err := s.Store.AddPITemplate(store.PITemplate{
		Name: tpl.Cmt, PlanetType: tpl.Pln, ProductType: target,
		CmdCtrLv: tpl.CmdCtrLv, Payload: string(payload),
	}); err != nil {
		httpError(w, "saving template", err)
		return
	}
	http.Redirect(w, r, "/tools/planetary?tab=templates", http.StatusFound)
}

// handlePIRefineries generates one extractor-less P0→P1 refinery
// template per basic commodity, on a planet that actually yields the
// raw material.
func (s *Server) handlePIRefineries(w http.ResponseWriter, r *http.Request) {
	// The layout comes from a hand-built colony (compact links = low
	// powergrid); we only swap the planet's structures and the recipe.
	donor, err := pi.RefineryDonor()
	if err != nil {
		httpError(w, "donor template", err)
		return
	}
	structs := s.SDE.PlanetStructures()
	recipes := s.SDE.PIRecipes()
	planetsOf := s.SDE.RawPlanets()
	roleOf := s.SDE.StructureRole

	made := 0
	for id, rec := range recipes {
		if rec.Tier != "P1" || len(rec.Inputs) != 1 {
			continue
		}
		input := rec.Inputs[0][0]
		planets := planetsOf[input]
		if len(planets) == 0 {
			continue
		}
		sort.Strings(planets)
		ps, ok := structs[planets[0]]
		if !ok || ps.Launchpad == 0 || ps.BasicFactory == 0 {
			continue
		}
		tpl := pi.Reskin(donor, pi.ReskinSpec{
			Name:           fmt.Sprintf("%s - %s", rec.Name, planets[0]),
			PlanetType:     ps.PlanetType,
			Launchpad:      ps.Launchpad,
			BasicFactory:   ps.BasicFactory,
			AdvFactory:     ps.AdvFactory,
			Storage:        ps.Storage,
			CommandCtr:     ps.CommandCtr,
			Product:        id,
			Input:          input,
			RoleOf:         roleOf,
			DropExtractors: true,
		})
		payload, err := tpl.JSON()
		if err != nil {
			continue
		}
		if _, err := s.Store.AddPITemplate(store.PITemplate{
			Name: tpl.Cmt, PlanetType: tpl.Pln, ProductType: id,
			CmdCtrLv: tpl.CmdCtrLv, Payload: string(payload),
		}); err == nil {
			made++
		}
	}
	http.Redirect(w, r, fmt.Sprintf("/tools/planetary?tab=templates&msg=%s",
		url.QueryEscape(fmt.Sprintf("создано шаблонов: %d", made))), http.StatusFound)
}

// handleSkillPlanner is the stub for the future skill planner with
// stored plans.
// ── mining page ──────────────────────────────────────────────────────
//
// Two very different ledgers live here. The personal one
// (/characters/{id}/mining/) is per day × system × ore and covers a
// rolling window ESI documents as 30 days. The corporation one is per
// observer (a refinery) and has no dates at all beyond "last_updated" —
// it is a running total, not a diary.

// miningSeg is one character's slice of a daily bar.
type miningSeg struct {
	Name  string
	Value float64
	Color string
	H, Y  float64 // percent of the chart height
}

// miningDay is one column of the daily chart. Key is the value the
// column links to — clicking it narrows the lists to that day.
type miningDay struct {
	Day   time.Time
	Key   string
	Label string
	Total float64
	Sel   bool
	Segs  []miningSeg
}

// miningMetric is one way of measuring the same days. Units, volume and
// ISK tell very different stories: an ice unit is 1000 m³ and ~150k ISK
// against a Veldspar unit's 0.1 m³ and pennies.
type miningMetric struct {
	Key    string // isk | vol | qty
	Label  string
	Unit   string
	Money  bool // format as ISK
	Total  float64
	Peak   float64
	Peak50 float64
	Days   []miningDay
}

type miningCharRow struct {
	ID       int64
	Name     string
	Color    string
	Qty      int64
	Vol      float64
	ISK      float64
	Days     int
	Systems  int
	TopOreID int64
	TopOre   string
	Last     time.Time
}

type miningOreRow struct {
	TypeID int64
	Name   string
	Qty    int64
	Vol    float64
	ISK    float64
	Price  float64 // ISK per unit, weighted by the day it was mined
	Chars  int
}

type miningSysRow struct {
	SystemID int64
	Name     string
	Qty      int64
	Vol      float64
	ISK      float64
	Chars    int
}

// moonLedgerRow is one pilot's take of one ore at one observer.
type moonLedgerRow struct {
	CharID int64
	Char   string
	TypeID int64
	Ore    string
	Qty    int64
	ISK    float64
}

type moonObserverView struct {
	ID          int64
	Name        string
	Type        string
	LastUpdated string
	Total       int64
	ISK         float64
	Rows        []moonLedgerRow
}

type moonExtractionView struct {
	Structure string
	Moon      string
	Start     time.Time
	Arrival   time.Time
	Decay     time.Time
}

type moonCorpView struct {
	Corp        corpEntry
	Note        string
	Observers   []moonObserverView
	Extractions []moonExtractionView
	Total       int64
	ISK         float64
}

// miningTotals is one bucket of the rollup.
type miningTotals struct {
	Qty   int64
	Vol   float64
	ISK   float64
	chars map[int64]bool
}

// miningCtx carries everything the rollup needs besides the rows, so
// the same aggregation runs over the whole history and over one day.
type miningCtx struct {
	chars    map[int64]sideChar
	volumes  map[int64]float64
	priceAt  func(typeID int64, day time.Time) float64
	oreNames map[int64]string
	sysNames map[int64]string
	colors   map[int64]string // filled after the first pass
}

type miningRollup struct {
	chars    []miningCharRow
	ores     []miningOreRow
	systems  []miningSysRow
	total    miningTotals
	firstDay time.Time
	byDay    map[string]map[int64]*miningTotals // day -> character
}

// aggregate rolls ledger rows up by character, ore, system and day.
func (c miningCtx) aggregate(rows []store.MiningRow) miningRollup {
	out := miningRollup{byDay: map[string]map[int64]*miningTotals{}}
	newT := func() *miningTotals { return &miningTotals{chars: map[int64]bool{}} }

	byChar := map[int64]*miningCharRow{}
	charDays := map[int64]map[string]bool{}
	charSys := map[int64]map[int64]bool{}
	charOre := map[int64]map[int64]int64{}
	byOre := map[int64]*miningTotals{}
	bySystem := map[int64]*miningTotals{}

	for _, r := range rows {
		ch, known := c.chars[r.CharacterID]
		if !known {
			continue // character removed from the cabinet
		}
		price := r.Price
		if price <= 0 {
			price = c.priceAt(r.TypeID, r.Day)
		}
		vol := float64(r.Quantity) * c.volumes[r.TypeID]
		isk := float64(r.Quantity) * price

		out.total.Qty += r.Quantity
		out.total.Vol += vol
		out.total.ISK += isk
		if out.firstDay.IsZero() || r.Day.Before(out.firstDay) {
			out.firstDay = r.Day
		}

		row := byChar[r.CharacterID]
		if row == nil {
			row = &miningCharRow{ID: ch.ID, Name: ch.Name, Color: c.colors[ch.ID]}
			byChar[r.CharacterID] = row
			charDays[r.CharacterID] = map[string]bool{}
			charSys[r.CharacterID] = map[int64]bool{}
			charOre[r.CharacterID] = map[int64]int64{}
		}
		row.Qty += r.Quantity
		row.Vol += vol
		row.ISK += isk
		if r.Day.After(row.Last) {
			row.Last = r.Day
		}
		key := r.Day.Format("2006-01-02")
		charDays[r.CharacterID][key] = true
		charSys[r.CharacterID][r.SystemID] = true
		charOre[r.CharacterID][r.TypeID] += r.Quantity

		o := byOre[r.TypeID]
		if o == nil {
			o = newT()
			byOre[r.TypeID] = o
		}
		o.Qty += r.Quantity
		o.Vol += vol
		o.ISK += isk
		o.chars[r.CharacterID] = true

		sy := bySystem[r.SystemID]
		if sy == nil {
			sy = newT()
			bySystem[r.SystemID] = sy
		}
		sy.Qty += r.Quantity
		sy.Vol += vol
		sy.ISK += isk
		sy.chars[r.CharacterID] = true

		if out.byDay[key] == nil {
			out.byDay[key] = map[int64]*miningTotals{}
		}
		d := out.byDay[key][r.CharacterID]
		if d == nil {
			d = newT()
			out.byDay[key][r.CharacterID] = d
		}
		d.Qty += r.Quantity
		d.Vol += vol
		d.ISK += isk
	}

	for id, row := range byChar {
		row.Days, row.Systems = len(charDays[id]), len(charSys[id])
		for typeID, qty := range charOre[id] {
			if row.TopOreID == 0 || qty > charOre[id][row.TopOreID] {
				row.TopOreID = typeID
			}
		}
		row.TopOre = c.oreNames[row.TopOreID]
		out.chars = append(out.chars, *row)
	}
	// ISK is the ranking that survives mixing ore with ice.
	sort.Slice(out.chars, func(i, j int) bool { return out.chars[i].ISK > out.chars[j].ISK })

	for id, a := range byOre {
		row := miningOreRow{
			TypeID: id, Name: c.oreNames[id], Qty: a.Qty, Vol: a.Vol,
			ISK: a.ISK, Chars: len(a.chars),
		}
		if a.Qty > 0 {
			row.Price = a.ISK / float64(a.Qty) // what it was worth when mined
		}
		out.ores = append(out.ores, row)
	}
	sort.Slice(out.ores, func(i, j int) bool { return out.ores[i].ISK > out.ores[j].ISK })

	for id, a := range bySystem {
		out.systems = append(out.systems, miningSysRow{
			SystemID: id, Name: c.sysNames[id], Qty: a.Qty, Vol: a.Vol,
			ISK: a.ISK, Chars: len(a.chars),
		})
	}
	sort.Slice(out.systems, func(i, j int) bool { return out.systems[i].ISK > out.systems[j].ISK })
	return out
}

// metricKey validates the ?m= parameter that keeps the chosen cut
// across day clicks.
func metricKey(v string) string {
	switch v {
	case "vol", "qty":
		return v
	}
	return "isk"
}

// miningPalette colours characters consistently across chart and tables.
var miningPalette = []string{
	"#4da3ff", "#5fd38d", "#e0b25f", "#e06c6c",
	"#b98cff", "#4dd0e1", "#f08fc0", "#8fa3bf",
}

// handleEmpireMining collects the personal ledgers of every character
// and the moon ledgers of every corporation we have access to.
func (s *Server) handleEmpireMining(w http.ResponseWriter, r *http.Request) {
	ec, stale := s.esiFor(r)
	data, _, err := s.shell(ec, 0, "")
	if err != nil {
		httpError(w, "loading characters", err)
		return
	}
	chars := empireChars(data)
	if len(chars) == 0 {
		s.render(w, "welcome", data, stale)
		return
	}
	corps, _ := data["Corporations"].([]corpEntry)

	var (
		errs    errList
		ledgers = make([][]esi.MiningEntry, len(chars))
		moons   = make([]moonCorpView, len(corps))
		prices  map[int64]esi.MarketPrice
		wg      sync.WaitGroup
	)
	wg.Add(1)
	go func() {
		defer wg.Done()
		prices, _ = s.ESI.MarketPrices()
	}()
	for i, ch := range chars {
		wg.Add(1)
		go func(i int, ch sideChar) {
			defer wg.Done()
			entries, err := ec.MiningLedger(ch.ID)
			if err != nil {
				errs.add(ch.Name+": добыча", err)
				return
			}
			ledgers[i] = entries
		}(i, ch)
	}
	for i, corp := range corps {
		wg.Add(1)
		go func(i int, corp corpEntry) {
			defer wg.Done()
			moons[i] = s.moonView(ec, corp)
		}(i, corp)
	}
	wg.Wait()

	// ── prices: Jita average OF THE DAY the ore was mined ──
	// Units are not comparable across ore and ice (a Veldspar unit is
	// 0.1 m³ and pennies, a Glacial Mass unit is 1000 m³ and ~150k ISK),
	// so ISK is the only honest common scale — and it has to be the
	// price of that day, not today's.
	typeSet := map[int64]bool{}
	for _, entries := range ledgers {
		for _, e := range entries {
			typeSet[e.TypeID] = true
		}
	}
	stored, err := s.Store.MiningRows(time.Time{})
	if err != nil {
		errs.add("история добычи", err)
	}
	for _, r := range stored {
		typeSet[r.TypeID] = true
	}
	typeIDs := make([]int64, 0, len(typeSet))
	for id := range typeSet {
		typeIDs = append(typeIDs, id)
	}
	sort.Slice(typeIDs, func(i, j int) bool { return typeIDs[i] < typeIDs[j] })

	history := make([]esi.PriceSeries, len(typeIDs))
	var hwg sync.WaitGroup
	for i, id := range typeIDs {
		hwg.Add(1)
		go func(i int, id int64) {
			defer hwg.Done()
			h, err := ec.JitaHistory(id)
			if err != nil {
				return // fall back to the estimated price below
			}
			history[i] = h
		}(i, id)
	}
	hwg.Wait()
	series := map[int64]esi.PriceSeries{}
	for i, id := range typeIDs {
		series[id] = history[i]
	}
	// priceAt: Jita history first, the client's estimated price second.
	priceAt := func(typeID int64, day time.Time) float64 {
		if p := series[typeID].At(day); p > 0 {
			return p
		}
		return prices[typeID].Average
	}

	// ── persist: ESI keeps ~30 days, we keep everything ──
	var fresh []store.MiningRow
	for i, ch := range chars {
		for _, e := range ledgers[i] {
			fresh = append(fresh, store.MiningRow{
				CharacterID: ch.ID, Day: e.Date, SystemID: e.SolarSystemID,
				TypeID: e.TypeID, Quantity: e.Quantity, Price: priceAt(e.TypeID, e.Date),
			})
		}
	}
	if err := s.Store.SaveMiningRows(fresh); err != nil {
		errs.add("сохранение добычи", err)
	}
	// Re-read so the page shows the union of the ESI window and history.
	rows, err := s.Store.MiningRows(time.Time{})
	if err != nil {
		errs.add("история добычи", err)
		rows = fresh
	}

	// ── aggregate over the stored ledger ──
	volumes := map[int64]float64{}
	if s.SDE.Available() {
		volumes = s.SDE.Volumes(typeIDs)
	}
	charByID := map[int64]sideChar{}
	for _, ch := range chars {
		charByID[ch.ID] = ch
	}

	ctx := miningCtx{
		chars:    charByID,
		volumes:  volumes,
		priceAt:  priceAt,
		oreNames: s.typeNames(typeIDs),
	}
	// System names need the full id set, whatever day is selected.
	var systemIDs []int64
	seenSys := map[int64]bool{}
	for _, r := range rows {
		if !seenSys[r.SystemID] {
			seenSys[r.SystemID] = true
			systemIDs = append(systemIDs, r.SystemID)
		}
	}
	ctx.sysNames = ec.Names(systemIDs)

	all := ctx.aggregate(rows)
	grand, firstDay, byDay := all.total, all.firstDay, all.byDay
	// Colours are assigned over the whole history, so a pilot keeps the
	// same colour when a single day is selected.
	charRows := all.chars
	ctx.colors = map[int64]string{}
	for i := range charRows {
		charRows[i].Color = miningPalette[i%len(miningPalette)]
		ctx.colors[charRows[i].ID] = charRows[i].Color
	}

	// ── day filter: a click on a chart column narrows the lists ──
	selected := r.URL.Query().Get("day")
	shown := all
	if selected != "" {
		var picked []store.MiningRow
		for _, row := range rows {
			if row.Day.Format("2006-01-02") == selected {
				picked = append(picked, row)
			}
		}
		if len(picked) == 0 {
			selected = "" // stale link — fall back to everything
		} else {
			shown = ctx.aggregate(picked)
			for i := range shown.chars {
				shown.chars[i].Color = ctx.colors[shown.chars[i].ID]
			}
		}
	}
	tableChars, ores, systems := shown.chars, shown.ores, shown.systems

	// ── three charts over one continuous daily axis ──
	now := time.Now()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	if firstDay.IsZero() {
		firstDay = today
	}
	if today.Sub(firstDay) > 120*24*time.Hour {
		firstDay = today.AddDate(0, 0, -120)
	}
	metrics := []miningMetric{
		// iskShort already prints the "ISK" suffix — no Unit here.
		{Key: "isk", Label: "ISK по цене дня", Total: grand.ISK, Money: true},
		{Key: "vol", Label: "Объём", Unit: "м³", Total: grand.Vol},
		{Key: "qty", Label: "Количество", Unit: "ед.", Total: float64(grand.Qty)},
	}
	for mi := range metrics {
		m := &metrics[mi]
		for d := firstDay; !d.After(today); d = d.AddDate(0, 0, 1) {
			key := d.Format("2006-01-02")
			md := miningDay{Day: d, Key: key, Label: d.Format("02.01"), Sel: key == selected}
			for _, c := range charRows {
				a := byDay[key][c.ID]
				if a == nil {
					continue
				}
				var v float64
				switch m.Key {
				case "isk":
					v = a.ISK
				case "vol":
					v = a.Vol
				default:
					v = float64(a.Qty)
				}
				if v <= 0 {
					continue
				}
				md.Total += v
				md.Segs = append(md.Segs, miningSeg{Name: c.Name, Value: v, Color: c.Color})
			}
			if md.Total > m.Peak {
				m.Peak = md.Total
			}
			m.Days = append(m.Days, md)
		}
		for i := range m.Days {
			var y float64
			for j := range m.Days[i].Segs {
				seg := &m.Days[i].Segs[j]
				if m.Peak > 0 {
					seg.H = seg.Value / m.Peak * 100
				}
				seg.Y = y
				y += seg.H
			}
		}
		m.Peak50 = m.Peak / 2
	}

	// ── moon totals ──
	var moonQty int64
	var moonISK float64
	for _, m := range moons {
		moonQty += m.Total
		moonISK += m.ISK
	}
	// Corporations with nothing are kept on the page on purpose: the
	// reader has to see that they were checked, not guess.
	sort.SliceStable(moons, func(i, j int) bool { return moons[i].Total > moons[j].Total })

	data["Chars"] = tableChars
	data["Ores"] = ores
	data["Systems"] = systems
	data["Metrics"] = metrics
	data["Metric"] = metricKey(r.URL.Query().Get("m"))
	data["Day"] = selected
	if selected != "" {
		if d, err := time.Parse("2006-01-02", selected); err == nil {
			data["DayLabel"] = d.Format("02.01.2006")
		}
		data["DayQty"] = shown.total.Qty
		data["DayVol"] = shown.total.Vol
		data["DayISK"] = shown.total.ISK
	}
	data["GrandQty"] = grand.Qty
	data["GrandVol"] = grand.Vol
	data["GrandISK"] = grand.ISK
	data["FirstDay"] = firstDay
	data["Stored"] = len(rows)
	data["Moons"] = moons
	data["MoonQty"] = moonQty
	data["MoonISK"] = moonISK
	data["Now"] = now
	data["Errors"] = errs.list
	s.render(w, "mining", data, stale)
}

// typeNames prefers the local SDE (localized) and falls back to ESI.
func (s *Server) typeNames(ids []int64) map[int64]string {
	out := map[int64]string{}
	if s.SDE.Available() {
		out = s.SDE.TypeNames(ids)
	}
	var missing []int64
	for _, id := range ids {
		if out[id] == "" {
			missing = append(missing, id)
		}
	}
	if len(missing) > 0 {
		for id, n := range s.ESI.Names(missing) {
			out[id] = n
		}
	}
	return out
}

// moonView reads one corporation's moon ledgers. Missing roles are the
// normal case, not an error worth shouting about.
func (s *Server) moonView(ec *esi.Client, corp corpEntry) moonCorpView {
	v := moonCorpView{Corp: corp}
	observers, err := ec.CorporationObservers(corp.ViaCharID, corp.ID)
	if err != nil {
		if strings.Contains(err.Error(), "required role") {
			v.Note = "нет роли Accountant/Director — корпоративный леджер закрыт"
		} else {
			v.Note = err.Error()
		}
		return v
	}

	locNames := ec.LocationNames(corp.ViaCharID, observerIDs(observers))
	for _, o := range observers {
		ov := moonObserverView{
			ID: o.ObserverID, Type: o.ObserverType,
			LastUpdated: o.LastUpdated, Name: locNames[o.ObserverID],
		}
		records, err := ec.ObserverLedger(corp.ViaCharID, corp.ID, o.ObserverID)
		if err != nil {
			v.Note = err.Error()
			v.Observers = append(v.Observers, ov)
			continue
		}
		prices, _ := s.ESI.MarketPrices()
		var typeIDs, charIDs []int64
		for _, rec := range records {
			typeIDs = append(typeIDs, rec.TypeID)
			charIDs = append(charIDs, rec.CharacterID)
		}
		oreNames := s.typeNames(typeIDs)
		charNames := ec.Names(charIDs)
		for _, rec := range records {
			isk := float64(rec.Quantity) * prices[rec.TypeID].Average
			ov.Rows = append(ov.Rows, moonLedgerRow{
				CharID: rec.CharacterID, Char: charNames[rec.CharacterID],
				TypeID: rec.TypeID, Ore: oreNames[rec.TypeID],
				Qty: rec.Quantity, ISK: isk,
			})
			ov.Total += rec.Quantity
			ov.ISK += isk
		}
		sort.Slice(ov.Rows, func(i, j int) bool { return ov.Rows[i].Qty > ov.Rows[j].Qty })
		v.Total += ov.Total
		v.ISK += ov.ISK
		v.Observers = append(v.Observers, ov)
	}
	sort.Slice(v.Observers, func(i, j int) bool { return v.Observers[i].Total > v.Observers[j].Total })

	extractions, err := ec.MoonExtractions(corp.ViaCharID, corp.ID)
	if err == nil {
		structIDs := make([]int64, 0, len(extractions))
		for _, e := range extractions {
			structIDs = append(structIDs, e.StructureID)
		}
		structNames := ec.LocationNames(corp.ViaCharID, structIDs)
		for _, e := range extractions {
			v.Extractions = append(v.Extractions, moonExtractionView{
				Structure: structNames[e.StructureID],
				// moons never resolve through the names batch
				Moon:    s.ESI.MoonName(e.MoonID),
				Start:   e.ExtractionStart,
				Arrival: e.ChunkArrival,
				Decay:   e.NaturalDecay,
			})
		}
		sort.Slice(v.Extractions, func(i, j int) bool {
			return v.Extractions[i].Arrival.Before(v.Extractions[j].Arrival)
		})
	}
	return v
}

func observerIDs(list []esi.MiningObserver) []int64 {
	out := make([]int64, 0, len(list))
	for _, o := range list {
		out = append(out, o.ObserverID)
	}
	return out
}

// ── fleet tool ───────────────────────────────────────────────────────

// fleetCharRef is one of our characters together with the fleet they
// turned out to be in (nil when they are in none).
type fleetCharRef struct {
	ch  sideChar
	ref *esi.FleetRef
	err string
}

// fleetMemberView is one fleet member with everything resolved.
type fleetMemberView struct {
	esi.FleetMember
	Name    string
	Ship    string
	System  string
	Station string
	Ours    bool // one of our authorized characters
	Boss    bool
	Docked  bool
}

type fleetSquadView struct {
	ID      int64
	WingID  int64
	Name    string
	Members []fleetMemberView
}

type fleetWingView struct {
	ID      int64
	Name    string
	Squads  []fleetSquadView
	Command []fleetMemberView // wing commander (squad_id = -1)
	Members int
}

// fleetProbe is the access experiment: what each of our characters in
// the fleet can actually read. Everyone but the boss is answered 404.
type fleetProbe struct {
	ID     int64
	Name   string
	Role   string
	RoleRu string
	Boss   bool
	OK     bool
	Note   string
}

type fleetShipRow struct {
	TypeID int64
	Name   string
	Count  int
}

// fleetTarget is a destination for the "move member" picker; Key encodes
// the role together with the wing/squad ids.
type fleetTarget struct {
	Key   string
	Label string
}

type fleetOutsider struct {
	ID   int64
	Name string
}

type fleetView struct {
	FleetID  int64
	ViaID    int64 // character whose token drives the /fleets/ calls
	ViaName  string
	BossID   int64
	BossName string
	BossOurs bool
	Settings esi.FleetSettings
	Wings    []fleetWingView
	Loose    []fleetMemberView // wing_id = -1: the fleet commander
	Probes   []fleetProbe
	Ships    []fleetShipRow
	Targets  []fleetTarget
	Members  int
	Systems  int
	Docked   int
	Squads   int
	Activity []fleetActivity
	Events   []fleetEventView
	Err      string
	RawSet   string
	RawMem   string
	RawWings string
}

var fleetRoleRu = map[string]string{
	esi.RoleFleetCommander: "командир флота",
	esi.RoleWingCommander:  "командир эскадры",
	esi.RoleSquadCommander: "командир эскадрильи",
	esi.RoleSquadMember:    "боец",
}

// fleetRolePrio orders members inside a squad the way the game does.
var fleetRolePrio = map[string]int{
	esi.RoleFleetCommander: 0,
	esi.RoleWingCommander:  1,
	esi.RoleSquadCommander: 2,
	esi.RoleSquadMember:    3,
}

// handleFleetTool shows every fleet our characters are currently in.
// Nothing here goes through the cache: fleet data is five seconds old at
// best, and a stale composition is worse than none.
func (s *Server) handleFleetTool(w http.ResponseWriter, r *http.Request) {
	ec, stale := s.esiFor(r)
	data, _, err := s.shell(ec, 0, "")
	if err != nil {
		httpError(w, "loading characters", err)
		return
	}
	chars := empireChars(data)

	// Step one: who is in a fleet at all (one live call per character).
	rows := make([]fleetCharRef, len(chars))
	var wg sync.WaitGroup
	for i, ch := range chars {
		wg.Add(1)
		go func(i int, ch sideChar) {
			defer wg.Done()
			rows[i] = fleetCharRef{ch: ch}
			ref, ok, err := s.ESI.CharacterFleet(ch.ID)
			if err != nil {
				rows[i].err = err.Error()
				return
			}
			if ok {
				rows[i].ref = ref
			}
		}(i, ch)
	}
	wg.Wait()

	// Group by fleet; prefer the boss as the character we call through.
	order := []int64{}
	byFleet := map[int64][]fleetCharRef{}
	var idle, failed []string
	for _, row := range rows {
		switch {
		case row.err != "":
			failed = append(failed, row.ch.Name+": "+row.err)
		case row.ref == nil:
			idle = append(idle, row.ch.Name)
		default:
			id := row.ref.FleetID
			if _, seen := byFleet[id]; !seen {
				order = append(order, id)
			}
			byFleet[id] = append(byFleet[id], row)
		}
	}

	views := make([]fleetView, len(order))
	var wg2 sync.WaitGroup
	for i, id := range order {
		wg2.Add(1)
		go func(i int, id int64) {
			defer wg2.Done()
			views[i] = s.fleetView(id, byFleet[id])
		}(i, id)
	}
	wg2.Wait()

	// Characters not in any fleet can be invited straight from the page.
	inFleet := map[int64]bool{}
	for _, list := range byFleet {
		for _, row := range list {
			inFleet[row.ch.ID] = true
		}
	}
	var outside []fleetOutsider
	for _, ch := range chars {
		if !inFleet[ch.ID] {
			outside = append(outside, fleetOutsider{ID: ch.ID, Name: ch.Name})
		}
	}

	data["Fleets"] = views
	data["Idle"] = idle
	data["Outside"] = outside
	data["Errors"] = failed
	data["Now"] = time.Now()
	s.render(w, "fleet", data, stale)
}

// fleetView reads one fleet and turns it into the page model. The read
// is attempted through the boss first, then through anyone else of ours
// in the fleet — that fallback is also the access experiment.
func (s *Server) fleetView(fleetID int64, ours []fleetCharRef) fleetView {
	v := fleetView{FleetID: fleetID}
	if len(ours) > 0 {
		v.BossID = ours[0].ref.BossID
	}

	// Probe every character of ours: who can read the fleet at all.
	v.Probes = make([]fleetProbe, len(ours))
	var wg sync.WaitGroup
	for i, row := range ours {
		wg.Add(1)
		go func(i int, row fleetCharRef) {
			defer wg.Done()
			p := fleetProbe{
				ID: row.ch.ID, Name: row.ch.Name, Role: row.ref.Role,
				RoleRu: fleetRoleRu[row.ref.Role], Boss: row.ch.ID == row.ref.BossID,
			}
			if _, err := s.ESI.FleetState(row.ch.ID, fleetID); err != nil {
				p.Note = err.Error()
			} else {
				p.OK, p.Note = true, "полный доступ на чтение и запись"
			}
			v.Probes[i] = p
		}(i, row)
	}
	wg.Wait()

	sort.SliceStable(v.Probes, func(i, j int) bool { return v.Probes[i].Boss && !v.Probes[j].Boss })
	for _, p := range v.Probes {
		if p.Boss {
			v.BossOurs, v.BossName = true, p.Name
		}
	}

	// Read through the first character that has access.
	var fleet *esi.Fleet
	for _, p := range v.Probes {
		if !p.OK {
			continue
		}
		f, err := s.ESI.FleetOf(p.ID, fleetID)
		if err != nil {
			v.Err = err.Error()
			continue
		}
		fleet, v.ViaID, v.ViaName = f, p.ID, p.Name
		break
	}
	if fleet == nil {
		if v.Err == "" && len(v.Probes) > 0 {
			v.Err = "ни один наш персонаж не видит этот флот: " + v.Probes[0].Note
		}
		return v
	}
	v.Err = ""
	v.Settings = fleet.Settings
	v.RawSet = prettyJSON(fleet.RawSettings)
	v.RawMem = prettyJSON(fleet.RawMembers)
	v.RawWings = prettyJSON(fleet.RawWings)

	// Resolve names: characters, ships, systems, stations.
	ids := []int64{}
	for _, m := range fleet.Members {
		ids = append(ids, m.CharacterID, m.ShipTypeID, m.SolarSystemID)
		if m.StationID != 0 {
			ids = append(ids, m.StationID)
		}
	}
	if v.BossID != 0 {
		ids = append(ids, v.BossID)
	}
	names := s.ESI.Names(ids)
	if v.BossName == "" {
		v.BossName = names[v.BossID]
	}

	mine := map[int64]bool{}
	for _, p := range v.Probes {
		mine[p.ID] = true
	}

	systems := map[int64]bool{}
	ships := map[int64]*fleetShipRow{}
	members := make([]fleetMemberView, 0, len(fleet.Members))
	for _, m := range fleet.Members {
		mv := fleetMemberView{
			FleetMember: m,
			Name:        names[m.CharacterID],
			Ship:        names[m.ShipTypeID],
			System:      names[m.SolarSystemID],
			Station:     names[m.StationID],
			Ours:        mine[m.CharacterID],
			Boss:        m.CharacterID == v.BossID,
			Docked:      m.StationID != 0,
		}
		if mv.Name == "" {
			mv.Name = fmt.Sprintf("ID %d", m.CharacterID)
		}
		if mv.Docked {
			v.Docked++
		}
		systems[m.SolarSystemID] = true
		sh := ships[m.ShipTypeID]
		if sh == nil {
			sh = &fleetShipRow{TypeID: m.ShipTypeID, Name: names[m.ShipTypeID]}
			ships[m.ShipTypeID] = sh
		}
		sh.Count++
		members = append(members, mv)
	}
	v.Members, v.Systems = len(members), len(systems)

	for _, sh := range ships {
		v.Ships = append(v.Ships, *sh)
	}
	sort.Slice(v.Ships, func(i, j int) bool {
		if v.Ships[i].Count != v.Ships[j].Count {
			return v.Ships[i].Count > v.Ships[j].Count
		}
		return v.Ships[i].Name < v.Ships[j].Name
	})

	// Hang members on the wing/squad tree. wing_id/squad_id are -1 for
	// the fleet commander and for a wing commander's squad slot.
	pick := func(filter func(fleetMemberView) bool) []fleetMemberView {
		var out []fleetMemberView
		for _, m := range members {
			if filter(m) {
				out = append(out, m)
			}
		}
		sort.SliceStable(out, func(i, j int) bool {
			if fleetRolePrio[out[i].Role] != fleetRolePrio[out[j].Role] {
				return fleetRolePrio[out[i].Role] < fleetRolePrio[out[j].Role]
			}
			return out[i].Name < out[j].Name
		})
		return out
	}

	v.Loose = pick(func(m fleetMemberView) bool { return m.WingID <= 0 })
	v.Targets = append(v.Targets, fleetTarget{Key: "fc", Label: "Командир флота"})
	for _, wing := range fleet.Wings {
		wv := fleetWingView{ID: wing.ID, Name: wing.Name}
		wv.Command = pick(func(m fleetMemberView) bool {
			return m.WingID == wing.ID && m.SquadID <= 0
		})
		wv.Members = len(wv.Command)
		v.Targets = append(v.Targets, fleetTarget{
			Key: fmt.Sprintf("wc:%d", wing.ID), Label: wing.Name + " · командир эскадры"})
		for _, sq := range wing.Squads {
			sv := fleetSquadView{ID: sq.ID, WingID: wing.ID, Name: sq.Name}
			sv.Members = pick(func(m fleetMemberView) bool { return m.SquadID == sq.ID })
			wv.Members += len(sv.Members)
			wv.Squads = append(wv.Squads, sv)
			v.Squads++
			v.Targets = append(v.Targets,
				fleetTarget{Key: fmt.Sprintf("sc:%d:%d", wing.ID, sq.ID),
					Label: wing.Name + " / " + sq.Name + " · командир"},
				fleetTarget{Key: fmt.Sprintf("sm:%d:%d", wing.ID, sq.ID),
					Label: wing.Name + " / " + sq.Name})
		}
		v.Wings = append(v.Wings, wv)
	}

	// Telemetry: this reading becomes history.
	now := time.Now()
	s.recordFleet(fleetID, members, now)
	v.Activity, v.Events = s.fleetHistory(fleetID, members, fleet.Wings, now)
	return v
}

// ── fleet telemetry ──────────────────────────────────────────────────
//
// ESI shows only the CURRENT state of a fleet — no history of any kind.
// Every reading of /fleets/{id}/members/ is therefore diffed against the
// previous one and the differences are kept in our own database. With
// the page's 6-second auto-refresh on, that turns the members endpoint
// into an activity log: who joined, who swapped hulls, who jumped, and
// every docking run.

// fleetEventView is one recorded change, rendered.
type fleetEventView struct {
	At   time.Time
	Char string
	Kind string
	Text string
}

// fleetActivity aggregates one pilot's recorded history in this fleet.
type fleetActivity struct {
	CharID int64
	Name   string
	Ship   string
	InDock bool
	Left   bool
	Joined time.Time
	Trips  int // dockings — a mining fleet's unload runs
	Swaps  int // ship changes
	Jumps  int // system changes
	Docked time.Duration
}

// recordFleet diffs a reading against the stored composition, appends
// what changed and stores the new state.
func (s *Server) recordFleet(fleetID int64, members []fleetMemberView, now time.Time) {
	prev, err := s.Store.FleetStates(fleetID)
	if err != nil {
		log.Printf("fleet telemetry: %v", err)
		return
	}

	var (
		states []store.FleetMemberState
		events []store.FleetEvent
		seen   = map[int64]bool{}
	)
	add := func(charID int64, at time.Time, kind string, from, to int64, text string) {
		events = append(events, store.FleetEvent{
			CharacterID: charID, At: at, Kind: kind, FromID: from, ToID: to, Text: text,
		})
	}

	for _, m := range members {
		seen[m.CharacterID] = true
		cur := store.FleetMemberState{
			CharacterID: m.CharacterID, ShipTypeID: m.ShipTypeID,
			SystemID: m.SolarSystemID, StationID: m.StationID,
			WingID: m.WingID, SquadID: m.SquadID, Role: m.Role,
			JoinedAt: m.JoinTime, SeenAt: now,
		}
		states = append(states, cur)

		old, known := prev[m.CharacterID]
		if !known {
			// join_time is exact, so the event gets the real moment
			// rather than "whenever we happened to look".
			add(m.CharacterID, m.JoinTime, "join", 0, m.ShipTypeID, "")
			if m.StationID != 0 {
				add(m.CharacterID, now, "dock", 0, m.StationID, "")
			}
			continue
		}
		if old.ShipTypeID != cur.ShipTypeID {
			add(m.CharacterID, now, "ship", old.ShipTypeID, cur.ShipTypeID, "")
		}
		if old.SystemID != cur.SystemID {
			add(m.CharacterID, now, "system", old.SystemID, cur.SystemID, "")
		}
		switch {
		case old.StationID == 0 && cur.StationID != 0:
			add(m.CharacterID, now, "dock", 0, cur.StationID, "")
		case old.StationID != 0 && cur.StationID == 0:
			add(m.CharacterID, now, "undock", old.StationID, 0, "")
		}
		if old.Role != cur.Role {
			add(m.CharacterID, now, "role", 0, 0,
				fleetRoleRu[old.Role]+" → "+fleetRoleRu[cur.Role])
		}
		if old.SquadID != cur.SquadID || old.WingID != cur.WingID {
			add(m.CharacterID, now, "squad", old.SquadID, cur.SquadID, "")
		}
	}

	var gone []int64
	for id := range prev {
		if !seen[id] {
			gone = append(gone, id)
			add(id, now, "leave", 0, 0, "")
		}
	}
	sort.Slice(gone, func(i, j int) bool { return gone[i] < gone[j] })

	if err := s.Store.SaveFleetSnapshot(fleetID, states, gone, events); err != nil {
		log.Printf("fleet telemetry: %v", err)
	}
}

// fleetHistory renders the recorded events and rolls them up per pilot.
func (s *Server) fleetHistory(fleetID int64, members []fleetMemberView,
	wings []esi.FleetWing, now time.Time) ([]fleetActivity, []fleetEventView) {

	events, err := s.Store.FleetEvents(fleetID, 500)
	if err != nil || len(events) == 0 {
		return nil, nil
	}

	ids := make([]int64, 0, len(events)*2)
	for _, e := range events {
		ids = append(ids, e.CharacterID)
		switch e.Kind {
		case "ship", "system", "dock", "undock", "join":
			ids = append(ids, e.FromID, e.ToID)
		}
	}
	names := s.ESI.Names(ids)
	squads := map[int64]string{}
	for _, w := range wings {
		for _, sq := range w.Squads {
			squads[sq.ID] = w.Name + " / " + sq.Name
		}
	}
	squadName := func(id int64) string {
		if id <= 0 {
			return "вне эскадрильи"
		}
		if n, ok := squads[id]; ok {
			return n
		}
		return fmt.Sprintf("эск. %d", id)
	}

	// Oldest first for the rollup; the feed keeps ESI's newest-first.
	type agg struct {
		fleetActivity
		dockedSince time.Time
	}
	byChar := map[int64]*agg{}
	get := func(id int64) *agg {
		a := byChar[id]
		if a == nil {
			a = &agg{}
			a.CharID, a.Name = id, names[id]
			byChar[id] = a
		}
		return a
	}
	for i := len(events) - 1; i >= 0; i-- {
		e := events[i]
		a := get(e.CharacterID)
		switch e.Kind {
		case "join":
			a.Joined, a.Left = e.At, false
		case "leave":
			a.Left = true
			if !a.dockedSince.IsZero() {
				a.Docked += e.At.Sub(a.dockedSince)
				a.dockedSince = time.Time{}
			}
		case "ship":
			a.Swaps++
		case "system":
			a.Jumps++
		case "dock":
			a.Trips++
			a.dockedSince = e.At
		case "undock":
			if !a.dockedSince.IsZero() {
				a.Docked += e.At.Sub(a.dockedSince)
				a.dockedSince = time.Time{}
			}
		}
	}

	// Current membership wins over the recorded tail.
	present := map[int64]fleetMemberView{}
	for _, m := range members {
		present[m.CharacterID] = m
	}
	rows := make([]fleetActivity, 0, len(byChar))
	for id, a := range byChar {
		act := a.fleetActivity
		if m, ok := present[id]; ok {
			act.Left, act.Ship, act.InDock = false, m.Ship, m.Docked
			act.Name = m.Name
			if act.Joined.IsZero() {
				act.Joined = m.JoinTime
			}
			if !a.dockedSince.IsZero() {
				act.Docked += now.Sub(a.dockedSince)
			}
		} else {
			act.Left = true
		}
		if act.Name == "" {
			act.Name = fmt.Sprintf("ID %d", id)
		}
		rows = append(rows, act)
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Left != rows[j].Left {
			return !rows[i].Left
		}
		return rows[i].Name < rows[j].Name
	})

	feed := make([]fleetEventView, 0, len(events))
	for _, e := range events {
		who := names[e.CharacterID]
		if who == "" {
			who = fmt.Sprintf("ID %d", e.CharacterID)
		}
		var text string
		switch e.Kind {
		case "join":
			text = "вошёл во флот"
			if n := names[e.ToID]; n != "" {
				text += " на " + n
			}
		case "leave":
			text = "вышел из флота"
		case "ship":
			text = "сменил корабль: " + names[e.FromID] + " → " + names[e.ToID]
		case "system":
			text = "перешёл: " + names[e.FromID] + " → " + names[e.ToID]
		case "dock":
			text = "пристыковался"
			if n := names[e.ToID]; n != "" {
				text += ": " + n
			}
		case "undock":
			text = "отстыковался"
			if n := names[e.FromID]; n != "" {
				text += ": " + n
			}
		case "role":
			text = "роль: " + e.Text
		case "squad":
			text = "переведён: " + squadName(e.FromID) + " → " + squadName(e.ToID)
		default:
			text = e.Kind
		}
		feed = append(feed, fleetEventView{At: e.At, Char: who, Kind: e.Kind, Text: text})
	}
	return rows, feed
}

// parseFleetTarget decodes a picker key into the ESI placement triple.
func parseFleetTarget(key string) (role string, wingID, squadID int64, err error) {
	parts := strings.Split(key, ":")
	num := func(i int) int64 {
		if i >= len(parts) {
			return 0
		}
		n, _ := strconv.ParseInt(parts[i], 10, 64)
		return n
	}
	switch parts[0] {
	case "fc":
		return esi.RoleFleetCommander, 0, 0, nil
	case "wc":
		return esi.RoleWingCommander, num(1), 0, nil
	case "sc":
		return esi.RoleSquadCommander, num(1), num(2), nil
	case "sm":
		return esi.RoleSquadMember, num(1), num(2), nil
	}
	return "", 0, 0, fmt.Errorf("неизвестное назначение %q", key)
}

// handleFleetAction performs one write against the fleet API. Every
// button on the page funnels through here; the browser reloads after.
func (s *Server) handleFleetAction(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Op     string `json:"op"`
		Via    int64  `json:"via"`
		Fleet  int64  `json:"fleet"`
		Member int64  `json:"member"`
		Wing   int64  `json:"wing"`
		Squad  int64  `json:"squad"`
		Target string `json:"target"`
		Name   string `json:"name"`
		MOTD   string `json:"motd"`
		Free   *bool  `json:"free"`
		Char   string `json:"char"`
		CharID int64  `json:"char_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	if req.Via == 0 || req.Fleet == 0 {
		http.Error(w, "via and fleet required", http.StatusBadRequest)
		return
	}

	fail := func(err error) {
		writeJSON(w, map[string]any{"error": err.Error()})
	}
	var (
		err  error
		note string
	)
	switch req.Op {
	case "settings":
		var motd *string
		if req.MOTD != "" {
			motd = &req.MOTD
		}
		err = s.ESI.FleetUpdate(req.Via, req.Fleet, motd, req.Free)
	case "move":
		role, wing, squad, perr := parseFleetTarget(req.Target)
		if perr != nil {
			fail(perr)
			return
		}
		err = s.ESI.FleetMove(req.Via, req.Fleet, req.Member, role, wing, squad)
	case "kick":
		err = s.ESI.FleetKick(req.Via, req.Fleet, req.Member)
	case "invite":
		role, wing, squad, perr := parseFleetTarget(req.Target)
		if perr != nil {
			fail(perr)
			return
		}
		id := req.CharID
		if id == 0 {
			if id, err = s.ESI.ResolveCharacter(strings.TrimSpace(req.Char)); err != nil {
				fail(err)
				return
			}
		}
		err = s.ESI.FleetInvite(req.Via, req.Fleet, id, role, wing, squad)
	case "wing-create":
		var id int64
		if id, err = s.ESI.FleetCreateWing(req.Via, req.Fleet); err == nil {
			note = fmt.Sprintf("крыло %d создано", id)
		}
	case "wing-rename":
		err = s.ESI.FleetRenameWing(req.Via, req.Fleet, req.Wing, req.Name)
	case "wing-delete":
		err = s.ESI.FleetDeleteWing(req.Via, req.Fleet, req.Wing)
	case "squad-create":
		var id int64
		if id, err = s.ESI.FleetCreateSquad(req.Via, req.Fleet, req.Wing); err == nil {
			note = fmt.Sprintf("эскадрилья %d создана", id)
		}
	case "squad-rename":
		err = s.ESI.FleetRenameSquad(req.Via, req.Fleet, req.Squad, req.Name)
	case "squad-delete":
		err = s.ESI.FleetDeleteSquad(req.Via, req.Fleet, req.Squad)
	default:
		fail(fmt.Errorf("неизвестная операция %q", req.Op))
		return
	}
	if err != nil {
		fail(err)
		return
	}
	writeJSON(w, map[string]any{"ok": true, "note": note})
}

// ── empire-wide pages ────────────────────────────────────────────────

// empireChars flattens the sidebar groups into one plain list, keeping
// the account order the sidebar shows.
func empireChars(data map[string]any) []sideChar {
	groups, _ := data["Groups"].([]accountGroup)
	var out []sideChar
	for _, g := range groups {
		out = append(out, g.Chars...)
	}
	return out
}

// trainingOrder sorts characters by how soon the skill they are
// training finishes; idle queues sink to the bottom in sidebar order.
func trainingOrder(chars []sideChar) []sideChar {
	out := append([]sideChar(nil), chars...)
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if (a.QueueSkillID != 0) != (b.QueueSkillID != 0) {
			return a.QueueSkillID != 0
		}
		if a.QueueSkillID == 0 {
			return false
		}
		return a.QueueEnds.Before(b.QueueEnds)
	})
	return out
}

// corpWallets fetches every corporation's wallet in parallel. Divisions
// are only worth resolving where they are actually shown (the wallets
// tab) — the name lookup needs the Director role on top of Accountant.
func (s *Server) corpWallets(ec *esi.Client, corps []corpEntry, divisions bool) []corpWalletRow {
	rows := make([]corpWalletRow, len(corps))
	var wg sync.WaitGroup
	for i, corp := range corps {
		wg.Add(1)
		go func(i int, corp corpEntry) {
			defer wg.Done()
			row := corpWalletRow{Corp: corp}
			wallets, err := ec.CorporationWallets(corp.ViaCharID, corp.ID)
			if err != nil {
				if strings.Contains(err.Error(), "403") {
					row.Note = "нет доступа (Accountant)"
				} else {
					row.Note = "н/д"
				}
				rows[i] = row
				return
			}
			row.OK = true
			var names map[int]string
			if divisions {
				names, _ = ec.CorporationWalletNames(corp.ViaCharID, corp.ID)
			}
			for _, wl := range wallets {
				row.Total += wl.Balance
				// Empty divisions are noise — most corps use one.
				if !divisions || wl.Balance == 0 {
					continue
				}
				name := names[wl.Division]
				if name == "" {
					if wl.Division == 1 {
						name = "Главный счёт"
					} else {
						name = fmt.Sprintf("Счёт №%d", wl.Division)
					}
				}
				row.Divisions = append(row.Divisions, corpDivision{
					Division: wl.Division, Name: name, Balance: wl.Balance,
				})
			}
			if len(row.Divisions) < 2 {
				row.Divisions = nil // a single division just repeats the total
			}
			rows[i] = row
		}(i, corp)
	}
	wg.Wait()
	return rows
}

// handleIndex renders the empire summary (or the welcome page while
// no characters are added yet).
func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	ec, stale := s.esiFor(r)
	data, _, err := s.shell(ec, 0, "")
	if err != nil {
		httpError(w, "loading characters", err)
		return
	}
	chars := empireChars(data)
	if len(chars) == 0 {
		s.render(w, "welcome", data, stale)
		return
	}

	// Personal wallets are already gathered by the sidebar pass; the
	// list under the "Персонажи всего" row goes richest first.
	var charTotal float64
	for _, ch := range chars {
		charTotal += ch.Wallet
	}
	byWallet := append([]sideChar(nil), chars...)
	sort.SliceStable(byWallet, func(i, j int) bool { return byWallet[i].Wallet > byWallet[j].Wallet })

	corps, _ := data["Corporations"].([]corpEntry)
	corpRows := s.corpWallets(ec, corps, false)
	var corpTotal float64
	for _, row := range corpRows {
		corpTotal += row.Total
	}

	// Industry lines: the per-character rows plus an empire total.
	var lines lineStats
	for _, ch := range chars {
		lines.MfgBusy += ch.Lines.MfgBusy
		lines.MfgTotal += ch.Lines.MfgTotal
		lines.SciBusy += ch.Lines.SciBusy
		lines.SciTotal += ch.Lines.SciTotal
		lines.ReaBusy += ch.Lines.ReaBusy
		lines.ReaTotal += ch.Lines.ReaTotal
	}

	idle := 0
	for _, ch := range chars {
		if ch.QueueSkillID == 0 {
			idle++
		}
	}

	data["Chars"] = chars
	data["CharWallets"] = byWallet
	data["CharTotal"] = charTotal
	data["CorpRows"] = corpRows
	data["CorpTotal"] = corpTotal
	data["GrandTotal"] = charTotal + corpTotal
	data["Training"] = trainingOrder(chars)
	data["IdleQueues"] = idle
	data["LineTotals"] = lines
	s.render(w, "empire", data, stale)
}

// handleEmpireWallets renders one game-style wallet header per
// character and per corporation.
func (s *Server) handleEmpireWallets(w http.ResponseWriter, r *http.Request) {
	ec, stale := s.esiFor(r)
	data, _, err := s.shell(ec, 0, "")
	if err != nil {
		httpError(w, "loading characters", err)
		return
	}
	chars := empireChars(data)
	if len(chars) == 0 {
		s.render(w, "welcome", data, stale)
		return
	}

	// walletHead is one character's wallet header: ISK plus the two
	// currencies the game shows next to it.
	type walletHead struct {
		sideChar
		LPCorps   int
		EverMarks int64 // −1 when unknown
		Note      string
	}
	heads := make([]walletHead, len(chars))
	var wg sync.WaitGroup
	for i, ch := range chars {
		wg.Add(1)
		go func(i int, ch sideChar) {
			defer wg.Done()
			head := walletHead{sideChar: ch, EverMarks: -1}
			loyalty, err := ec.Loyalty(ch.ID)
			if err != nil {
				head.Note = "НБ недоступны"
			}
			for _, l := range loyalty {
				// EverMarks live in ESI as Paragon loyalty points.
				if l.CorpName == "Paragon" {
					head.EverMarks = l.Points
					continue
				}
				head.LPCorps++
			}
			heads[i] = head
		}(i, ch)
	}

	corps, _ := data["Corporations"].([]corpEntry)
	corpRows := s.corpWallets(ec, corps, true)
	wg.Wait()

	var charTotal, corpTotal float64
	for _, h := range heads {
		charTotal += h.Wallet
	}
	for _, row := range corpRows {
		corpTotal += row.Total
	}

	data["Heads"] = heads
	data["CorpRows"] = corpRows
	data["CharTotal"] = charTotal
	data["CorpTotal"] = corpTotal
	data["GrandTotal"] = charTotal + corpTotal
	s.render(w, "empire_wallets", data, stale)
}

// handleEmpireTraining shows the next few queued skills of every
// character, in the same square-per-level shape as the skills page.
func (s *Server) handleEmpireTraining(w http.ResponseWriter, r *http.Request) {
	ec, stale := s.esiFor(r)
	data, _, err := s.shell(ec, 0, "")
	if err != nil {
		httpError(w, "loading characters", err)
		return
	}
	chars := empireChars(data)
	if len(chars) == 0 {
		s.render(w, "welcome", data, stale)
		return
	}

	// How many entries of each queue the tab shows.
	const shown = 5

	type trainChar struct {
		sideChar
		Rows     []queueRow
		More     int // queued entries past the shown ones
		TotalDur string
		PlanEnds time.Time
		PlanSP   int64
		Paused   bool
	}
	rows := make([]trainChar, len(chars))
	now := time.Now()
	var wg sync.WaitGroup
	for i, ch := range chars {
		wg.Add(1)
		go func(i int, ch sideChar) {
			defer wg.Done()
			tc := trainChar{sideChar: ch}
			var sheet *esi.SkillSheet
			var queue []esi.QueueEntry
			var w2 sync.WaitGroup
			w2.Add(2)
			go func() { defer w2.Done(); sheet, _ = ec.Skills(ch.ID) }()
			go func() { defer w2.Done(); queue, _ = ec.SkillQueue(ch.ID) }()
			w2.Wait()

			qv := buildQueue(sheet, queue, now)
			tc.Rows = qv.Rows
			if len(tc.Rows) > shown {
				tc.More = len(tc.Rows) - shown
				tc.Rows = tc.Rows[:shown]
			}
			tc.TotalDur, tc.PlanEnds, tc.PlanSP, tc.Paused = qv.TotalDur, qv.PlanEnds, qv.PlanSP, qv.Paused
			rows[i] = tc
		}(i, ch)
	}
	wg.Wait()

	idle, paused := 0, 0
	for _, tc := range rows {
		switch {
		case len(tc.Rows) == 0:
			idle++
		case tc.QueueSkillID == 0:
			paused++
		}
	}

	data["Chars"] = rows
	data["Shown"] = shown
	data["IdleQueues"] = idle
	data["PausedQueues"] = paused
	data["Soonest"] = trainingOrder(chars)
	s.render(w, "empire_training", data, stale)
}

// handleEmpireIndustry shows what every character's industry lines are
// busy with: one slot per line, filled with its job or marked free.
func (s *Server) handleEmpireIndustry(w http.ResponseWriter, r *http.Request) {
	ec, stale := s.esiFor(r)
	data, _, err := s.shell(ec, 0, "")
	if err != nil {
		httpError(w, "loading characters", err)
		return
	}
	chars := empireChars(data)
	if len(chars) == 0 {
		s.render(w, "welcome", data, stale)
		return
	}

	now := time.Now()
	perChar := make([][]jobRow, len(chars))
	var wg sync.WaitGroup
	for i, ch := range chars {
		wg.Add(1)
		go func(i int, ch sideChar) {
			defer wg.Done()
			var jobs []jobRow
			if personal, err := ec.IndustryJobs(ch.ID); err == nil {
				for _, j := range personal {
					jobs = append(jobs, jobRow{IndustryJob: j})
				}
			}
			if corpID, _, err := ec.CharacterPublic(ch.ID); err == nil && corpID != 0 {
				if corpJobs, err := ec.CorporationIndustryJobs(ch.ID, corpID); err == nil {
					for _, cj := range corpJobs {
						if cj.InstallerID == ch.ID {
							jobs = append(jobs, jobRow{IndustryJob: cj.IndustryJob, IsCorp: true, Installer: cj.Installer})
						}
					}
				}
			}
			// Only undelivered jobs hold a line (same rule as lineStats).
			held := jobs[:0]
			for _, j := range jobs {
				if j.Status == "active" || j.Status == "paused" || j.Status == "ready" {
					held = append(held, j)
				}
			}
			perChar[i] = held
		}(i, ch)
	}
	wg.Wait()

	// One flat list of everything running anywhere, newest deadline last.
	// Each row carries the installer and how loaded that character's lines
	// of this very kind are.
	type indJob struct {
		jobRow
		CharID  int64
		Char    string
		Account string
		Kind    string // mfg | sci | rea
		Busy    int    // installer's busy lines of this kind
		Total   int
		Ready   bool
	}
	var jobs []indJob
	var lines lineStats
	kindJobs := map[string]int{}
	kindReady := map[string]int{}
	for i, ch := range chars {
		lines.MfgBusy += ch.Lines.MfgBusy
		lines.MfgTotal += ch.Lines.MfgTotal
		lines.SciBusy += ch.Lines.SciBusy
		lines.SciTotal += ch.Lines.SciTotal
		lines.ReaBusy += ch.Lines.ReaBusy
		lines.ReaTotal += ch.Lines.ReaTotal
		for _, j := range perChar[i] {
			row := indJob{jobRow: j, CharID: ch.ID, Char: ch.Name, Account: ch.Account}
			switch j.ActivityID {
			case 1:
				row.Kind, row.Busy, row.Total = "mfg", ch.Lines.MfgBusy, ch.Lines.MfgTotal
			case 3, 4, 5, 8:
				row.Kind, row.Busy, row.Total = "sci", ch.Lines.SciBusy, ch.Lines.SciTotal
			case 9, 11:
				row.Kind, row.Busy, row.Total = "rea", ch.Lines.ReaBusy, ch.Lines.ReaTotal
			default:
				continue
			}
			row.Ready = !j.EndDate.After(now)
			kindJobs[row.Kind]++
			if row.Ready {
				kindReady[row.Kind]++
			}
			jobs = append(jobs, row)
		}
	}
	sort.SliceStable(jobs, func(a, b int) bool { return jobs[a].EndDate.Before(jobs[b].EndDate) })

	// kindTile is one of the three header buttons: it both reports the
	// load of that line type and filters the list below.
	type kindTile struct {
		Key   string
		Name  string
		Busy  int
		Total int
		Jobs  int
		Ready int
	}
	tiles := []kindTile{
		{Key: "mfg", Name: "Производство", Busy: lines.MfgBusy, Total: lines.MfgTotal},
		{Key: "sci", Name: "Наука", Busy: lines.SciBusy, Total: lines.SciTotal},
		{Key: "rea", Name: "Реакции", Busy: lines.ReaBusy, Total: lines.ReaTotal},
	}
	for i := range tiles {
		tiles[i].Jobs = kindJobs[tiles[i].Key]
		tiles[i].Ready = kindReady[tiles[i].Key]
	}

	var cost float64
	for _, j := range jobs {
		cost += j.Cost
	}

	data["Jobs"] = jobs
	data["Tiles"] = tiles
	data["LineTotals"] = lines
	data["TotalCost"] = cost
	s.render(w, "empire_industry", data, stale)
}

func (s *Server) handleAssets(w http.ResponseWriter, r *http.Request) {
	ec, stale := s.esiFor(r)
	data, selected, ok := s.shellFor(w, r, ec, "assets")
	if !ok {
		return
	}
	var errs errList
	locations, err := ec.CharacterAssets(selected.ID)
	if err != nil {
		errs.add("имущество", err)
	}
	var total int64
	for _, l := range locations {
		total += l.Total
	}
	data["Locations"] = locations
	data["Total"] = total
	data["Errors"] = errs.list
	s.render(w, "assets", data, stale)
}

func (s *Server) handleCorpAssets(w http.ResponseWriter, r *http.Request) {
	ec, stale := s.esiFor(r)
	data, corp, ok := s.corpFor(w, r, ec)
	if !ok {
		return
	}
	var errs errList
	locations, err := ec.CorporationAssets(corp.ViaCharID, corp.ID)
	if err != nil {
		if strings.Contains(err.Error(), "403") {
			errs.add("имущество", fmt.Errorf("нет доступа — нужна роль Director в корпорации"))
		} else {
			errs.add("имущество", err)
		}
	}
	var total int64
	for _, l := range locations {
		total += l.Total
	}
	data["Locations"] = locations
	data["Total"] = total
	data["Errors"] = errs.list
	s.render(w, "corp_assets", data, stale)
}

// Industry line skills: base 1 line + skill levels.
const (
	skillMassProduction    = 3387
	skillAdvMassProduction = 24625
	skillLabOperation      = 3406
	skillAdvLabOperation   = 24624
	skillMassReactions     = 45748
	skillAdvMassReactions  = 45749
)

// lineStats holds busy/total industry lines per category.
type lineStats struct {
	MfgBusy, MfgTotal int
	SciBusy, SciTotal int
	ReaBusy, ReaTotal int
}

// industryLines derives available lines from skills and busy lines
// from undelivered jobs (active, paused or ready all hold a slot).
func industryLines(sheet *esi.SkillSheet, jobs []esi.IndustryJob) lineStats {
	lvl := map[int64]int{}
	for _, s := range sheet.Skills {
		lvl[s.SkillID] = s.ActiveLevel
	}
	ls := lineStats{
		MfgTotal: 1 + lvl[skillMassProduction] + lvl[skillAdvMassProduction],
		SciTotal: 1 + lvl[skillLabOperation] + lvl[skillAdvLabOperation],
		ReaTotal: 1 + lvl[skillMassReactions] + lvl[skillAdvMassReactions],
	}
	for _, j := range jobs {
		if j.Status != "active" && j.Status != "paused" && j.Status != "ready" {
			continue
		}
		switch j.ActivityID {
		case 1:
			ls.MfgBusy++
		case 3, 4, 5, 8:
			ls.SciBusy++
		case 9, 11:
			ls.ReaBusy++
		}
	}
	return ls
}

func (s *Server) handleOverview(w http.ResponseWriter, r *http.Request) {
	ec, stale := s.esiFor(r)
	data, selected, ok := s.shellFor(w, r, ec, "")
	if !ok {
		return
	}
	id := selected.ID

	var (
		wg         sync.WaitGroup
		sum        esi.Summary
		jobs       []esi.IndustryJob
		corpJobs   []esi.CorpIndustryJob
		sheet      *esi.SkillSheet
		orders     []esi.MarketOrder
		assetCount int
		errs       errList

		corpID, allianceID     int64
		corpName, allianceName string

		attrs      *esi.Attributes
		implantIDs []int64
		colonies   []colonyView
	)
	wg.Add(10)
	go func() { defer wg.Done(); colonies = s.colonyViews(ec, id, &errs) }()
	go func() {
		defer wg.Done()
		attrs, _ = ec.CharacterAttributes(id)
	}()
	go func() {
		defer wg.Done()
		implantIDs, _ = ec.Implants(id)
	}()
	go func() { defer wg.Done(); sum = ec.Summary(id) }()
	go func() {
		defer wg.Done()
		cid, cname, err := ec.CharacterPublic(id)
		if err != nil || cid == 0 {
			return
		}
		corpID, corpName = cid, cname
		if ci, err := ec.CorporationInfo(cid); err == nil {
			allianceID, allianceName = ci.AllianceID, ci.Alliance
		}
	}()
	go func() {
		defer wg.Done()
		var err error
		if jobs, err = ec.IndustryJobs(id); err != nil {
			errs.add("индустрия", err)
		}
	}()
	go func() {
		defer wg.Done()
		// Corp jobs installed by this character occupy their lines too.
		corpID, _, err := ec.CharacterPublic(id)
		if err != nil || corpID == 0 {
			return
		}
		all, err := ec.CorporationIndustryJobs(id, corpID)
		if err != nil {
			return // no Factory Manager role etc. — lines just count personal jobs
		}
		for _, cj := range all {
			if cj.InstallerID == id {
				corpJobs = append(corpJobs, cj)
			}
		}
	}()
	go func() {
		defer wg.Done()
		var err error
		if sheet, err = ec.Skills(id); err != nil {
			errs.add("навыки", err)
		}
	}()
	go func() {
		defer wg.Done()
		var err error
		if orders, err = ec.MarketOrders(id); err != nil {
			errs.add("ордера", err)
		}
	}()
	go func() {
		defer wg.Done()
		var err error
		if assetCount, err = ec.AssetCount(id); err != nil {
			errs.add("ассеты", err)
		}
	}()
	wg.Wait()
	errs.addAll("", sum.Errors)

	allJobs := jobs
	for _, cj := range corpJobs {
		allJobs = append(allJobs, cj.IndustryJob)
	}
	activeJobs := 0
	for _, j := range allJobs {
		if j.Status == "active" {
			activeJobs++
		}
	}
	if sheet == nil {
		sheet = &esi.SkillSheet{}
	}

	implants := s.SDE.Implants(implantIDs)
	now := time.Now()

	data["Now"] = now
	data["Planets"] = s.planetSlots(colonies, sheet, now)
	data["ColonyCount"] = len(colonies)
	data["Sum"] = sum
	data["Attrs"] = s.attributeRows(attrs, implants, "")
	data["ImplantCount"] = len(implantIDs)
	data["CorpID"] = corpID
	data["CorpName"] = corpName
	data["AllianceID"] = allianceID
	data["AllianceName"] = allianceName
	data["JobsCount"] = len(allJobs)
	data["ActiveJobs"] = activeJobs
	data["Lines"] = industryLines(sheet, allJobs)
	data["OrdersCount"] = len(orders)
	data["AssetCount"] = assetCount
	data["Errors"] = errs.list
	s.render(w, "overview", data, stale)
}

func (s *Server) handleSkills(w http.ResponseWriter, r *http.Request) {
	ec, stale := s.esiFor(r)
	data, selected, ok := s.shellFor(w, r, ec, "skills")
	if !ok {
		return
	}

	var errs errList
	sheet, err := ec.Skills(selected.ID)
	if err != nil {
		errs.add("навыки", err)
		sheet = &esi.SkillSheet{}
	}

	var (
		wg    sync.WaitGroup
		tree  []esi.SkillGroupTree
		queue []esi.QueueEntry
		attrs *esi.Attributes
	)
	wg.Add(3)
	go func() {
		defer wg.Done()
		var err error
		if tree, err = ec.SkillTree(sheet); err != nil {
			errs.add("дерево навыков", err)
		}
	}()
	go func() {
		defer wg.Done()
		var err error
		if queue, err = ec.SkillQueue(selected.ID); err != nil {
			errs.add("очередь", err)
		}
	}()
	go func() {
		defer wg.Done()
		// Скорость обучения. ESI отдаёт атрибуты уже с имплантами,
		// так что ничего доливать не надо.
		attrs, _ = ec.CharacterAttributes(selected.ID)
	}()
	wg.Wait()

	qv := buildQueue(sheet, queue, time.Now())

	// The skill in training right now — its target square blinks both in
	// the plan and in the catalog.
	var trainSkill int64
	var trainLvl int
	for _, row := range qv.Rows {
		if row.Active {
			trainSkill, trainLvl = row.SkillID, row.FinishedLevel
			break
		}
	}
	data["TrainSkill"] = trainSkill
	data["TrainLvl"] = trainLvl

	// ── что из каталога стоит в очереди и с какой скоростью учится ──
	// Каталог сам по себе не отвечает на вопрос «что из этой группы
	// вообще учится»: у навыка видно изученные уровни, но не то, что он
	// уже стоит в плане. Собираем и уровень из очереди, и счётчик на
	// группу, и СП/мин — скорость у каждого навыка своя, она зависит от
	// пары атрибутов.
	queued := map[int64]int{}
	for _, row := range qv.Rows {
		if row.FinishedLevel > queued[row.SkillID] {
			queued[row.SkillID] = row.FinishedLevel
		}
	}
	groupQueued := make([]int, len(tree))
	for i, g := range tree {
		for _, sk := range g.Skills {
			if queued[sk.SkillID] > 0 {
				groupQueued[i]++
			}
		}
	}
	rates := map[int64]float64{}
	if attrs != nil {
		a := skillplan.Attrs{
			"intelligence": attrs.Intelligence,
			"memory":       attrs.Memory,
			"perception":   attrs.Perception,
			"willpower":    attrs.Willpower,
			"charisma":     attrs.Charisma,
		}
		for id, info := range s.SDE.Skills() {
			if info.Prim != "" {
				rates[id] = a.Rate(info.Prim, info.Sec)
			}
		}
	}
	data["Queued"] = queued
	data["GroupQueued"] = groupQueued
	data["Rates"] = rates
	data["ActiveRate"] = rates[trainSkill]

	data["Sheet"] = sheet
	data["Tree"] = tree
	data["Queue"] = qv.Rows
	data["QueuePaused"] = qv.Paused
	data["TotalDur"] = qv.TotalDur
	data["PlanEnds"] = qv.PlanEnds
	data["PlanSP"] = qv.PlanSP
	data["Errors"] = errs.list
	s.render(w, "skills", data, stale)
}

// queueRow is one skill-queue entry prepared for display.
type queueRow struct {
	esi.QueueEntry
	TrainedNow int     // really trained now (white); up to FinishedLevel — queued (teal)
	Dur        string  // human duration of this entry
	Pct        float64 // share of total plan time, %
	Active     bool
}

// queueView is a whole skill queue in EVE's plan shape.
type queueView struct {
	Rows     []queueRow
	Total    time.Duration
	TotalDur string
	PlanSP   int64
	PlanEnds time.Time
	Paused   bool
}

// buildQueue turns a raw skill queue into the plan view: per-entry
// duration, share of the total for the segmented bar, SP to be gained.
// Entries already finished are dropped; a paused queue has zero finish
// dates.
func buildQueue(sheet *esi.SkillSheet, queue []esi.QueueEntry, now time.Time) queueView {
	trained := map[int64]int{}
	if sheet != nil {
		for _, sk := range sheet.Skills {
			trained[sk.SkillID] = sk.TrainedLevel
		}
	}
	var (
		qv      queueView
		durs    []time.Duration
		started bool
	)
	for _, q := range queue {
		if !q.FinishDate.IsZero() && q.FinishDate.Before(now) {
			continue // already completed
		}
		row := queueRow{QueueEntry: q, TrainedNow: q.FinishedLevel - 1}
		if lv := trained[q.SkillID]; lv < row.TrainedNow {
			row.TrainedNow = lv // the gap up to the target is trained by the queue → teal
		}
		var d time.Duration
		switch {
		case q.FinishDate.IsZero():
			qv.Paused = true
		case !started:
			started, row.Active = true, true
			d = q.FinishDate.Sub(now)
		default:
			d = q.FinishDate.Sub(q.StartDate)
		}
		if d > 0 {
			row.Dur = humanDur(d)
			qv.Total += d
		}
		start := q.LevelStartSP
		if q.TrainingStartSP > start {
			start = q.TrainingStartSP
		}
		if q.LevelEndSP > start {
			qv.PlanSP += q.LevelEndSP - start
		}
		if q.FinishDate.After(qv.PlanEnds) {
			qv.PlanEnds = q.FinishDate
		}
		qv.Rows = append(qv.Rows, row)
		durs = append(durs, d)
	}
	if qv.Total > 0 {
		for i := range qv.Rows {
			qv.Rows[i].Pct = float64(durs[i]) / float64(qv.Total) * 100
		}
	}
	qv.TotalDur = humanDur(qv.Total)
	return qv
}

func (s *Server) handleWallet(w http.ResponseWriter, r *http.Request) {
	ec, stale := s.esiFor(r)
	data, selected, ok := s.shellFor(w, r, ec, "wallet")
	if !ok {
		return
	}
	id := selected.ID

	var (
		wg      sync.WaitGroup
		balance float64
		journal []esi.JournalEntry
		txns    []esi.Transaction
		errs    errList
	)
	wg.Add(3)
	go func() {
		defer wg.Done()
		var err error
		if balance, err = ec.WalletBalance(id); err != nil {
			errs.add("баланс", err)
		}
	}()
	go func() {
		defer wg.Done()
		var err error
		if journal, err = ec.WalletJournal(id); err != nil {
			errs.add("журнал", err)
		}
	}()
	go func() {
		defer wg.Done()
		var err error
		if txns, err = ec.WalletTransactions(id); err != nil {
			errs.add("транзакции", err)
		}
	}()
	var loyalty []esi.LoyaltyRow
	wg.Add(1)
	go func() {
		defer wg.Done()
		var err error
		if loyalty, err = ec.Loyalty(id); err != nil {
			errs.add("наградные баллы", err)
		}
	}()
	wg.Wait()

	type journalRow struct {
		esi.JournalEntry
		TypeRu string
	}
	jrows := make([]journalRow, len(journal))
	for i, j := range journal {
		jrows[i] = journalRow{JournalEntry: j, TypeRu: refTypeRu(j.RefType)}
	}

	// EverMarks live in ESI as loyalty points of the Paragon corporation —
	// the game shows them separately from the LP list, so do we.
	var evermarks int64 = -1
	lp := loyalty[:0]
	for _, l := range loyalty {
		if l.CorpName == "Paragon" {
			evermarks = l.Points
			continue
		}
		lp = append(lp, l)
	}

	data["Balance"] = balance
	data["Journal"] = jrows
	data["Transactions"] = txns
	data["Loyalty"] = lp
	data["EverMarks"] = evermarks
	data["Errors"] = errs.list
	s.render(w, "wallet", data, stale)
}

// refTypeRu localizes the common wallet-journal ref_type values the way
// the game client names them; unknown types fall back to the raw value.
var refTypeNames = map[string]string{
	"player_donation":                   "Сделанное игроком пожертвование",
	"player_trading":                    "Коммерческая деятельность игрока",
	"market_transaction":                "Коммерческая деятельность игрока",
	"market_escrow":                     "Депонирование (маркет)",
	"brokers_fee":                       "Брокерская комиссия",
	"transaction_tax":                   "Налог с транзакции",
	"bounty_prizes":                     "Награда за голову",
	"bounty_prize":                      "Награда за голову",
	"agent_mission_reward":              "Награда за миссию",
	"agent_mission_time_bonus_reward":   "Бонус за миссию",
	"daily_goal_payouts":                "Награда за ежедневную цель",
	"corporate_reward_payout":           "Корпоративная награда",
	"corporation_account_withdrawal":    "Перевод со счёта корпорации",
	"insurance":                         "Страховка",
	"industry_job_tax":                  "Налог индустрии",
	"manufacturing":                     "Производство",
	"researching_time_productivity":     "Исследование (время)",
	"researching_material_productivity": "Исследование (материалы)",
	"copying":                           "Копирование чертежей",
	"reaction":                          "Реакции",
	"reprocessing_tax":                  "Налог переработки",
	"planetary_import_tax":              "Налог ПВ (ввоз)",
	"planetary_export_tax":              "Налог ПВ (вывоз)",
	"planetary_construction":            "Строительство ПВ",
	"contract_price":                    "Контракт: цена",
	"contract_reward":                   "Контракт: вознаграждение",
	"contract_collateral":               "Контракт: залог",
	"contract_brokers_fee":              "Контракт: комиссия",
	"contract_deposit":                  "Контракт: депозит",
	"contract_sales_tax":                "Контракт: налог",
	"skill_purchase":                    "Покупка навыка",
	"structure_gate_jump":               "Прыжок через врата",
	"office_rental_fee":                 "Аренда офиса",
	"ess_escrow_transfer":               "Выплата из ESS",
	"milestone_reward_payment":          "Награда за этап",
	"season_challenge_reward":           "Награда за испытание",
	"project_discovery_reward":          "Project Discovery",
	"clone_activation":                  "Активация клона",
	"jump_clone_installation_fee":       "Установка джамп-клона",
	"asset_safety_recovery_tax":         "Налог asset safety",
	"air_career_program_reward":         "Награда программы АМИ",
	"daily_challenge_reward":            "Ежедневное испытание",
	"agents_preward":                    "Аванс агента",
	"structure_service_fee":             "Сервис структуры",
	"infrastructure_hub_maintenance":    "Содержание инфраструктуры",
	"war_fee":                           "Взнос за войну",
	"kill_right_fee":                    "Право на убийство",
	"cspa":                              "CSPA",
	"docking_fee":                       "Стыковочный сбор",
	"reimbursement_of_taxes":            "Возврат налогов",
	"resource_wars_reward":              "Resource Wars",
	"duel_wager_escrow":                 "Ставка дуэли",
	"external_trade_delivery":           "Внешняя торговая поставка",
	"external_trade_freeze":             "Внешняя торговля (заморозка)",
	"external_trade_thaw":               "Внешняя торговля (разморозка)",
}

func refTypeRu(rt string) string {
	if v, ok := refTypeNames[rt]; ok {
		return v
	}
	return strings.ReplaceAll(rt, "_", " ")
}

// attrRow is one character attribute with its implant contribution.
type attrRow struct {
	Key      string
	Label    string
	Total    int // as reported by ESI (already includes implants)
	Implant  int // points coming from implants
	Base     int // total minus implants
	Training bool
}

// implantView is one implant with its bonuses, ready for templates.
type implantView struct {
	TypeID  int64
	Name    string
	Slot    int
	Bonuses []string
}

func bonusList(b map[string]int) []string {
	order := []struct{ Key, Label string }{
		{"perception", "Восприятие"}, {"memory", "Память"},
		{"willpower", "Сила воли"}, {"intelligence", "Интеллект"},
		{"charisma", "Харизма"},
	}
	var out []string
	for _, o := range order {
		if v, ok := b[o.Key]; ok && v != 0 {
			out = append(out, fmt.Sprintf("%s +%d", o.Label, v))
		}
	}
	return out
}

// attributeRows combines ESI attributes with implant bonuses from the
// static-data database. Without sde.db implant columns stay zero.
func (s *Server) attributeRows(attrs *esi.Attributes, implants []sde.Implant, trainKey string) []attrRow {
	if attrs == nil {
		return nil
	}
	fromImplants := map[string]int{}
	for _, im := range implants {
		for k, v := range im.Bonuses {
			fromImplants[k] += v
		}
	}
	defs := []struct {
		Key, Label string
		Total      int
	}{
		{"perception", "Восприятие", attrs.Perception},
		{"memory", "Память", attrs.Memory},
		{"willpower", "Сила воли", attrs.Willpower},
		{"intelligence", "Интеллект", attrs.Intelligence},
		{"charisma", "Харизма", attrs.Charisma},
	}
	rows := make([]attrRow, 0, len(defs))
	for _, d := range defs {
		imp := fromImplants[d.Key]
		rows = append(rows, attrRow{
			Key: d.Key, Label: d.Label, Total: d.Total,
			Implant: imp, Base: d.Total - imp,
			Training: d.Key == trainKey,
		})
	}
	return rows
}

// cloneData gathers attributes, implants and jump clones for a character.
func (s *Server) cloneData(ec *esi.Client, id int64, errs *errList) (
	attrs *esi.Attributes, active []sde.Implant, clones *esi.Clones, locNames map[int64]string,
	jumpImplants map[int64][]sde.Implant) {

	var wg sync.WaitGroup
	var implantIDs []int64
	wg.Add(3)
	go func() {
		defer wg.Done()
		var err error
		if attrs, err = ec.CharacterAttributes(id); err != nil {
			errs.add("характеристики", err)
		}
	}()
	go func() {
		defer wg.Done()
		var err error
		if implantIDs, err = ec.Implants(id); err != nil {
			errs.add("импланты", err)
		}
	}()
	go func() {
		defer wg.Done()
		var err error
		if clones, err = ec.CharacterClones(id); err != nil {
			errs.add("клоны", err)
		}
	}()
	wg.Wait()

	active = s.SDE.Implants(implantIDs)
	// Names fall back to ESI when the static database is absent.
	if !s.SDE.Available() && len(implantIDs) > 0 {
		names := ec.Names(implantIDs)
		for _, tid := range implantIDs {
			active = append(active, sde.Implant{TypeID: tid, Name: names[tid]})
		}
	}

	jumpImplants = map[int64][]sde.Implant{}
	locIDs := []int64{}
	if clones != nil {
		if clones.HomeLocation.LocationID != 0 {
			locIDs = append(locIDs, clones.HomeLocation.LocationID)
		}
		for _, jc := range clones.JumpClones {
			locIDs = append(locIDs, jc.LocationID)
			ims := s.SDE.Implants(jc.Implants)
			if !s.SDE.Available() && len(jc.Implants) > 0 {
				names := ec.Names(jc.Implants)
				for _, tid := range jc.Implants {
					ims = append(ims, sde.Implant{TypeID: tid, Name: names[tid]})
				}
			}
			jumpImplants[jc.ID] = ims
		}
	}
	locNames = ec.LocationNames(id, locIDs)
	return
}

func (s *Server) handleClones(w http.ResponseWriter, r *http.Request) {
	ec, stale := s.esiFor(r)
	data, selected, ok := s.shellFor(w, r, ec, "clones")
	if !ok {
		return
	}
	var errs errList
	attrs, active, clones, locNames, jumpImplants := s.cloneData(ec, selected.ID, &errs)

	type cloneView struct {
		ID       int64
		Name     string
		Location string
		Implants []implantView
	}
	var views []cloneView
	if clones != nil {
		for _, jc := range clones.JumpClones {
			cv := cloneView{ID: jc.ID, Name: jc.Name, Location: locNames[jc.LocationID]}
			if cv.Name == "" {
				cv.Name = "Клон без имени"
			}
			if cv.Location == "" {
				cv.Location = fmt.Sprintf("локация %d", jc.LocationID)
			}
			for _, im := range jumpImplants[jc.ID] {
				cv.Implants = append(cv.Implants, implantView{im.TypeID, im.Name, im.Slot, bonusList(im.Bonuses)})
			}
			views = append(views, cv)
		}
	}
	var activeViews []implantView
	for _, im := range active {
		activeViews = append(activeViews, implantView{im.TypeID, im.Name, im.Slot, bonusList(im.Bonuses)})
	}

	data["Attrs"] = s.attributeRows(attrs, active, "")
	data["RawAttrs"] = attrs
	data["ActiveImplants"] = activeViews
	data["JumpClones"] = views
	if clones != nil {
		data["HomeLocation"] = locNames[clones.HomeLocation.LocationID]
		data["LastJump"] = clones.LastCloneJump
	}
	data["SDEReady"] = s.SDE.Available()
	data["Errors"] = errs.list
	s.render(w, "clones", data, stale)
}

func (s *Server) handleBlueprints(w http.ResponseWriter, r *http.Request) {
	ec, stale := s.esiFor(r)
	data, selected, ok := s.shellFor(w, r, ec, "blueprints")
	if !ok {
		return
	}
	var errs errList
	bps, err := ec.CharacterBlueprints(selected.ID)
	if err != nil {
		errs.add("чертежи", err)
	}
	originals, copies := 0, 0
	for _, b := range bps {
		if b.IsCopy() {
			copies++
		} else {
			originals++
		}
	}
	data["Blueprints"] = bps
	data["Originals"] = originals
	data["Copies"] = copies
	data["Errors"] = errs.list
	s.render(w, "blueprints", data, stale)
}

// planetPinView is one structure of a colony, decoded for display.
type planetPinView struct {
	PinID     int64
	TypeID    int64
	TypeName  string
	Role      string
	Product   string
	ProductID int64
	QtyCycle  int64
	CycleMin  int
	Install   time.Time
	Expiry    time.Time
	Contents  []planetContent
	Heads     int

	// extraction program (extractors)
	Cycles    int
	ProgTotal int64
	ProgHour  float64
	ProgLeft  int64 // still to be extracted
	Bars      []progBar
	Grid      []gridMark // 25/50/75/100 % rules behind the bars
	SparkPeak int64
	CycleDone int // cycles already finished
	prog      []pi.CycleYield

	// where the yield stops covering the factories fed by this extractor
	CoverCycle  int     // first cycle running at a deficit, 0 = never
	CoverX      float64 // its position on the chart, in svg units
	CoverAt     string  // when that cycle ends
	CoverDemand int64   // what the factories eat per extractor cycle
	CoverFacs   int     // how many factories that is
	CoverPeers  int     // extractors sharing this material

	// storage load (storage / launchpad / command centre)
	UsedM3 float64
	CapM3  float64
	LoadPc float64

	// colony map: position in percent of the layout box, cycle progress
	X, Y      float64
	CycleSec  int
	CycleEnds time.Time // when the running cycle completes
	Idle      bool      // nothing in production
	Delivered bool      // extractor: at least one cycle has been paid out
	Inputs    []planetContent
	OutQty    int64
}

type planetContent struct {
	TypeID int64
	Name   string
	Amount int64
}

// progBar is one bar of the extraction program chart.
type progBar struct {
	X, W, Y, H float64 // svg geometry in a 0..100 box
	Qty        int64
	Cycle      int
	Past       bool   // cycle already finished
	At         string // when this cycle ends, for the tooltip
}

// gridMark is one horizontal rule of the extraction chart.
type gridMark struct {
	Y   float64 // svg units
	Pc  int     // 25 / 50 / 75 / 100 % of the peak
	Val int64   // the amount that rule stands for
}

// mapRoute is one hop of a product flow between two structures.
type mapRoute struct {
	X1, Y1, X2, Y2 float64
	Goods          string // what travels this hop
	Kind           string // "raw" | "product" — colours the arrow
	FromPin, ToPin int64
}

// colonyView is a colony with its decoded layout.
type colonyView struct {
	esi.Colony
	Pins       []planetPinView
	Extractors []planetPinView
	Factories  []planetPinView
	Storage    []planetPinView
	Links      int
	Routes     int
	MapRoutes  []mapRoute // product flows drawn on the colony map
	NextExpiry time.Time  // soonest extractor expiry
	Err        string
}

func (s *Server) handlePlanets(w http.ResponseWriter, r *http.Request) {
	ec, stale := s.esiFor(r)
	data, selected, ok := s.shellFor(w, r, ec, "planets")
	if !ok {
		return
	}
	var errs errList
	views := s.colonyViews(ec, selected.ID, &errs)

	data["Colonies"] = views
	data["Now"] = time.Now()
	data["Errors"] = errs.list
	s.render(w, "planets", data, stale)
}

// colonyViews decodes every colony of a character into the layout the
// planets page draws: pins by role, product flows, extraction programs
// and the idle verdict per factory. The overview card reuses it.
func (s *Server) colonyViews(ec *esi.Client, charID int64, errs *errList) []colonyView {
	colonies, err := ec.Colonies(charID)
	if err != nil {
		errs.add("колонии", err)
	}

	views := make([]colonyView, 0, len(colonies))
	for _, col := range colonies {
		v := colonyView{Colony: col}
		detail, err := ec.ColonyDetail(charID, col.PlanetID)
		if err != nil {
			v.Err = err.Error()
			views = append(views, v)
			continue
		}
		v.Links, v.Routes = len(detail.Links), len(detail.Routes)

		// Collect ids for one batched name lookup — including commodities
		// that only appear on routes (transit goods never sit in a hangar).
		var ids []int64
		for _, p := range detail.Pins {
			ids = append(ids, p.TypeID)
			if p.ExtractorDetails != nil {
				ids = append(ids, p.ExtractorDetails.ProductTypeID)
			}
			for _, c := range p.Contents {
				ids = append(ids, c.TypeID)
			}
		}
		for _, rt := range detail.Routes {
			ids = append(ids, rt.ContentTypeID)
		}
		names := ec.Names(ids)
		sdeNames := s.SDE.TypeNames(ids)
		nameOf := func(id int64) string {
			if n := sdeNames[id]; n != "" {
				return n
			}
			return names[id]
		}

		// Where each pin sends its goods, from the routes.
		volumes := s.SDE.Volumes(ids)
		recipes := s.SDE.PIRecipes()
		now := time.Now()

		for _, p := range detail.Pins {
			pv := planetPinView{
				PinID: p.PinID, TypeID: p.TypeID, TypeName: nameOf(p.TypeID),
				Role:    s.SDE.StructureRole(p.TypeID),
				Install: p.InstallTime, Expiry: p.ExpiryTime,
			}
			for _, c := range p.Contents {
				pv.Contents = append(pv.Contents, planetContent{c.TypeID, nameOf(c.TypeID), c.Amount})
				pv.UsedM3 += float64(c.Amount) * volumes[c.TypeID]
			}
			sort.Slice(pv.Contents, func(i, j int) bool { return pv.Contents[i].Amount > pv.Contents[j].Amount })
			if cap := s.SDE.Capacity(p.TypeID); cap > 0 {
				pv.CapM3 = cap
				pv.LoadPc = pv.UsedM3 / cap * 100
			}

			switch {
			case p.ExtractorDetails != nil:
				pv.ProductID = p.ExtractorDetails.ProductTypeID
				pv.Product = nameOf(pv.ProductID)
				pv.QtyCycle = p.ExtractorDetails.QtyPerCycle
				pv.CycleSec = p.ExtractorDetails.CycleTime
				pv.CycleMin = p.ExtractorDetails.CycleTime / 60
				pv.Heads = len(p.ExtractorDetails.Heads)
				// Extractors cycle continuously until the program expires.
				if pv.CycleSec > 0 && !p.InstallTime.IsZero() && p.ExpiryTime.After(now) {
					elapsed := now.Sub(p.InstallTime).Seconds()
					done := math.Floor(elapsed / float64(pv.CycleSec))
					pv.CycleEnds = p.InstallTime.Add(
						time.Duration((done + 1) * float64(pv.CycleSec) * float64(time.Second)))
					// Output lands at the end of a cycle: a freshly
					// installed programme has delivered nothing yet.
					pv.Delivered = done >= 1
				} else {
					pv.Idle = true
				}
				if !p.ExpiryTime.IsZero() && (v.NextExpiry.IsZero() || p.ExpiryTime.Before(v.NextExpiry)) {
					v.NextExpiry = p.ExpiryTime
				}
				// Rebuild the whole extraction program the way the game
				// draws it: decaying yield per cycle.
				cs := p.ExtractorDetails.CycleTime
				if cs > 0 && !p.InstallTime.IsZero() && !p.ExpiryTime.IsZero() {
					pv.Cycles = int(p.ExpiryTime.Sub(p.InstallTime).Seconds()) / cs
				}
				if prog := pi.ExtractionProgram(pv.QtyCycle, cs, pv.Cycles); len(prog) > 0 {
					pv.ProgTotal, pv.ProgHour, pv.SparkPeak = pi.ProgramTotals(prog, cs)
					done := 0
					if !p.InstallTime.IsZero() && now.After(p.InstallTime) {
						done = int(now.Sub(p.InstallTime).Seconds()) / cs
					}
					if done > len(prog) { // a finished program stops counting
						done = len(prog)
					}
					for i, c := range prog {
						if i >= done {
							pv.ProgLeft += c.Qty
						}
					}
					pv.CycleDone = done
					pv.prog = prog
					pv.Bars, pv.Grid = programBars(prog, done, p.InstallTime, cs)
				}
				v.Extractors = append(v.Extractors, pv)
			case p.SchematicID != 0 || p.FactoryDetails != nil:
				sid := p.SchematicID
				if sid == 0 && p.FactoryDetails != nil {
					sid = p.FactoryDetails.SchematicID
				}
				pv.Product, pv.ProductID, pv.CycleSec = s.SDE.SchematicInfo(sid)
				for _, in := range s.SDE.SchematicInputs(sid) {
					pv.Inputs = append(pv.Inputs, planetContent{in.TypeID, in.Name, in.Quantity})
				}
				if rec, ok := recipes[pv.ProductID]; ok {
					pv.OutQty = rec.OutQty
				}
				// ESI recalculates a colony lazily, so last_cycle_start can
				// lag far behind even on a running colony. Treat a factory
				// as running when the raw material it needs is present
				// somewhere in the colony, and show a rolling cycle.
				if !p.LastCycleStart.IsZero() && pv.CycleSec > 0 {
					elapsed := now.Sub(p.LastCycleStart).Seconds()
					cyc := float64(pv.CycleSec)
					if elapsed < 0 {
						elapsed = 0
					}
					// position inside the current (repeating) cycle
					intoCycle := math.Mod(elapsed, cyc)
					pv.CycleEnds = now.Add(time.Duration((cyc - intoCycle) * float64(time.Second)))
				}
				v.Factories = append(v.Factories, pv)
			default:
				v.Storage = append(v.Storage, pv)
			}
			v.Pins = append(v.Pins, pv)
		}

		// Whether a factory runs cannot be read off last_cycle_start:
		// ESI recalculates a colony lazily. Follow the supply instead —
		// but per route and per quantity, not "this material exists
		// somewhere in the colony": a factory holding 5 of the 10 units
		// it needs, fed by neighbours that have themselves stalled, is
		// idle. A material reaches a factory when the factory already
		// holds a full cycle of it, or when a route carries it in from a
		// pin that produces it (running extractor / running factory),
		// stocks a full cycle of it, or is itself being fed with it.
		stock := make(map[int64]map[int64]int64, len(detail.Pins))
		for _, p := range detail.Pins {
			m := make(map[int64]int64, len(p.Contents))
			for _, c := range p.Contents {
				m[c.TypeID] += c.Amount
			}
			stock[p.PinID] = m
		}
		// A live extractor only counts once it has actually paid out a
		// cycle: a programme installed minutes ago has put nothing in the
		// colony yet, and if the leftovers in the hangars are a different
		// material the factories are standing there waiting.
		extracts := map[int64]int64{} // pin → product of a delivering extractor
		for _, e := range v.Extractors {
			if !e.Idle && e.Delivered && e.ProductID != 0 {
				extracts[e.PinID] = e.ProductID
			}
		}
		makes := map[int64]int64{} // pin → product of a factory
		for _, f := range v.Factories {
			makes[f.PinID] = f.ProductID
		}
		type feed struct{ pin, typ int64 }
		sources := map[feed][]int64{}
		for _, rt := range detail.Routes {
			k := feed{rt.DestinationPinID, rt.ContentTypeID}
			sources[k] = append(sources[k], rt.SourcePinID)
		}
		running := map[int64]bool{}
		var supplied func(pin, typ, need int64, seen map[int64]bool) bool
		supplied = func(pin, typ, need int64, seen map[int64]bool) bool {
			if seen[pin] {
				return false
			}
			seen[pin] = true
			if extracts[pin] == typ {
				return true
			}
			if makes[pin] == typ && running[pin] {
				return true
			}
			if need > 0 && stock[pin][typ] >= need {
				return true
			}
			for _, src := range sources[feed{pin, typ}] {
				if supplied(src, typ, need, seen) {
					return true
				}
			}
			return false
		}
		// Forward pass: the running set only grows, so P0 → P1 → P2
		// chains settle after at most one pass per factory.
		for pass := 0; pass <= len(v.Factories); pass++ {
			grew := false
			for i := range v.Factories {
				f := &v.Factories[i]
				if running[f.PinID] || len(f.Inputs) == 0 {
					continue
				}
				ok := true
				for _, in := range f.Inputs {
					if !supplied(f.PinID, in.TypeID, in.Amount, map[int64]bool{}) {
						ok = false
						break
					}
				}
				if ok {
					running[f.PinID] = true
					grew = true
				}
			}
			if !grew {
				break
			}
		}
		for i := range v.Factories {
			v.Factories[i].Idle = !running[v.Factories[i].PinID]
		}
		// Mirror the verdict onto the map pins.
		idleByPin := map[int64]bool{}
		for _, f := range v.Factories {
			idleByPin[f.PinID] = f.Idle
		}
		for i := range v.Pins {
			if idle, ok := idleByPin[v.Pins[i].PinID]; ok {
				v.Pins[i].Idle = idle
			}
		}

		markExtractorCoverage(&v)

		if len(v.Pins) > 0 {
			layoutColony(v.Pins)
			// Sync positions into the per-role slices used by the map.
			pos := map[int64][2]float64{}
			for _, p := range v.Pins {
				pos[p.PinID] = [2]float64{p.X, p.Y}
			}
			fix := func(list []planetPinView) {
				for i := range list {
					if xy, ok := pos[list[i].PinID]; ok {
						list[i].X, list[i].Y = xy[0], xy[1]
					}
				}
			}
			fix(v.Extractors)
			fix(v.Factories)
			fix(v.Storage)

			// Draw the product flows (routes), not the physical cables:
			// hops are aggregated per pin pair so one arrow can carry
			// several commodities.
			isFactory := map[int64]bool{}
			for _, p := range v.Pins {
				if p.Role == "basic" || p.Role == "advanced" || p.Role == "hightech" || p.ProductID != 0 {
					isFactory[p.PinID] = p.Product != ""
				}
			}
			// One straight arrow per route: source → destination. The
			// waypoints in between are just the physical path the goods
			// take and only clutter the diagram.
			type hop struct{ from, to int64 }
			goods := map[hop]map[string]bool{}
			kindOf := map[hop]string{}
			order := []hop{}
			for _, rt := range detail.Routes {
				h := hop{rt.SourcePinID, rt.DestinationPinID}
				if goods[h] == nil {
					goods[h] = map[string]bool{}
					order = append(order, h)
				}
				goods[h][nameOf(rt.ContentTypeID)] = true
				// Feeding a factory means inputs; anything else is output.
				if isFactory[rt.DestinationPinID] {
					kindOf[h] = "raw"
				} else if kindOf[h] == "" {
					kindOf[h] = "product"
				}
			}
			for _, h := range order {
				a, okA := pos[h.from]
				b, okB := pos[h.to]
				if !okA || !okB {
					continue
				}
				var names []string
				for n := range goods[h] {
					names = append(names, n)
				}
				sort.Strings(names)
				kind := kindOf[h]
				if kind == "" {
					kind = "product"
				}
				v.MapRoutes = append(v.MapRoutes, mapRoute{
					X1: a[0], Y1: a[1], X2: b[0], Y2: b[1],
					Goods: strings.Join(names, " · "), Kind: kind,
					FromPin: h.from, ToPin: h.to,
				})
			}
		}

		views = append(views, v)
	}
	return views
}

// Interplanetary Consolidation: one command centre by default, one more
// per level of the skill, six planets at level V.
const (
	skillInterplanetary = 2495
	maxPlanets          = 6
)

// planetSlot is one tile of the planets card on the overview page.
// There are always six of them: the colonies first, then the free
// slots, then the ones the skill has not unlocked yet.
type planetSlot struct {
	Locked bool      // Interplanetary Consolidation is not trained that far
	Empty  bool      // unlocked, but no colony on it
	Now    time.Time // the tile template renders countdowns against it

	PlanetName   string
	PlanetType   string
	PlanetTypeID int64 // icon
	SystemName   string
	UpgradeLevel int
	PlanetID     int64

	// extraction ring (blue)
	ExtHas  bool
	ExtIdle bool // has extractors, none of them running
	ExtEnds time.Time
	ExtSec  int

	// production ring (orange)
	FacHas   bool
	FacIdle  bool
	FacEnds  time.Time
	FacSec   int
	FacRun   int
	FacTotal int

	// storage ring (green → yellow → red), command centre excluded
	UsedM3   float64
	CapM3    float64
	LoadPc   float64
	StorRing []storSeg

	Extractors []planetPinView
	Storage    []planetPinView
	Feed       []slotFeed
}

// ── planets across all alts ──────────────────────────────────────────

// charPlanets is one character's block on the all-alts planet page.
type charPlanets struct {
	ID      int64
	Name    string
	Account string
	Allowed int // planet slots unlocked by the skill
	Used    int // colonies planted
	YieldHr float64
	Slots   []planetSlot
}

// planetAlert is one problem of one colony.
type planetAlert struct {
	Level string // "err" — stopped; "warn" — will stop soon
	What  string
	Note  string
}

// colonyAlert is one line of the "needs attention" list: everything
// wrong with a single colony, joined with "и" — a colony never takes
// more than one row.
type colonyAlert struct {
	Level  string // worst severity of the parts — colours the badge
	CharID int64
	Char   string
	Planet string
	Parts  []planetAlert
	rank   int // sort key: colonies already stopped first
}

// yieldRow is what all colonies together pull out of the ground per
// hour, per material.
type yieldRow struct {
	TypeID  int64
	Name    string
	PerHour float64
	Rigs    int // extractors working on it
}

// planetTotals are the headline numbers of the summary page.
type planetTotals struct {
	Chars          int // characters running colonies
	Colonies       int
	Allowed        int // planet slots unlocked across all alts
	Free           int // unlocked but unused
	ExtRun, ExtAll int
	FacRun, FacAll int
	StorFull       int // colonies at 90 % or more
	YieldHr        float64
	AlertsErr      int // already stopped
	AlertsWarn     int // about to stop
}

func (s *Server) handleEmpirePlanets(w http.ResponseWriter, r *http.Request) {
	ec, stale := s.esiFor(r)
	data, _, err := s.shell(ec, 0, "")
	if err != nil {
		httpError(w, "loading characters", err)
		return
	}
	groups, _ := data["Groups"].([]accountGroup)
	var chars []sideChar
	for _, g := range groups {
		chars = append(chars, g.Chars...)
	}
	if len(chars) == 0 {
		s.render(w, "welcome", data, stale)
		return
	}

	now := time.Now()
	var errs errList
	blocks := make([]charPlanets, len(chars))
	var wg sync.WaitGroup
	for i, ch := range chars {
		wg.Add(1)
		go func(i int, ch sideChar) {
			defer wg.Done()
			var (
				inner errList
				sheet *esi.SkillSheet
				views []colonyView
				w2    sync.WaitGroup
			)
			w2.Add(2)
			go func() { defer w2.Done(); sheet, _ = ec.Skills(ch.ID) }()
			go func() { defer w2.Done(); views = s.colonyViews(ec, ch.ID, &inner) }()
			w2.Wait()
			errs.addAll(ch.Name, inner.list)

			cp := charPlanets{ID: ch.ID, Name: ch.Name, Account: ch.Account, Used: len(views)}
			cp.Slots = s.planetSlots(views, sheet, now)
			for _, sl := range cp.Slots {
				if !sl.Locked {
					cp.Allowed++
				}
				for _, e := range sl.Extractors {
					if !e.Idle {
						cp.YieldHr += e.ProgHour
					}
				}
			}
			blocks[i] = cp
		}(i, ch)
	}
	wg.Wait()

	var (
		active []charPlanets
		bare   []string
		totals planetTotals
		alerts []colonyAlert
		yields = map[int64]*yieldRow{}
	)
	for _, cp := range blocks {
		totals.Allowed += cp.Allowed
		totals.Colonies += cp.Used
		totals.YieldHr += cp.YieldHr
		if cp.Used == 0 {
			bare = append(bare, cp.Name)
			continue
		}
		totals.Chars++
		active = append(active, cp)
		for _, sl := range cp.Slots {
			if sl.Locked || sl.Empty {
				continue
			}
			totals.ExtAll += len(sl.Extractors)
			totals.FacAll += sl.FacTotal
			totals.FacRun += sl.FacRun
			if sl.LoadPc >= 90 {
				totals.StorFull++
			}
			for _, e := range sl.Extractors {
				if e.Idle {
					continue
				}
				totals.ExtRun++
				y := yields[e.ProductID]
				if y == nil {
					y = &yieldRow{TypeID: e.ProductID, Name: e.Product}
					yields[e.ProductID] = y
				}
				y.PerHour += e.ProgHour
				y.Rigs++
			}
			items := slotAlerts(sl, now)
			if len(items) == 0 {
				continue
			}
			row := colonyAlert{
				Level: "warn", CharID: cp.ID, Char: cp.Name,
				Planet: sl.PlanetName, Parts: items, rank: 1,
			}
			for _, it := range items {
				if it.Level == "err" {
					row.Level, row.rank = "err", 0
					totals.AlertsErr++
				} else {
					totals.AlertsWarn++
				}
			}
			alerts = append(alerts, row)
		}
	}
	totals.Free = totals.Allowed - totals.Colonies
	// Colonies where something has already stopped come first.
	sort.SliceStable(alerts, func(i, j int) bool { return alerts[i].rank < alerts[j].rank })

	rows := make([]yieldRow, 0, len(yields))
	for _, y := range yields {
		rows = append(rows, *y)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].PerHour > rows[j].PerHour })

	data["Chars"] = active
	data["NoColonies"] = bare
	data["Totals"] = totals
	data["Alerts"] = alerts
	data["Yields"] = rows
	data["Now"] = now
	data["Errors"] = errs.list
	s.render(w, "empire_planets", data, stale)
}

// slotAlerts turns one colony into the lines worth acting on: a dead
// extraction program, stalled factories, a storage about to overflow.
func slotAlerts(sl planetSlot, now time.Time) []planetAlert {
	var out []planetAlert
	add := func(level, what, note string) {
		out = append(out, planetAlert{Level: level, What: what, Note: note})
	}

	for _, e := range sl.Extractors {
		switch {
		case e.Expiry.IsZero():
		case !e.Expiry.After(now):
			add("err", "программа бура завершена", e.Product)
		case e.Expiry.Sub(now) < 24*time.Hour:
			add("warn", "программа бура истекает через "+humanUntil(e.Expiry, now), e.Product)
		}
		// Yield decays: at some point it stops covering the factories.
		if e.CoverCycle > 0 && !e.Idle {
			note := e.Product
			if e.CoverAt != "" {
				note += " · с " + e.CoverAt
			}
			add("warn", "добычи перестанет хватать заводам", note)
		}
	}

	switch {
	case !sl.FacHas:
	case sl.FacRun == 0:
		add("err", "заводы стоят", fmt.Sprintf("все %d", sl.FacTotal))
	case sl.FacRun < sl.FacTotal:
		add("warn", "часть заводов стоит",
			fmt.Sprintf("работают %d из %d", sl.FacRun, sl.FacTotal))
	}

	if sl.CapM3 > 0 {
		switch {
		case sl.LoadPc >= 90:
			add("err", "склады переполняются", fmt.Sprintf("%.0f %%", sl.LoadPc))
		case sl.LoadPc >= 70:
			add("warn", "склады заполняются", fmt.Sprintf("%.0f %%", sl.LoadPc))
		}
	}
	// What already stopped goes above what is only going to.
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].Level == "err" && out[j].Level != "err"
	})
	return out
}

// storSeg is one coloured slice of the storage ring. A linear gradient
// would paint a half-full colony red, so the ring is drawn as segments
// instead and doubles as its own scale: the colour at a given angle
// always means the same load — green at empty, yellow at half, red at
// the brim.
type storSeg struct {
	Color string
	Dash  string // stroke-dasharray
	Off   string // stroke-dashoffset
}

// storRingLen is the circumference of the outer ring in the 100x100 box.
const storRingLen = 282.74

func storageRing(pc float64) []storSeg {
	if pc <= 0 {
		return nil
	}
	full := math.Min(pc, 100) / 100
	const n = 12
	segs := make([]storSeg, 0, n)
	for i := 0; i < n; i++ {
		a, b := float64(i)/n, float64(i+1)/n
		if a >= full {
			break
		}
		draw := math.Min(b, full)
		end := draw
		if draw < full { // a hair of overlap hides the seams
			end = math.Min(draw+0.004, 1)
		}
		segs = append(segs, storSeg{
			Color: loadColor((a + draw) / 2),
			Dash:  fmt.Sprintf("%.2f %.2f", storRingLen*(end-a), storRingLen),
			Off:   fmt.Sprintf("%.2f", -storRingLen*a),
		})
	}
	return segs
}

// loadColor walks green → yellow → red as the fraction goes 0 → 1.
func loadColor(f float64) string {
	stops := [3][3]float64{{95, 211, 141}, {224, 178, 95}, {224, 108, 108}}
	from, to, t := stops[0], stops[1], f/0.5
	if f >= 0.5 {
		from, to, t = stops[1], stops[2], (f-0.5)/0.5
	}
	c := [3]int{}
	for i := range c {
		c[i] = int(from[i] + (to[i]-from[i])*math.Max(0, math.Min(1, t)) + 0.5)
	}
	return fmt.Sprintf("#%02x%02x%02x", c[0], c[1], c[2])
}

// slotFeed is one material the colony's factories eat, with what is
// lying around for it right now.
type slotFeed struct {
	TypeID  int64
	Name    string
	Stock   int64 // everything held anywhere in the colony
	PerCyc  int64 // consumed per cycle by all factories needing it
	Cycles  int64 // how many full cycles the stock covers
	Starved bool  // not even one cycle left
}

// planetSlots builds the six tiles of the overview card.
func (s *Server) planetSlots(views []colonyView, sheet *esi.SkillSheet, now time.Time) []planetSlot {
	allowed := 1
	if sheet != nil {
		for _, sk := range sheet.Skills {
			if sk.SkillID == skillInterplanetary {
				allowed += sk.ActiveLevel
				break
			}
		}
	}
	if allowed > maxPlanets {
		allowed = maxPlanets
	}

	slots := make([]planetSlot, 0, maxPlanets)
	for _, v := range views {
		slot := planetSlot{
			Now:          now,
			PlanetName:   v.PlanetName,
			PlanetType:   v.PlanetType,
			PlanetTypeID: sde.PlanetTypeID(v.PlanetType),
			SystemName:   v.SystemName,
			UpgradeLevel: v.UpgradeLevel,
			PlanetID:     v.PlanetID,
			Extractors:   v.Extractors,
		}
		if slot.PlanetTypeID != 0 {
			if n := s.SDE.TypeNames([]int64{slot.PlanetTypeID})[slot.PlanetTypeID]; n != "" {
				slot.PlanetType = n
			}
		}
		// Extraction: the ring follows the cycle that finishes first.
		slot.ExtHas = len(v.Extractors) > 0
		slot.ExtIdle = slot.ExtHas
		for _, e := range v.Extractors {
			if e.Idle || !e.CycleEnds.After(now) {
				continue
			}
			slot.ExtIdle = false
			if slot.ExtEnds.IsZero() || e.CycleEnds.Before(slot.ExtEnds) {
				slot.ExtEnds, slot.ExtSec = e.CycleEnds, e.CycleSec
			}
		}
		// Production: same idea over the factories that are running.
		slot.FacHas = len(v.Factories) > 0
		slot.FacTotal = len(v.Factories)
		for _, f := range v.Factories {
			if f.Idle {
				continue
			}
			slot.FacRun++
			if f.CycleEnds.After(now) &&
				(slot.FacEnds.IsZero() || f.CycleEnds.Before(slot.FacEnds)) {
				slot.FacEnds, slot.FacSec = f.CycleEnds, f.CycleSec
			}
		}
		slot.FacIdle = slot.FacHas && slot.FacRun == 0

		// Storage load over the whole colony. The command centre is a
		// staging box, not a warehouse — it is left out on purpose.
		for _, st := range v.Storage {
			if st.Role == "cc" {
				continue
			}
			slot.Storage = append(slot.Storage, st)
			slot.UsedM3 += st.UsedM3
			slot.CapM3 += st.CapM3
		}
		if slot.CapM3 > 0 {
			slot.LoadPc = slot.UsedM3 / slot.CapM3 * 100
			if slot.LoadPc > 100 {
				slot.LoadPc = 100
			}
			slot.StorRing = storageRing(slot.LoadPc)
		}
		slot.Feed = colonyFeed(v)
		slots = append(slots, slot)
	}

	for len(slots) < maxPlanets {
		slots = append(slots, planetSlot{
			Empty:  len(slots) < allowed,
			Locked: len(slots) >= allowed,
		})
	}
	return slots[:maxPlanets]
}

// colonyFeed tallies what the colony's factories need against what it
// actually holds, anywhere — the "materials to process" line of the
// tooltip. Demand is summed per cycle over every factory using it.
func colonyFeed(v colonyView) []slotFeed {
	demand := map[int64]int64{}
	names := map[int64]string{}
	for _, f := range v.Factories {
		for _, in := range f.Inputs {
			demand[in.TypeID] += in.Amount
			names[in.TypeID] = in.Name
		}
	}
	if len(demand) == 0 {
		return nil
	}
	stock := map[int64]int64{}
	for _, p := range v.Pins {
		for _, c := range p.Contents {
			if _, ok := demand[c.TypeID]; ok {
				stock[c.TypeID] += c.Amount
				if names[c.TypeID] == "" {
					names[c.TypeID] = c.Name
				}
			}
		}
	}
	out := make([]slotFeed, 0, len(demand))
	for id, per := range demand {
		f := slotFeed{TypeID: id, Name: names[id], Stock: stock[id], PerCyc: per}
		if per > 0 {
			f.Cycles = f.Stock / per
			f.Starved = f.Cycles == 0
		}
		out = append(out, f)
	}
	// Scarcest first: that is what the tooltip is being opened for.
	sort.Slice(out, func(i, j int) bool {
		if out[i].Cycles != out[j].Cycles {
			return out[i].Cycles < out[j].Cycles
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// Colony map geometry, in logical pixels of a 3:2 box. The pin is 46px
// across and neighbours keep a 10px gap, so centres sit 56px apart.
const (
	mapW    = 760.0
	mapH    = 507.0
	pinSize = 46.0
	pinStep = 56.0
)

// layoutColony arranges pins by role instead of by their real
// coordinates: command centre top-left, extractors top-right,
// launchpads in the middle and factories in rings around them. Real
// colonies pack structures tighter than any readable diagram can, so a
// schematic layout beats a faithful projection here.
func layoutColony(pins []planetPinView) {
	pct := func(x, y float64) (float64, float64) {
		return x / mapW * 100, y / mapH * 100
	}
	var cc, extractors, launchpads, storage, factories []int
	for i := range pins {
		switch pins[i].Role {
		case "cc":
			cc = append(cc, i)
		case "extractor":
			extractors = append(extractors, i)
		case "launchpad":
			launchpads = append(launchpads, i)
		case "storage":
			storage = append(storage, i)
		default:
			factories = append(factories, i)
		}
	}

	// Command centres: top-left corner, stacked downwards.
	for n, i := range cc {
		pins[i].X, pins[i].Y = pct(52, 46+float64(n)*pinStep)
	}
	// Extractors: right edge, stacked downwards — a row of them would run
	// into the factory rings on a narrow map. They stand half a pin apart
	// so the coloured rings do not merge into one blob.
	for n, i := range extractors {
		pins[i].X, pins[i].Y = pct(mapW-52, 46+float64(n)*pinSize*1.5)
	}

	// Launchpads (with storages next to them) form the hub in the middle.
	hub := append(append([]int{}, launchpads...), storage...)
	cx, cy := mapW/2, mapH/2+18
	switch len(hub) {
	case 0:
	case 1:
		pins[hub[0]].X, pins[hub[0]].Y = pct(cx, cy)
	default:
		// small ring so the hub itself stays compact
		r := pinStep / (2 * math.Sin(math.Pi/float64(len(hub))))
		if r < pinStep*0.6 {
			r = pinStep * 0.6
		}
		for n, i := range hub {
			a := 2 * math.Pi * float64(n) / float64(len(hub))
			pins[i].X, pins[i].Y = pct(cx+r*math.Cos(a), cy+r*math.Sin(a))
		}
	}

	// Factories: concentric rings around the hub, each ring as full as
	// the 10px gap allows.
	hubR := pinStep * 0.6
	if len(hub) > 1 {
		hubR = pinStep / (2 * math.Sin(math.Pi/float64(len(hub))))
	}
	placed := 0
	ring := 1
	for placed < len(factories) {
		r := hubR + float64(ring)*pinStep*1.05
		capacity := int(math.Floor(2 * math.Pi * r / pinStep))
		if capacity < 1 {
			capacity = 1
		}
		left := len(factories) - placed
		n := capacity
		if left < n {
			n = left
		}
		// Offset every other ring so pins don't line up radially.
		offset := 0.0
		if ring%2 == 0 {
			offset = math.Pi / float64(n)
		}
		for k := 0; k < n; k++ {
			a := 2*math.Pi*float64(k)/float64(n) + offset
			i := factories[placed+k]
			pins[i].X, pins[i].Y = pct(cx+r*math.Cos(a), cy+r*math.Sin(a))
		}
		placed += n
		ring++
	}

	// Keep everything inside the box.
	for i := range pins {
		pins[i].X = math.Max(4, math.Min(96, pins[i].X))
		pins[i].Y = math.Max(6, math.Min(94, pins[i].Y))
	}
}

// markExtractorCoverage finds the moment from which the extraction of a
// material no longer keeps the factories that eat it busy.
//
// The extractor curve starts well above what the colony can process and
// decays, so the early surplus piles up in storage and carries the
// factories for a while after the yield itself has dropped below their
// appetite. The mark is therefore the moment at which the *running
// total* falls behind the running demand — that is when the buffer is
// gone and the factories start waiting.
//
// Extractors of the same material are counted together: their heads feed
// one shared pool of storages and factories, so the deficit is a property
// of the colony, not of a single unit. It is found once on a common time
// axis (programs may be installed at different moments and run different
// cycle lengths) and then projected onto each of their charts, which is
// why the mark can land on a different cycle number of each program.
func markExtractorCoverage(v *colonyView) {
	// What the factories eat, per material, in units per second.
	rate := map[int64]float64{}
	eaters := map[int64]int{}
	for _, f := range v.Factories {
		if f.CycleSec <= 0 {
			continue
		}
		for _, in := range f.Inputs {
			rate[in.TypeID] += float64(in.Amount) / float64(f.CycleSec)
			eaters[in.TypeID]++
		}
	}

	group := map[int64][]int{}
	var products []int64
	for i := range v.Extractors {
		e := &v.Extractors[i]
		if e.ProductID == 0 || e.CycleSec <= 0 || e.Install.IsZero() ||
			len(e.prog) == 0 || len(e.Bars) != len(e.prog) {
			continue
		}
		if group[e.ProductID] == nil {
			products = append(products, e.ProductID)
		}
		group[e.ProductID] = append(group[e.ProductID], i)
	}

	for _, prod := range products {
		idx := group[prod]
		r := rate[prod]
		if r <= 0 {
			continue
		}
		// Time zero is the start of the earliest program of the group.
		t0 := v.Extractors[idx[0]].Install
		for _, i := range idx {
			if v.Extractors[i].Install.Before(t0) {
				t0 = v.Extractors[i].Install
			}
		}
		for _, i := range idx {
			e := &v.Extractors[i]
			e.CoverDemand = int64(math.Round(r * float64(e.CycleSec)))
			e.CoverFacs = eaters[prod]
			e.CoverPeers = len(idx)
		}

		// Everything extracted by the group up to `sec` seconds after t0.
		// A cycle is counted as it runs, not in one lump at its end, so
		// programs installed minutes apart do not produce phantom dips.
		supply := func(sec float64) float64 {
			var tot float64
			for _, i := range idx {
				e := &v.Extractors[i]
				cyc := float64(e.CycleSec)
				off := e.Install.Sub(t0).Seconds()
				for c, y := range e.prog {
					done := (sec - off - float64(c)*cyc) / cyc
					if done <= 0 {
						break
					}
					if done > 1 {
						done = 1
					}
					tot += float64(y.Qty) * done
				}
			}
			return tot
		}

		// Both curves are piecewise linear between cycle boundaries, so
		// checking the boundaries and interpolating inside the segment
		// that flips the sign gives the crossing exactly.
		var bounds []float64
		for _, i := range idx {
			e := &v.Extractors[i]
			off := e.Install.Sub(t0).Seconds()
			for c := 0; c <= len(e.prog); c++ {
				bounds = append(bounds, off+float64(c)*float64(e.CycleSec))
			}
		}
		sort.Float64s(bounds)

		// Nothing is delivered until a cycle completes, so the balance is
		// meaningless until every extractor of the group has finished its
		// first one — before that the factories are simply waiting for the
		// first load and the running demand would show a phantom deficit.
		warm := 0.0
		for _, i := range idx {
			e := &v.Extractors[i]
			if t := e.Install.Sub(t0).Seconds() + float64(e.CycleSec); t > warm {
				warm = t
			}
		}

		cross, found := 0.0, false
		prevT, prevBal := warm, supply(warm)-r*warm
		if prevBal < 0 {
			cross, found = 0, true // short from the very first cycle
		} else {
			for _, t := range bounds {
				if t <= warm {
					continue
				}
				bal := supply(t) - r*t
				if bal < 0 {
					if d := prevBal - bal; d > 0 {
						cross = prevT + (t-prevT)*prevBal/d
					} else {
						cross = prevT
					}
					found = true
					break
				}
				prevT, prevBal = t, bal
			}
		}
		if !found {
			continue // the whole programme keeps up
		}

		for _, i := range idx {
			e := &v.Extractors[i]
			at := (cross - e.Install.Sub(t0).Seconds()) / float64(e.CycleSec)
			if at < 0 {
				at = 0
			}
			c := int(at)
			if c >= len(e.prog) {
				continue // this programme ends before the deficit starts
			}
			e.CoverCycle = c + 1
			e.CoverX = at / float64(len(e.prog)) * 100
			e.CoverAt = e.Bars[c].At
		}
	}
}

// chartTop is the headroom kept above the tallest bar of the extraction
// chart, in svg units, so the peak does not touch the frame.
const chartTop = 14.0

// programBars turns an extraction program into chart bars laid out in a
// 0..100 box, so the svg can scale to any width. It also returns the
// quarter rules drawn behind them.
func programBars(prog []pi.CycleYield, done int, install time.Time, cycleSec int) ([]progBar, []gridMark) {
	if len(prog) == 0 {
		return nil, nil
	}
	var peak int64
	for _, c := range prog {
		if c.Qty > peak {
			peak = c.Qty
		}
	}
	if peak == 0 {
		return nil, nil
	}
	plot := 100 - chartTop
	slot := 100 / float64(len(prog))
	gap := slot * 0.18
	if gap > 0.5 {
		gap = 0.5
	}
	bars := make([]progBar, 0, len(prog))
	for i, c := range prog {
		h := float64(c.Qty) / float64(peak) * plot
		at := ""
		if !install.IsZero() && cycleSec > 0 {
			at = install.Add(time.Duration(i+1) * time.Duration(cycleSec) * time.Second).
				Format("02.01 15:04")
		}
		bars = append(bars, progBar{
			X: float64(i)*slot + gap/2, W: slot - gap,
			Y: 100 - h, H: h,
			Qty: c.Qty, Cycle: i + 1, Past: i < done, At: at,
		})
	}
	grid := make([]gridMark, 0, 4)
	for _, pc := range []int{25, 50, 75, 100} {
		f := float64(pc) / 100
		grid = append(grid, gridMark{
			Y: 100 - f*plot, Pc: pc, Val: int64(float64(peak) * f),
		})
	}
	return bars, grid
}

// prettyJSON indents an ESI payload for the raw view.
func prettyJSON(raw json.RawMessage) string {
	var buf bytes.Buffer
	if err := json.Indent(&buf, raw, "", "  "); err != nil {
		return string(raw)
	}
	return buf.String()
}

// jobRow merges personal and corporation jobs on the character page;
// the whole struct is marshalled into the row for the details modal.
type jobRow struct {
	esi.IndustryJob
	IsCorp    bool   `json:"is_corp"`
	Installer string `json:"installer_name,omitempty"`
}

func (s *Server) handleIndustry(w http.ResponseWriter, r *http.Request) {
	ec, stale := s.esiFor(r)
	data, selected, ok := s.shellFor(w, r, ec, "industry")
	if !ok {
		return
	}
	id := selected.ID
	var errs errList

	var rows []jobRow
	personal, err := ec.IndustryJobs(id)
	if err != nil {
		errs.add("индустрия", err)
	}
	for _, j := range personal {
		rows = append(rows, jobRow{IndustryJob: j})
	}

	// Corporation jobs installed by this character, marked "корп".
	var corpNote string
	if corpID, _, err := ec.CharacterPublic(id); err == nil && corpID != 0 {
		corpJobs, err := ec.CorporationIndustryJobs(id, corpID)
		switch {
		case err == nil:
			for _, cj := range corpJobs {
				if cj.InstallerID == id {
					rows = append(rows, jobRow{IndustryJob: cj.IndustryJob, IsCorp: true, Installer: cj.Installer})
				}
			}
		case strings.Contains(err.Error(), "403"):
			corpNote = "корпоративные джобы недоступны (нужна роль Factory Manager)"
		default:
			errs.add("корп. индустрия", err)
		}
	}

	sort.Slice(rows, func(i, j int) bool { return rows[i].EndDate.Before(rows[j].EndDate) })

	data["Jobs"] = rows
	data["CorpNote"] = corpNote
	data["Errors"] = errs.list
	s.render(w, "industry", data, stale)
}

func (s *Server) handleMarket(w http.ResponseWriter, r *http.Request) {
	ec, stale := s.esiFor(r)
	data, selected, ok := s.shellFor(w, r, ec, "market")
	if !ok {
		return
	}
	var errs errList
	orders, err := ec.MarketOrders(selected.ID)
	if err != nil {
		errs.add("ордера", err)
	}
	s.markOutbid(ec, selected.ID, orders)
	data["Orders"] = orders
	data["Errors"] = errs.list
	s.render(w, "market", data, stale)
}

// markOutbid checks each order against the live book at its location:
// a sell order is outbid when a cheaper foreign sell exists, a buy
// order when a higher foreign buy exists. Book fetch failures (e.g. no
// cache yet in stale mode) simply leave the flag unset.
func (s *Server) markOutbid(ec *esi.Client, charID int64, orders []esi.MarketOrder) {
	mine := map[int64]bool{}
	for _, o := range orders {
		mine[o.OrderID] = true
	}

	type pair struct{ typeID, locID int64 }
	depths := map[pair]*esi.MarketDepth{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	seen := map[pair]bool{}
	for _, o := range orders {
		p := pair{o.TypeID, o.LocationID}
		if seen[p] {
			continue
		}
		seen[p] = true
		wg.Add(1)
		go func(p pair) {
			defer wg.Done()
			if d, err := ec.MarketOrdersAt(charID, p.typeID, p.locID); err == nil {
				mu.Lock()
				depths[p] = d
				mu.Unlock()
			}
		}(p)
	}
	wg.Wait()

	for i := range orders {
		d := depths[pair{orders[i].TypeID, orders[i].LocationID}]
		if d == nil {
			continue
		}
		if orders[i].IsBuyOrder {
			for _, b := range d.Buys {
				if !mine[b.OrderID] && b.Price > orders[i].Price {
					orders[i].Outbid = true
					break
				}
			}
		} else {
			for _, sell := range d.Sells {
				if !mine[sell.OrderID] && sell.Price < orders[i].Price {
					orders[i].Outbid = true
					break
				}
			}
		}
	}
}

// handleMarketDepth returns the live order book for one of the pilot's
// orders (modal on the market tab). Always fresh — it's an explicit click.
func (s *Server) handleMarketDepth(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	charID, _ := strconv.ParseInt(q.Get("char"), 10, 64)
	typeID, _ := strconv.ParseInt(q.Get("type"), 10, 64)
	locID, _ := strconv.ParseInt(q.Get("location"), 10, 64)
	if charID == 0 || typeID == 0 || locID == 0 {
		http.Error(w, "char, type, location required", http.StatusBadRequest)
		return
	}
	depth, err := s.ESI.MarketOrdersAt(charID, typeID, locID)
	if err != nil {
		writeJSON(w, map[string]any{"error": err.Error()})
		return
	}

	// Mark the pilot's own orders for highlighting.
	if own, err := s.ESI.MarketOrders(charID); err == nil {
		mine := map[int64]bool{}
		for _, o := range own {
			mine[o.OrderID] = true
		}
		for i := range depth.Sells {
			depth.Sells[i].IsMine = mine[depth.Sells[i].OrderID]
		}
		for i := range depth.Buys {
			depth.Buys[i].IsMine = mine[depth.Buys[i].OrderID]
		}
	}

	// Price/volume history is best-effort — the modal renders without it.
	history, histErr := s.ESI.MarketHistory(charID, typeID, locID, 30)
	resp := map[string]any{"sells": depth.Sells, "buys": depth.Buys, "history": history}
	if histErr != nil {
		resp["history_error"] = histErr.Error()
	}
	writeJSON(w, resp)
}

// ── corporation pages ────────────────────────────────────────────────

func (s *Server) corpFor(w http.ResponseWriter, r *http.Request, ec *esi.Client) (map[string]any, *corpEntry, bool) {
	corpID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return nil, nil, false
	}
	data, _, err := s.shell(ec, 0, "")
	if err != nil {
		httpError(w, "loading characters", err)
		return nil, nil, false
	}
	corps, _ := data["Corporations"].([]corpEntry)
	for i := range corps {
		if corps[i].ID == corpID {
			data["Corp"] = corps[i]
			return data, &corps[i], true
		}
	}
	http.NotFound(w, r)
	return nil, nil, false
}

func (s *Server) handleCorpInfo(w http.ResponseWriter, r *http.Request) {
	ec, stale := s.esiFor(r)
	data, corp, ok := s.corpFor(w, r, ec)
	if !ok {
		return
	}
	var errs errList
	info, err := ec.CorporationInfo(corp.ID)
	if err != nil {
		errs.add("информация", err)
		info = &esi.CorpInfo{Name: corp.Name}
	}
	data["Info"] = info
	data["Errors"] = errs.list
	s.render(w, "corp_info", data, stale)
}

func (s *Server) handleCorpProjects(w http.ResponseWriter, r *http.Request) {
	ec, stale := s.esiFor(r)
	data, corp, ok := s.corpFor(w, r, ec)
	if !ok {
		return
	}
	var errs errList
	var projects []projectView
	raw, err := ec.CorporationProjects(corp.ViaCharID, corp.ID)
	if err != nil {
		if strings.Contains(err.Error(), "403") {
			errs.add("проекты", fmt.Errorf("нет доступа — нужен scope esi-corporations.read_projects.v1 (перелогинься) или права в корпорации"))
		} else {
			errs.add("проекты", err)
		}
	}
	for _, p := range raw {
		projects = append(projects, toProjectView(p))
	}
	data["Projects"] = projects
	data["Errors"] = errs.list
	s.render(w, "projects", data, stale)
}

func (s *Server) handleCorpIndustry(w http.ResponseWriter, r *http.Request) {
	ec, stale := s.esiFor(r)
	data, corp, ok := s.corpFor(w, r, ec)
	if !ok {
		return
	}
	var errs errList
	jobs, err := ec.CorporationIndustryJobs(corp.ViaCharID, corp.ID)
	if err != nil {
		if strings.Contains(err.Error(), "403") {
			errs.add("индустрия", fmt.Errorf("нет доступа — нужна роль Factory Manager в корпорации"))
		} else {
			errs.add("индустрия", err)
		}
	}
	paused := 0
	for _, j := range jobs {
		if j.Status == "paused" {
			paused++
		}
	}
	data["Jobs"] = jobs
	data["Paused"] = paused
	data["Errors"] = errs.list
	s.render(w, "corp_industry", data, stale)
}

func (s *Server) handleCorpWallets(w http.ResponseWriter, r *http.Request) {
	ec, stale := s.esiFor(r)
	data, corp, ok := s.corpFor(w, r, ec)
	if !ok {
		return
	}
	division, _ := strconv.Atoi(r.URL.Query().Get("division"))
	if division < 1 || division > 7 {
		division = 1
	}
	tab := r.URL.Query().Get("tab")
	switch tab {
	case "invoices", "journal", "market", "divisions":
	default:
		tab = "divisions"
	}

	var errs errList
	wallets, err := ec.CorporationWallets(corp.ViaCharID, corp.ID)
	if err != nil {
		if strings.Contains(err.Error(), "403") {
			errs.add("кошельки", fmt.Errorf("нет доступа — нужна роль Accountant в корпорации и scope esi-wallet.read_corporation_wallets.v1"))
		} else {
			errs.add("кошельки", err)
		}
	}

	var (
		wg      sync.WaitGroup
		journal []esi.JournalEntry
		txns    []esi.Transaction
	)
	if len(wallets) > 0 {
		wg.Add(2)
		go func() {
			defer wg.Done()
			var err error
			if journal, err = ec.CorporationWalletJournal(corp.ViaCharID, corp.ID, division); err != nil {
				errs.add("журнал", err)
			}
		}()
		go func() {
			defer wg.Done()
			var err error
			if txns, err = ec.CorporationWalletTransactions(corp.ViaCharID, corp.ID, division); err != nil {
				errs.add("транзакции", err)
			}
		}()
	}

	// Custom division names need the Director role — fall back to
	// generic labels quietly.
	names, _ := ec.CorporationWalletNames(corp.ViaCharID, corp.ID)
	wg.Wait()

	type walletCard struct {
		Division int
		Name     string
		Balance  float64
	}
	var cards []walletCard
	var total, selBalance float64
	selName := "Главный счёт"
	for _, wl := range wallets {
		name := names[wl.Division]
		if name == "" {
			if wl.Division == 1 {
				name = "Главный счёт"
			} else {
				name = fmt.Sprintf("Счёт №%d", wl.Division)
			}
		}
		cards = append(cards, walletCard{Division: wl.Division, Name: name, Balance: wl.Balance})
		total += wl.Balance
		if wl.Division == division {
			selBalance, selName = wl.Balance, name
		}
	}

	type journalRow struct {
		esi.JournalEntry
		TypeRu string
	}
	jrows := make([]journalRow, len(journal))
	for i, j := range journal {
		jrows[i] = journalRow{JournalEntry: j, TypeRu: refTypeRu(j.RefType)}
	}

	data["Wallets"] = cards
	data["Total"] = total
	data["Division"] = division
	data["SelBalance"] = selBalance
	data["SelName"] = selName
	data["Tab"] = tab
	data["Journal"] = jrows
	data["Transactions"] = txns
	data["Errors"] = errs.list
	s.render(w, "corp_wallets", data, stale)
}

// projectView is a template-friendly, schema-agnostic project rendering.
type projectView struct {
	Title  string
	Fields []kv
}
type kv struct{ K, V string }

func toProjectView(m map[string]any) projectView {
	v := projectView{Title: "Проект"}
	if n, ok := m["name"].(string); ok {
		v.Title = n
	}
	var keys []string
	for k := range m {
		if k != "name" {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		val := m[k]
		var str string
		switch x := val.(type) {
		case string, float64, bool, nil:
			str = fmt.Sprint(x)
		default:
			b, _ := json.Marshal(x)
			str = string(b)
		}
		v.Fields = append(v.Fields, kv{K: k, V: str})
	}
	return v
}

// ── sidebar actions ──────────────────────────────────────────────────

func (s *Server) handleSidebarOrder(w http.ResponseWriter, r *http.Request) {
	var groups []store.SidebarGroup
	if err := json.NewDecoder(r.Body).Decode(&groups); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	if err := s.Store.SaveSidebarOrder(groups); err != nil {
		httpError(w, "saving order", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleBulkWaypoint(w http.ResponseWriter, r *http.Request) {
	var req struct {
		IDs    []int64 `json:"ids"`
		System string  `json:"system"`
		Clear  bool    `json:"clear"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	req.System = strings.TrimSpace(req.System)
	if req.System == "" || len(req.IDs) == 0 {
		http.Error(w, "system and ids required", http.StatusBadRequest)
		return
	}

	destID, err := s.ESI.ResolveSystem(req.System)
	if err != nil {
		writeJSON(w, map[string]any{"error": err.Error()})
		return
	}

	type charResult struct {
		ID    int64  `json:"id"`
		Error string `json:"error,omitempty"`
	}
	results := make([]charResult, len(req.IDs))
	var wg sync.WaitGroup
	for i, id := range req.IDs {
		wg.Add(1)
		go func(i int, id int64) {
			defer wg.Done()
			results[i] = charResult{ID: id}
			if err := s.ESI.SetWaypoint(id, destID, req.Clear); err != nil {
				results[i].Error = err.Error()
			}
		}(i, id)
	}
	wg.Wait()

	writeJSON(w, map[string]any{"system": req.System, "results": results})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

// shellFor resolves the {id} path param and builds shared page data.
func (s *Server) shellFor(w http.ResponseWriter, r *http.Request, ec *esi.Client, section string) (map[string]any, *store.Character, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return nil, nil, false
	}
	data, selected, err := s.shell(ec, id, section)
	if err != nil {
		httpError(w, "loading characters", err)
		return nil, nil, false
	}
	if selected == nil {
		http.NotFound(w, r)
		return nil, nil, false
	}
	return data, selected, true
}

// ── auth & actions ───────────────────────────────────────────────────

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	state, err := randomState()
	if err != nil {
		httpError(w, "generating state", err)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     stateCookie,
		Value:    state,
		Path:     "/",
		MaxAge:   600,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
	http.SetCookie(w, &http.Cookie{
		Name:     backCookie,
		Value:    localPath(r.URL.Query().Get("back")),
		Path:     "/",
		MaxAge:   600,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, s.SSO.AuthorizeURL(state), http.StatusFound)
}

func (s *Server) handleCallback(w http.ResponseWriter, r *http.Request) {
	// CSRF check: state must match the cookie we set at /login.
	cookie, err := r.Cookie(stateCookie)
	if err != nil || cookie.Value == "" || r.URL.Query().Get("state") != cookie.Value {
		http.Error(w, "state mismatch — start login again", http.StatusBadRequest)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: stateCookie, Path: "/", MaxAge: -1})

	code := r.URL.Query().Get("code")
	if code == "" {
		http.Error(w, "missing code", http.StatusBadRequest)
		return
	}

	tok, err := s.SSO.Exchange(code)
	if err != nil {
		httpError(w, "exchanging code", err)
		return
	}

	claims, err := s.SSO.VerifyToken(tok.AccessToken)
	if err != nil {
		httpError(w, "verifying token", err)
		return
	}

	err = s.Store.UpsertCharacter(
		claims.CharacterID, claims.CharacterName,
		tok.RefreshToken, tok.AccessToken,
		time.Now().Add(time.Duration(tok.ExpiresIn)*time.Second),
		claims.Scopes, s.SSO.ClientID,
	)
	if err != nil {
		httpError(w, "saving character", err)
		return
	}

	log.Printf("logged in: %s (%d), %d scopes", claims.CharacterName, claims.CharacterID, len(claims.Scopes))

	back := ""
	if c, err := r.Cookie(backCookie); err == nil {
		back = localPath(c.Value)
	}
	http.SetCookie(w, &http.Cookie{Name: backCookie, Path: "/", MaxAge: -1})
	if back == "" {
		back = fmt.Sprintf("/characters/%d", claims.CharacterID)
	}
	http.Redirect(w, r, back, http.StatusFound)
}

func (s *Server) handleSetAccount(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	account := strings.TrimSpace(r.FormValue("account"))
	if err := s.Store.SetAccount(id, account); err != nil {
		httpError(w, "saving account", err)
		return
	}
	// Аккаунт правят из настроек — возвращаемся туда же, а не на
	// карточку персонажа.
	back := r.FormValue("back")
	if !strings.HasPrefix(back, "/") || strings.HasPrefix(back, "//") {
		back = fmt.Sprintf("/characters/%d", id)
	}
	http.Redirect(w, r, back, http.StatusSeeOther)
}

func (s *Server) handleSetTags(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	var tags []string
	seen := map[string]bool{}
	for _, t := range strings.Split(r.FormValue("tags"), ",") {
		t = strings.TrimSpace(t)
		if t != "" && !seen[t] {
			seen[t] = true
			tags = append(tags, t)
		}
	}
	if err := s.Store.SetTags(id, tags); err != nil {
		httpError(w, "saving tags", err)
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/characters/%d", id), http.StatusFound)
}

func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	if err := s.Store.DeleteCharacter(id); err != nil {
		httpError(w, "deleting character", err)
		return
	}
	http.Redirect(w, r, "/", http.StatusFound)
}

// ── helpers ──────────────────────────────────────────────────────────

type errList struct {
	mu   sync.Mutex
	list []string
}

func (e *errList) add(what string, err error) {
	e.mu.Lock()
	if what != "" {
		e.list = append(e.list, what+": "+err.Error())
	} else {
		e.list = append(e.list, err.Error())
	}
	e.mu.Unlock()
}

func (e *errList) addAll(prefix string, msgs []string) {
	e.mu.Lock()
	for _, m := range msgs {
		if prefix != "" {
			m = prefix + ": " + m
		}
		e.list = append(e.list, m)
	}
	e.mu.Unlock()
}

func httpError(w http.ResponseWriter, what string, err error) {
	log.Printf("error %s: %v", what, err)
	http.Error(w, "internal error: "+what, http.StatusInternalServerError)
}

// localPath keeps only same-site paths, so a crafted ?back= cannot bounce
// the login to another host.
func localPath(p string) string {
	if !strings.HasPrefix(p, "/") || strings.HasPrefix(p, "//") {
		return ""
	}
	return p
}

func randomState() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// iskShort renders a compact balance: 1.2B, 345.6M, 12.3K ISK.
func iskShort(v float64) string {
	abs := v
	if abs < 0 {
		abs = -abs
	}
	switch {
	case abs >= 1e9:
		return fmt.Sprintf("%.1fB ISK", v/1e9)
	case abs >= 1e6:
		return fmt.Sprintf("%.1fM ISK", v/1e6)
	case abs >= 1e3:
		return fmt.Sprintf("%.1fK ISK", v/1e3)
	default:
		return fmt.Sprintf("%.0f ISK", v)
	}
}

// formatISK renders 1234567.89 as "1 234 568 ISK".
func formatISK(v float64) string {
	return formatNum(int64(v+0.5)) + " ISK"
}

// formatNum inserts spaces as thousands separators; accepts any integer kind
// so templates can pass int, int32 or int64 alike.
func formatNum(v any) string {
	var n int64
	switch x := v.(type) {
	case int:
		n = int64(x)
	case int32:
		n = int64(x)
	case int64:
		n = x
	default:
		return fmt.Sprint(v)
	}
	s := strconv.FormatInt(n, 10)
	neg := strings.HasPrefix(s, "-")
	if neg {
		s = s[1:]
	}
	var parts []string
	for len(s) > 3 {
		parts = append([]string{s[len(s)-3:]}, parts...)
		s = s[:len(s)-3]
	}
	parts = append([]string{s}, parts...)
	out := strings.Join(parts, " ")
	if neg {
		out = "-" + out
	}
	return out
}

// humanUntil renders a future time as "3д 4ч" / "2ч 15м".
// humanDur renders a duration as "971д 12ч" / "1ч 46м" / "22м 44с".
func humanDur(d time.Duration) string {
	if d <= 0 {
		return "—"
	}
	days := int(d.Hours()) / 24
	hours := int(d.Hours()) % 24
	mins := int(d.Minutes()) % 60
	secs := int(d.Seconds()) % 60
	switch {
	case days > 0:
		return fmt.Sprintf("%dд %dч", days, hours)
	case hours > 0:
		return fmt.Sprintf("%dч %dм", hours, mins)
	case mins > 0:
		return fmt.Sprintf("%dм %dс", mins, secs)
	default:
		return fmt.Sprintf("%dс", secs)
	}
}

func humanUntil(t, now time.Time) string {
	d := t.Sub(now)
	if d <= 0 {
		return "—"
	}
	days := int(d.Hours()) / 24
	hours := int(d.Hours()) % 24
	mins := int(d.Minutes()) % 60
	switch {
	case days > 0:
		return fmt.Sprintf("%dд %dч", days, hours)
	case hours > 0:
		return fmt.Sprintf("%dч %dм", hours, mins)
	default:
		return fmt.Sprintf("%dм", mins)
	}
}
