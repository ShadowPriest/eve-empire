package web

// The build calculator: what one blueprint costs to run when every
// material is bought off the Jita market.
//
// Two numbers make up the cost and they come from DIFFERENT prices, which
// is the whole reason this page exists:
//   - the materials are bought at the real order book (esi.RegionOrderStats),
//     after the blueprint's ME cut them down;
//   - the installation fee is charged on the ESTIMATED ITEM VALUE — the
//     BASE materials (no ME) at CCP's adjusted_price, times the system
//     cost index plus the facility tax plus the flat SCC surcharge.
//
// Everything else (product price, sales tax, broker fee) only serves the
// verdict: is building it worth more than buying it ready-made.

import (
	"fmt"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"eve-empire/internal/esi"
	"eve-empire/internal/sde"
)

const (
	// A researched BPO is the normal case: an unresearched print is a
	// state you pass through, not one you produce from.
	buildDefaultME = 10
	buildDefaultTE = 20

	skillIndustry    = 3380 // Industry: −4 % job time per level
	skillAdvIndustry = 3388 // Advanced Industry: −3 % job time per level
)

// buildSet is the standing answer to "where do I build and what do the
// brokers take" — kept in app_settings, like the reprocessing window:
// it is the same on every blueprint, unlike the view state in the URL.
type buildSet struct {
	System string  // solar system the job is installed in
	Struct string  // structure hull key, "npc" for a station
	Tax    float64 // facility tax, % of the estimated item value
	Broker float64 // broker fee on a sell order, %
	Sales  float64 // sales tax, %
}

// The system is deliberately EMPTY by default: the obvious guess, Jita,
// carries a manufacturing index of 17 % — nobody builds there, and a
// silently applied index that high makes every verdict look hopeless.
var buildDefaults = buildSet{Struct: "npc", Tax: 0.25, Broker: 3.0, Sales: 4.5}

// buildSettings reads the settings, letting the query string override
// them; anything passed explicitly becomes the new default.
func (s *Server) buildSettings(q url.Values) buildSet {
	set := buildSet{
		System: s.Store.Setting("build_system"),
		Struct: s.Store.Setting("build_struct"),
		Tax:    settingFloat(s.Store.Setting("build_tax"), buildDefaults.Tax),
		Broker: settingFloat(s.Store.Setting("build_broker"), buildDefaults.Broker),
		Sales:  settingFloat(s.Store.Setting("build_sales"), buildDefaults.Sales),
	}
	if set.Struct == "" {
		set.Struct = buildDefaults.Struct
	}
	save := func(key, form string, v *string) {
		if !q.Has(form) {
			return
		}
		*v = strings.TrimSpace(q.Get(form))
		s.Store.SetSetting(key, *v)
	}
	saveF := func(key, form string, v *float64) {
		if !q.Has(form) {
			return
		}
		*v = settingFloat(q.Get(form), *v)
		s.Store.SetSetting(key, strconv.FormatFloat(*v, 'f', -1, 64))
	}
	save("build_system", "sys", &set.System)
	save("build_struct", "struct", &set.Struct)
	saveF("build_tax", "tax", &set.Tax)
	saveF("build_broker", "broker", &set.Broker)
	saveF("build_sales", "sales", &set.Sales)
	return set
}

// settingFloat parses a percentage typed by a human: both "0.25" and
// "0,25" mean a quarter of a per cent.
func settingFloat(s string, def float64) float64 {
	s = strings.TrimSpace(strings.ReplaceAll(s, ",", "."))
	s = strings.TrimSuffix(s, "%")
	v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil || v < 0 {
		return def
	}
	return v
}

// buildModes are the three answers to "what do I do with components".
var buildModes = []struct{ Key, Label, Note string }{
	{modeBuy, "покупать всё", "комплектующие берём готовыми с рынка"},
	{modeAuto, "считать выгоду", "по каждому — что дешевле"},
	{modeMake, "делать всё", "строим всё, у чего есть чертёж"},
}

// idSet parses a comma-separated id list from the query string and gives
// it back normalized, so the URL stays stable across clicks.
func idSet(s string) (map[int64]bool, string) {
	set := map[int64]bool{}
	for _, part := range strings.Split(s, ",") {
		if id, err := strconv.ParseInt(strings.TrimSpace(part), 10, 64); err == nil && id > 0 {
			set[id] = true
		}
	}
	return set, idList(set)
}

func idList(set map[int64]bool) string {
	ids := make([]int64, 0, len(set))
	for id := range set {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	parts := make([]string, len(ids))
	for i, id := range ids {
		parts[i] = strconv.FormatInt(id, 10)
	}
	return strings.Join(parts, ",")
}

// buildToggle is the URL that flips one node between built and bought.
// The two id lists live in the query string, so the state of the whole
// tree stays linkable — same rule as the rest of the page.
func buildToggle(q map[string]string, makeSet, buySet map[int64]bool, n *buildNode) string {
	if !n.CanMake {
		return ""
	}
	mk, by := map[int64]bool{}, map[int64]bool{}
	for id := range makeSet {
		mk[id] = true
	}
	for id := range buySet {
		by[id] = true
	}
	if n.Made {
		delete(mk, n.TypeID)
		by[n.TypeID] = true
	} else {
		delete(by, n.TypeID)
		mk[n.TypeID] = true
	}
	next := make(map[string]string, len(q)+2)
	for k, v := range q {
		next[k] = v
	}
	next["mk"], next["by"] = idList(mk), idList(by)
	return "/tools/build" + queryOf(next)
}

// queryOf renders a query map the way the qs template helper does:
// sorted, empties dropped.
func queryOf(q map[string]string) string {
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		if q[k] == "" {
			continue
		}
		parts = append(parts, k+"="+url.QueryEscape(q[k]))
	}
	if len(parts) == 0 {
		return ""
	}
	return "?" + strings.Join(parts, "&")
}

func countMade(rows []*buildNode) int {
	n := 0
	for _, r := range rows {
		if r.Made {
			n++
		}
	}
	return n
}

// buildOwned is one copy of the blueprint the owner already has.
type buildOwned struct {
	CharName string
	ME, TE   int
	Runs     int64 // −1 on an original
	IsCopy   bool
}

func (s *Server) handleBuildCalc(w http.ResponseWriter, r *http.Request) {
	ec, stale := s.esiFor(r)
	data, _, err := s.shell(ec, 0, "")
	if err != nil {
		httpError(w, "loading characters", err)
		return
	}
	q := r.URL.Query()
	set := s.buildSettings(q)

	// The whole view lives in the URL, so a calculation can be linked.
	query := strings.TrimSpace(q.Get("q"))
	bpID, _ := strconv.ParseInt(q.Get("bp"), 10, 64)
	runs := clampInt(atoiDef(q.Get("runs"), 1), 1, 1_000_000)
	me := clampInt(atoiDef(q.Get("me"), buildDefaultME), 0, 10)
	te := clampInt(atoiDef(q.Get("te"), buildDefaultTE), 0, 20)
	charID, _ := strconv.ParseInt(q.Get("ch"), 10, 64)
	stat := q.Get("p")
	if stat == "" {
		stat = esi.StatSellP98
	}
	mode := q.Get("m")
	switch mode {
	case modeAuto, modeMake:
	default:
		mode = modeBuy
	}
	compME := clampInt(atoiDef(q.Get("cme"), buildDefaultME), 0, 10)
	makeSet, makeList := idSet(q.Get("mk"))
	buySet, buyList := idSet(q.Get("by"))

	qmap := map[string]string{
		"q": query, "bp": q.Get("bp"), "runs": strconv.Itoa(runs),
		"me": strconv.Itoa(me), "te": strconv.Itoa(te), "p": stat, "ch": q.Get("ch"),
		"m": mode, "cme": strconv.Itoa(compME), "mk": makeList, "by": buyList,
	}
	data["Q"] = qmap
	data["Query"] = query
	data["Runs"], data["ME"], data["TE"] = runs, me, te
	data["Stat"], data["Stats"] = stat, oreStats
	data["Mode"], data["Modes"] = mode, buildModes
	data["CompME"] = compME
	data["Set"] = set
	data["SCC"] = esi.SCCSurcharge
	data["CharID"] = charID
	data["NoSDE"] = !s.SDE.Available()

	// No blueprint picked yet: the page is just the search box.
	rec, found := s.SDE.RecipeOf(bpID)
	if !found {
		if query != "" {
			data["Hits"] = s.SDE.SearchRecipes(query, 40)
		}
		s.render(w, "build", data, stale)
		return
	}
	if query == "" {
		// Keep the box filled with what is on screen, so refining the
		// search does not start from an empty field.
		query = rec.ProductName
		data["Query"], qmap["q"] = query, query
	}
	// NOTE: max_production_limit is NOT a cap on a manufacturing job — it
	// is how many runs a COPY of this print may carry. Clamping runs to it
	// silently turned 6618 fuel-block runs into 200. It is shown as a hint
	// next to the runs field and nothing more.
	data["Rec"] = rec
	data["Hits"] = s.SDE.SearchRecipes(query, 40)

	cache := newMatCache(s.SDE)
	mats := cache.of(rec)
	if len(mats) == 0 {
		data["Err"] = "у этого чертежа нет списка материалов в sde.db"
		s.render(w, "build", data, stale)
		return
	}

	// The structure the job sits in: its hull carries a material and a
	// time multiplier, both straight from the SDE.
	structs := s.SDE.IndustryStructures()
	str := structs[0]
	for _, c := range structs {
		if c.Key == set.Struct {
			str = c
		}
	}
	// Reactions get no material bonus from the hull (only rigs move them),
	// and their time multiplier is a different attribute.
	matMul, timeMul := str.Mat, str.Time
	if rec.IsReaction() {
		matMul, timeMul = 1, str.RTime
	}
	data["Structs"], data["Struct"] = structs, str
	data["MatMul"], data["TimeMul"] = matMul, timeMul

	// How deep to walk: buying everything only needs the blueprint's own
	// materials (plus the one-level hint "this would be cheaper to make"),
	// while deciding build-or-buy needs the whole subtree.
	maxDepth := buildDepthBuy
	if mode != modeBuy || len(makeSet) > 0 {
		maxDepth = buildDepthMake
	}
	ids := collectTypes(cache, s.SDE, rec, maxDepth+1)
	volumes := s.SDE.Volumes(ids)
	hulls := s.SDE.ShipTypes(ids)

	// Market data, industry indices and the owner's own copies are four
	// independent trips — do them at once.
	var (
		book     map[int64]esi.OrderStats
		adjusted map[int64]esi.MarketPrice
		indices  esi.CostIndices
		idxErr   error
		owned    []buildOwned
		skillMul = 1.0
		charName string
		wg       sync.WaitGroup
	)
	wg.Add(1)
	go func() {
		defer wg.Done()
		book = ec.RegionOrderBook(esi.RegionTheForge, ids)
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		adjusted, _ = ec.MarketPrices()
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		if set.System == "" {
			return
		}
		_, indices, idxErr = ec.SystemIndex(set.System)
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		owned = s.ownedBlueprints(ec, rec.BlueprintID)
	}()
	if charID != 0 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			skillMul, charName = s.industrySkills(ec, charID, rec)
		}()
	}
	wg.Wait()

	// ── installation fee of the final job ────────────────────────────
	// EIV is built from the BASE quantities: ME cuts what you spend, not
	// what CCP charges you for.
	var eiv float64
	for _, m := range mats {
		eiv += adjusted[m.TypeID].Adjusted * float64(m.Quantity) * float64(runs)
	}
	index := indices.For(rec.Activity)
	feeRate := (index + set.Tax + esi.SCCSurcharge) / 100
	jobFee := eiv * feeRate

	// ── the material bill, build-or-buy per line ─────────────────────
	tree := &buildTree{
		sde: s.SDE, mats: cache, book: book, adjusted: adjusted,
		volumes: volumes, hulls: hulls, stat: stat, feeRate: feeRate,
		compME: compME, mode: mode, makeSet: makeSet, buySet: buySet,
		maxDepth: maxDepth, matMul: str.Mat, te: te,
		timeMul: str.Time, rtimeMul: str.RTime,
	}
	top, matCost := tree.materials(rec, int64(runs), me, 1, map[int64]bool{rec.ProductID: true})
	rows := flatten(top)
	for _, n := range rows {
		n.Toggle = buildToggle(qmap, makeSet, buySet, n)
	}

	units := rec.ProductQty * int64(runs)
	// matCost already contains the sub-jobs' fees (they are part of what
	// a built child costs) — adding SubFee again here would double it.
	total := matCost + jobFee
	perUnit := 0.0
	if units > 0 {
		perUnit = total / float64(units)
	}

	// ── the verdict ──────────────────────────────────────────────────
	prod := book[rec.ProductID]
	sellPrice, buyPrice := prod.SellP98, prod.BuyMax
	revenueOrder := sellPrice * float64(units) * (1 - (set.Broker+set.Sales)/100)
	revenueBuy := buyPrice * float64(units) * (1 - set.Sales/100)

	// Time: blueprint TE, then the industry skills, then the structure.
	// Verified against the client: 6618 fuel-block runs at TE 20 in an
	// Azbel with Industry V and Advanced Industry V come to
	// 900·6618·0.8·0.68·0.8 = 2 592 138 s — the 30D 00:02:18 it shows.
	timeFactor := (1 - float64(te)/100) * skillMul * timeMul
	if rec.IsReaction() {
		// Reactions ignore blueprint TE the same way they ignore ME.
		timeFactor = skillMul * timeMul
	}
	jobTime := time.Duration(float64(rec.Time)*float64(runs)*timeFactor) * time.Second

	// ISK per hour is measured against ALL the work the decision implies:
	// building the components is where a good part of the profit comes
	// from, so their job time belongs in the denominator.
	subTime := time.Duration(tree.SubTime*skillMul) * time.Second
	totalTime := jobTime + subTime

	profit := revenueOrder - total
	margin, iskPerHour := 0.0, 0.0
	if total > 0 {
		margin = profit / total * 100
	}
	if h := totalTime.Hours(); h > 0 {
		iskPerHour = profit / h
	}

	data["Rows"] = rows
	data["MatCost"] = matCost
	data["BuyCost"] = matCost - tree.SubFee // what actually leaves for the market
	data["MatVolume"] = tree.Volume
	data["HullLines"] = tree.Hulls
	data["Missing"] = tree.Missing
	data["SubFee"] = tree.SubFee
	data["SubJobs"] = tree.SubJobs
	data["SubTime"] = subTime
	data["Capped"] = tree.Capped
	data["MadeCount"] = countMade(rows)
	data["EIV"] = eiv
	data["Index"] = index
	data["IndexErr"] = errText(idxErr)
	data["FeeRate"] = feeRate * 100
	data["JobFee"] = jobFee
	data["Total"] = total
	data["Units"] = units
	data["PerUnit"] = perUnit
	data["SellPrice"] = sellPrice
	data["BuyPrice"] = buyPrice
	// Without a market price for the product there is no verdict to give:
	// showing "profit −34M" for something Jita simply does not trade would
	// be a lie about the market, not about the build.
	data["HasProdPrice"] = sellPrice > 0 || buyPrice > 0
	data["RevenueOrder"] = revenueOrder
	data["RevenueBuy"] = revenueBuy
	data["Profit"] = profit
	data["ProfitBuy"] = revenueBuy - total
	data["Margin"] = margin
	data["ISKPerHour"] = iskPerHour
	data["JobTime"] = jobTime
	data["TotalTime"] = totalTime
	data["Owned"] = owned
	data["SkillMul"] = skillMul
	data["SkillChar"] = charName
	data["StatIsBuy"] = esi.IsBuy(stat)

	s.render(w, "build", data, stale)
}

// buildQty is EVE's material formula:
//
//	max(runs, ceil(base × runs × (1 − ME/100) × структура))
//
// Verified against the client on a Hydrogen Fuel Block, 6618 runs, ME 10,
// Azbel (material multiplier 0.99), to the unit on all nine materials.
// TWO THINGS THE ORDER MATTERS FOR: the structure multiplier is applied
// AFTER the ME cut as a separate factor, and the rounding happens once,
// at the very end. The floor of one unit per run is last of all — that is
// why the game asks for 6618 Robotics where the arithmetic gives 5897.
// Reactions have no blueprint ME and no hull material bonus; only rigs
// move them, and rigs are not modelled here.
func buildQty(base int64, runs int64, me int, reaction bool, matMul float64) int64 {
	if reaction {
		return base * runs
	}
	if matMul <= 0 {
		matMul = 1
	}
	v := math.Ceil(float64(base)*float64(runs)*(1-float64(me)/100)*matMul - 1e-9)
	if v < float64(runs) {
		v = float64(runs)
	}
	return int64(v)
}

// ownedBlueprints finds the picked print among the characters' own
// blueprints, so their researched ME/TE can be applied with one click.
func (s *Server) ownedBlueprints(ec *esi.Client, blueprintID int64) []buildOwned {
	chars, err := s.Store.Characters()
	if err != nil {
		return nil
	}
	out := make([][]buildOwned, len(chars))
	var wg sync.WaitGroup
	for i, ch := range chars {
		wg.Add(1)
		go func(i int, charID int64, name string) {
			defer wg.Done()
			bps, err := ec.CharacterBlueprints(charID)
			if err != nil {
				return
			}
			for _, bp := range bps {
				if bp.TypeID != blueprintID {
					continue
				}
				out[i] = append(out[i], buildOwned{
					CharName: name, ME: bp.ME, TE: bp.TE,
					Runs: bp.Runs, IsCopy: bp.IsCopy(),
				})
			}
		}(i, ch.ID, ch.Name)
	}
	wg.Wait()

	var all []buildOwned
	seen := map[string]bool{}
	for _, list := range out {
		for _, o := range list {
			// One line per (character, ME, TE): a stack of identical
			// copies says nothing new.
			key := fmt.Sprintf("%s|%d|%d|%t", o.CharName, o.ME, o.TE, o.IsCopy)
			if seen[key] {
				continue
			}
			seen[key] = true
			all = append(all, o)
		}
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].ME != all[j].ME {
			return all[i].ME > all[j].ME
		}
		return all[i].CharName < all[j].CharName
	})
	return all
}

// industrySkills turns a character's Industry / Advanced Industry into
// the job-time multiplier. Reactions get no blueprint-skill discount, so
// the multiplier stays 1 for them.
func (s *Server) industrySkills(ec *esi.Client, charID int64, rec sde.Recipe) (float64, string) {
	name := ""
	if chars, err := s.Store.Characters(); err == nil {
		for _, ch := range chars {
			if ch.ID == charID {
				name = ch.Name
			}
		}
	}
	if rec.IsReaction() {
		return 1, name
	}
	sheet, err := ec.Skills(charID)
	if err != nil || sheet == nil {
		return 1, name
	}
	level := func(id int64) float64 {
		for _, sk := range sheet.Skills {
			if sk.SkillID == id {
				return float64(sk.TrainedLevel)
			}
		}
		return 0
	}
	return (1 - 0.04*level(skillIndustry)) * (1 - 0.03*level(skillAdvIndustry)), name
}

func atoiDef(s string, def int) int {
	v, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return def
	}
	return v
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
