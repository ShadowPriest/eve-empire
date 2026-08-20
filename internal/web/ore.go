package web

// The ore table: what every rock is worth, per m³, at Jita prices.
//
// A local answer to ore.cerlestes.de. The numbers behind it are ours —
// composition from the SDE (type_materials), prices reduced from the
// live order book of The Forge (esi.RegionOrderBook) — but the way of
// looking at them is that site's idea and worth keeping: mining lasers
// cut a fixed VOLUME per cycle, so a rock is only comparable per m³,
// never per unit.
//
// The percentages next to every price answer one question: "would
// reprocessing this beat selling it as it is?". Both sides are counted
// per UNIT of the traded type, which is what makes compressed and
// batch-compressed rocks directly comparable — one unit of Compressed
// Veldspar refines into exactly what one unit of Veldspar does, one
// unit of the batch-compressed kind into a hundred times that.

import (
	"fmt"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"

	"eve-empire/internal/esi"
	"eve-empire/internal/sde"
)

// oreKinds are the tabs of the tool, in display order.
var oreKinds = []struct{ Key, Label, Amount string }{
	{sde.KindOre, "Руда", "м³"},
	{sde.KindIce, "Лёд", "блоков"},
	{sde.KindMoon, "Лунная руда", "м³"},
	{sde.KindGas, "Газ", ""},
}

// oreStats are the price reductions offered in the settings row.
var oreStats = []struct{ Key, Label, Note string }{
	{esi.StatSellP98, "Продажа, 98-й перцентиль", "минимальный селл без 2 % самого дешёвого объёма"},
	{esi.StatSellMin, "Продажа, минимум", "прямой минимальный селл-ордер"},
	{esi.StatSellP90, "Продажа, 90-й перцентиль", "для тонких рынков"},
	{esi.StatBuyP98, "Скупка, 98-й перцентиль", "максимальный бай без 2 % самого дорогого объёма"},
	{esi.StatBuyMax, "Скупка, максимум", "прямой максимальный бай-ордер"},
	{esi.StatBuyP90, "Скупка, 90-й перцентиль", "для тонких рынков"},
}

// priceCell is one market price with its refine-or-sell verdict.
type priceCell struct {
	Price    float64
	Delta    float64 // refined value over market price, percent
	HasPrice bool
	HasDelta bool
	PerM3    float64
}

// oreVariantRow is one grade inside a family.
type oreVariantRow struct {
	TypeID  int64
	Label   string // "II-Grade" — empty on the base rock
	Bonus   float64
	Mats    []float64 // aligned with the kind's material columns
	Other   string    // materials too rare to deserve a column
	Tiers   []string  // moon table: material list per R-tier
	Refined float64
	Raw     priceCell
	Comp    priceCell
	Batch   priceCell
	Erratic bool
}

// oreRow is one family: the rock and every grade of it.
type oreRow struct {
	Name      string
	NameEn    string
	BaseID    int64
	Volume    float64
	Found     []sde.FoundTag
	Variants  []oreVariantRow
	Yield     float64 // the picked character's real yield on this rock
	YieldNote string  // how that number is made up
	SkillName string
	sortKey   float64
}

// oreMatCol is one material column of the table.
type oreMatCol struct {
	TypeID int64
	Name   string
	Short  string
	Price  float64
}

// moonTiers are the rarity columns of the moon table.
var moonTiers = []string{"R4", "R8", "R16", "R32", "R64", "Минералы"}

// refineSet is the standing answer to "how do I refine": who does it and
// at what station. Kept in app_settings, per kind of rock — it is the
// same on every visit, unlike the view state in the URL.
//
// The three kinds get their own setup NOT because the game refines them
// differently — it does not, the SDE gives the same 0.50 base for ore,
// ice and moon ore, and the rigs carry one multiplier for everything —
// but because they are mined and hauled to different places: the moon
// chunk goes into the nullsec Tatara, the veldspar into whatever NPC
// station is closest.
type refineSet struct {
	CharID           int64
	Struct, Rig, Sec string
}

// refineKinds are the tabs that reprocess anything at all (gas is sold,
// never refined), with the label the settings window shows.
var refineKinds = []struct{ Key, Label string }{
	{sde.KindOre, "Руда"},
	{sde.KindIce, "Лёд"},
	{sde.KindMoon, "Лунная руда"},
}

// refineSettings reads one kind's setup. Settings saved before the split
// (plain "refine_char" & co) still apply to every kind, so nobody has to
// set anything up twice after the update.
func (s *Server) refineSettings(kind string) refineSet {
	get := func(name string) string {
		if v := s.Store.Setting("refine_" + kind + "_" + name); v != "" {
			return v
		}
		return s.Store.Setting("refine_" + name)
	}
	set := refineSet{Struct: get("struct"), Rig: get("rig"), Sec: get("sec")}
	set.CharID, _ = strconv.ParseInt(get("char"), 10, 64)
	if set.Struct == "" {
		set.Struct = "npc"
	}
	if set.Rig == "" {
		set.Rig = "none"
	}
	if set.Sec == "" {
		set.Sec = "hi"
	}
	return set
}

// sameRefineSetup reports whether every kind is set up identically —
// that is what the "одинаково для всех" box in the window starts as.
func (s *Server) sameRefineSetup() bool {
	first := s.refineSettings(refineKinds[0].Key)
	for _, k := range refineKinds[1:] {
		if s.refineSettings(k.Key) != first {
			return false
		}
	}
	return true
}

// handleRefineSettings saves the reprocessing window for one kind (or for
// all of them) and comes back to the exact table view it was opened from.
func (s *Server) handleRefineSettings(w http.ResponseWriter, r *http.Request) {
	kinds := []string{r.FormValue("kind")}
	if r.FormValue("all") != "" {
		kinds = nil
		for _, k := range refineKinds {
			kinds = append(kinds, k.Key)
		}
	}
	for _, kind := range kinds {
		known := false
		for _, k := range refineKinds {
			if k.Key == kind {
				known = true
			}
		}
		if !known {
			continue
		}
		for _, f := range []struct{ name, form string }{
			{"char", "ch"}, {"struct", "struct"}, {"rig", "rig"}, {"sec", "sec"},
		} {
			if err := s.Store.SetSetting("refine_"+kind+"_"+f.name, r.FormValue(f.form)); err != nil {
				httpError(w, "сохранение настроек переработки", err)
				return
			}
		}
	}
	back := r.FormValue("back")
	if !strings.HasPrefix(back, "/tools/ore") {
		back = "/tools/ore"
	}
	http.Redirect(w, r, back, http.StatusSeeOther)
}

// Reprocessing skills that apply to everything, whatever the rock.
const (
	skillReprocessing    = 3385 // +3 % yield per level
	skillReprocessingEff = 3389 // +2 % yield per level
)

// refinery is one character's reprocessing ability: the station's base
// rate, the two general skills, the implants, and a level per ore-
// specific skill. The yield is per ORE, not one number for the table —
// "Simple Ore Processing V" does nothing for Arkonor.
type refinery struct {
	CharID   int64
	CharName string
	Base     float64          // station or structure base rate, 0..1
	General  float64          // the two general skills, as a multiplier
	Implant  float64          // implant bonus, per cent
	Levels   map[int64]int    // ore skill -> trained level
	Names    map[int64]string // ore skill -> name, for the tooltip
}

// yieldFor is the real refining yield of one ore family.
//
//	base × (1 + 0.03·Reprocessing) × (1 + 0.02·Efficiency)
//	     × (1 + 0.02·<ore>Processing) × (1 + implants/100)
func (r *refinery) yieldFor(skillID int64) float64 {
	y := r.Base * r.General * (1 + r.Implant/100)
	if skillID != 0 {
		y *= 1 + 0.02*float64(r.Levels[skillID])
	}
	if y > 1 {
		y = 1
	}
	return y
}

// note explains one family's yield in the tooltip. The skill name comes
// from the SDE, not from the character: an untrained skill is missing
// from the sheet entirely, and that is exactly the case worth naming.
func (r *refinery) note(skillID int64, skillName string) string {
	parts := []string{fmt.Sprintf("база %s %%", pctText(r.Base*100))}
	parts = append(parts, fmt.Sprintf("Переработка сырья %d", r.Levels[skillReprocessing]))
	parts = append(parts, fmt.Sprintf("КПД переработки %d", r.Levels[skillReprocessingEff]))
	if skillID != 0 {
		if skillName == "" {
			skillName = "профильный навык"
		}
		parts = append(parts, fmt.Sprintf("%s %d", skillName, r.Levels[skillID]))
	}
	if r.Implant > 0 {
		parts = append(parts, fmt.Sprintf("импланты +%s %%", trimFloat(r.Implant)))
	}
	return strings.Join(parts, " · ")
}

// loadRefinery reads the character's skills and implants.
func (s *Server) loadRefinery(ec *esi.Client, ch sideChar, base float64, skillIDs []int64) *refinery {
	r := &refinery{
		CharID: ch.ID, CharName: ch.Name, Base: base,
		Levels: map[int64]int{}, Names: map[int64]string{},
	}
	sheet, err := ec.Skills(ch.ID)
	if err != nil {
		return nil
	}
	want := map[int64]bool{skillReprocessing: true, skillReprocessingEff: true}
	for _, id := range skillIDs {
		want[id] = true
	}
	for _, sk := range sheet.Skills {
		if want[sk.SkillID] {
			r.Levels[sk.SkillID] = sk.ActiveLevel
			r.Names[sk.SkillID] = sk.SkillName
		}
	}
	r.General = (1 + 0.03*float64(r.Levels[skillReprocessing])) *
		(1 + 0.02*float64(r.Levels[skillReprocessingEff]))
	if imps, err := ec.Implants(ch.ID); err == nil {
		r.Implant = s.SDE.ImplantRefineBonus(imps)
	}
	return r
}

// moonTierName spells out what an R-rating means.
var moonTierName = map[string]string{
	"R4":  "Повсеместные лунные астероиды",
	"R8":  "Обычные лунные астероиды",
	"R16": "Необычные лунные астероиды",
	"R32": "Редкие лунные астероиды",
	"R64": "Исключительные лунные астероиды",
}

// classicMineral is the eight-mineral backbone of the ore table.
var classicMineral = map[int64]bool{
	34: true, 35: true, 36: true, 37: true, 38: true, 39: true, 40: true, 11399: true,
}

func (s *Server) handleOreTool(w http.ResponseWriter, r *http.Request) {
	ec, stale := s.esiFor(r)
	data, _, err := s.shell(ec, 0, "")
	if err != nil {
		httpError(w, "loading characters", err)
		return
	}
	q := r.URL.Query()

	kind := q.Get("t")
	switch kind {
	case sde.KindIce, sde.KindMoon, sde.KindGas:
	default:
		kind = sde.KindOre
	}
	stat := q.Get("p")
	known := false
	for _, s := range oreStats {
		if s.Key == stat {
			known = true
		}
	}
	if !known {
		stat = esi.StatSellP98
	}
	// Amount: m³ of rock, or blocks of ice. Moon ore is dense enough
	// that a single m³ says nothing — the game mines it by the thousand.
	amount := parseAmount(q.Get("a"), defaultAmount(kind))
	yield := parseYield(q.Get("y"))

	fams := s.SDE.Harvestables(kind)
	chars := empireChars(data)
	if len(fams) == 0 {
		data["Kind"] = kind
		data["Kinds"] = oreKinds
		data["NoSDE"] = true
		s.render(w, "ore", data, stale)
		return
	}

	// ── everything the page needs a price for ──
	var ids []int64
	matSet := map[int64]int{} // material -> how many families yield it
	for _, f := range fams {
		for _, v := range f.Variants {
			ids = append(ids, v.TypeID)
			if v.Compressed != nil {
				ids = append(ids, v.Compressed.TypeID)
			}
			if v.Batch != nil {
				ids = append(ids, v.Batch.TypeID)
			}
		}
		for _, m := range f.MaterialIDs {
			matSet[m]++
		}
	}
	matIDs := make([]int64, 0, len(matSet))
	for m := range matSet {
		matIDs = append(matIDs, m)
	}
	mats := s.SDE.MaterialInfos(matIDs)
	sort.Slice(matIDs, func(i, j int) bool {
		a, b := mats[matIDs[i]], mats[matIDs[j]]
		if ra, rb := matRank(a), matRank(b); ra != rb {
			return ra < rb
		}
		return a.TypeID < b.TypeID
	})
	ids = append(ids, matIDs...)

	book := ec.RegionOrderBook(esi.RegionTheForge, ids)
	price := func(id int64) float64 { return book[id].Pick(stat) }

	// ── whose skills do the refining, and where ──
	// The setup lives in app_settings, not in the URL: it is a standing
	// answer to "how do I refine", the same on every tab, while the rest
	// of the table's state is a view and belongs in the address bar.
	// With a character picked the manual yield field steps aside — the
	// yield is computed per family, because the ore-specific skill is
	// per family too.
	model := s.SDE.RefineryModel()
	set := s.refineSettings(kind)
	var ref *refinery
	if set.CharID != 0 {
		for _, ch := range chars {
			if ch.ID != set.CharID {
				continue
			}
			var skillIDs []int64
			for _, f := range fams {
				if f.SkillID != 0 {
					skillIDs = append(skillIDs, f.SkillID)
				}
			}
			ref = s.loadRefinery(ec, ch, model.Base(set.Struct, set.Rig, set.Sec), skillIDs)
			break
		}
	}
	yieldOf := func(f sde.OreFamily) float64 {
		if ref != nil {
			return ref.yieldFor(f.SkillID)
		}
		return yield
	}

	// ── columns ──
	// A material earns a column of its own once several rocks yield it.
	// The one-offs (Eleutrium out of Tyranite, the crystalline minerals
	// of the X-Grade rocks) would otherwise add a column of dashes each
	// and push the classic eight off the screen — they go to "прочее".
	var cols []oreMatCol
	rare := map[int64]bool{}
	if kind != sde.KindMoon && kind != sde.KindGas {
		for _, id := range matIDs {
			m := mats[id]
			// Ore keeps the eight classic minerals as its columns, come
			// what may: Morphite comes out of Mercoxit alone and still
			// belongs there, while the newer minerals of the X-Grade
			// rocks would each add a column of dashes.
			if kind == sde.KindOre && !classicMineral[id] {
				rare[id] = true
				continue
			}
			cols = append(cols, oreMatCol{TypeID: id, Name: m.Name, Short: shortMat(m.Name), Price: price(id)})
		}
	}
	hasRare := false
	for _, f := range fams {
		for _, m := range f.MaterialIDs {
			if rare[m] {
				hasRare = true
			}
		}
	}

	// ── rows ──
	perM3 := kind != sde.KindIce // ice is mined by the block, not by volume
	rows := make([]oreRow, 0, len(fams))
	for _, f := range fams {
		row := oreRow{Name: f.Name, NameEn: f.NameEn, BaseID: f.BaseID, Volume: f.Volume, Found: f.Found}
		// Moon rocks spawn on moons and nowhere else — the useful thing
		// to say about one is its rarity, which is its group.
		if kind == sde.KindMoon {
			if tier := sde.MoonTier(f.GroupID); tier != "" {
				row.Found = []sde.FoundTag{{Label: tier, Title: moonTierName[tier]}}
			}
		}
		yield := yieldOf(f)
		if ref != nil {
			row.Yield = yield
			row.YieldNote = ref.note(f.SkillID, f.SkillName)
			row.SkillName = f.SkillName
		}
		for _, v := range f.Variants {
			factor := amount
			if perM3 && v.Volume > 0 {
				factor = amount / v.Volume
			}
			vr := oreVariantRow{TypeID: v.TypeID, Label: v.Grade, Bonus: v.Bonus, Erratic: v.Erratic}
			for _, c := range cols {
				vr.Mats = append(vr.Mats, v.PerUnit(c.TypeID)*factor*yield)
			}
			if hasRare {
				var extra []string
				for _, id := range matIDs {
					if !rare[id] {
						continue
					}
					if q := v.PerUnit(id) * factor * yield; q > 0 {
						extra = append(extra, fmt.Sprintf("%s %s", formatQty(q), mats[id].Name))
					}
				}
				vr.Other = strings.Join(extra, " · ")
			}
			if kind == sde.KindMoon {
				vr.Tiers = moonTierCells(v.OreType, mats, factor*yield)
			}
			// Refined value of the displayed amount, and — for the price
			// verdicts — of a single unit of each traded type.
			unit := refinedPerUnit(&v.OreType, price, yield)
			vr.Refined = unit * factor
			vr.Raw = cell(price(v.TypeID), unit, v.Volume)
			if v.Compressed != nil {
				vr.Comp = cell(price(v.Compressed.TypeID),
					refinedPerUnit(v.Compressed, price, yield), v.Compressed.Volume)
			}
			if v.Batch != nil {
				vr.Batch = cell(price(v.Batch.TypeID),
					refinedPerUnit(v.Batch, price, yield), v.Batch.Volume)
			}
			row.Variants = append(row.Variants, vr)
		}
		if len(row.Variants) == 0 {
			continue
		}
		row.sortKey = row.Variants[0].Refined
		rows = append(rows, row)
	}

	// ── sorting: name by default, any value column on demand ──
	sortKey, desc := q.Get("s"), q.Get("d") != "asc"
	switch {
	case sortKey == "" || sortKey == "name":
		sort.Slice(rows, func(i, j int) bool {
			if q.Get("d") == "desc" {
				return rows[i].Name > rows[j].Name
			}
			return rows[i].Name < rows[j].Name
		})
	case sortKey == "refined":
		sortRows(rows, desc, func(r oreRow) float64 { return r.sortKey })
	case sortKey == "raw":
		sortRows(rows, desc, func(r oreRow) float64 { return r.Variants[0].Raw.PerM3 })
	case strings.HasPrefix(sortKey, "m"):
		if id, err := strconv.ParseInt(strings.TrimPrefix(sortKey, "m"), 10, 64); err == nil {
			idx := -1
			for i, c := range cols {
				if c.TypeID == id {
					idx = i
				}
			}
			if idx >= 0 {
				sortRows(rows, desc, func(r oreRow) float64 { return r.Variants[0].Mats[idx] })
			}
		}
	}

	// The whole state of the table lives in the address bar: every
	// setting and every sortable header is a plain link, so the back
	// button works and a view can be shared as it is.
	data["Q"] = map[string]string{
		"t": kind, "a": q.Get("a"), "y": q.Get("y"), "p": stat,
		"v": q.Get("v"), "c": q.Get("c"), "b": q.Get("b"), "f": q.Get("f"),
		"s": q.Get("s"), "d": q.Get("d"),
	}
	data["Chars"] = chars
	data["Model"] = model
	data["Set"] = set
	data["SetCharID"] = strconv.FormatInt(set.CharID, 10)
	data["SetBase"] = pctText(model.Base(set.Struct, set.Rig, set.Sec) * 100)
	data["SetWhere"] = model.Describe(set.Struct, set.Rig, set.Sec)
	data["SameSetup"] = s.sameRefineSetup()
	data["BackURL"] = r.URL.RequestURI()
	for _, k := range refineKinds {
		if k.Key == kind {
			data["KindLabel"] = k.Label
		}
	}
	// What the other kinds are set to, so the window says out loud that
	// they exist and are not being touched.
	var others []string
	for _, k := range refineKinds {
		if k.Key == kind {
			continue
		}
		o := s.refineSettings(k.Key)
		who := "вручную"
		for _, ch := range chars {
			if ch.ID == o.CharID {
				who = ch.Name
			}
		}
		others = append(others, fmt.Sprintf("%s — %s, %s", k.Label, who,
			model.Describe(o.Struct, o.Rig, o.Sec)))
	}
	data["OtherSetups"] = others
	if ref != nil {
		data["Refinery"] = ref
	} else if set.CharID != 0 {
		data["CharErr"] = "не удалось прочитать навыки персонажа — считаю по ручному выходу"
	}
	data["Kind"] = kind
	data["Kinds"] = oreKinds
	data["Stats"] = oreStats
	data["Stat"] = stat
	data["StatIsBuy"] = esi.IsBuy(stat)
	data["Amount"] = trimFloat(amount)
	data["AmountUnit"] = amountUnit(kind)
	data["Yield"] = trimFloat(yield * 100)
	data["Cols"] = cols
	data["HasRare"] = hasRare
	data["Rows"] = rows
	data["MoonTiers"] = moonTiers
	data["Sort"] = sortKey
	data["Desc"] = q.Get("d")
	data["ShowVariants"] = q.Get("v") != "0"
	data["ShowComp"] = q.Get("c") != "0"
	data["ShowBatch"] = q.Get("b") == "1"
	data["ShowFound"] = q.Get("f") != "0"
	data["PerM3"] = perM3
	data["Priced"] = len(book)
	data["Wanted"] = len(dedupeIDs(ids))
	s.render(w, "ore", data, stale)
}

// refinedPerUnit is the market value of the materials in one unit.
func refinedPerUnit(t *sde.OreType, price func(int64) float64, yield float64) float64 {
	var sum float64
	for m := range t.Materials {
		sum += t.PerUnit(m) * price(m)
	}
	return sum * yield
}

// cell pairs a market price with the refine-or-sell verdict for it.
func cell(p, refinedUnit, volume float64) priceCell {
	c := priceCell{Price: p, HasPrice: p > 0}
	if volume > 0 {
		c.PerM3 = p / volume
	}
	if p > 0 && refinedUnit > 0 {
		c.Delta = (refinedUnit/p - 1) * 100
		c.HasDelta = true
	}
	return c
}

// moonTierCells renders the "R4 … R64 + minerals" columns of the moon
// table: which materials of that rarity one amount of ore yields.
func moonTierCells(t sde.OreType, mats map[int64]sde.MaterialInfo, factor float64) []string {
	buckets := make([][]string, len(moonTiers))
	ids := make([]int64, 0, len(t.Materials))
	for m := range t.Materials {
		ids = append(ids, m)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	for _, m := range ids {
		info := mats[m]
		qty := t.PerUnit(m) * factor
		if qty <= 0 {
			continue
		}
		idx := moonMatTier(info)
		buckets[idx] = append(buckets[idx],
			fmt.Sprintf("%s %s", formatQty(qty), info.Name))
	}
	out := make([]string, len(moonTiers))
	for i, b := range buckets {
		out[i] = strings.Join(b, " · ")
	}
	return out
}

// moonMatTier places a moon material in its R-column. The rarity is not
// a property of the material in the SDE — it is the rarity of the ore
// it comes out of — so we read it off the material's market group name
// and fall back to the minerals column.
func moonMatTier(m sde.MaterialInfo) int {
	if m.GroupID == 18 { // Mineral
		return len(moonTiers) - 1
	}
	switch m.NameEn {
	case "Atmospheric Gases", "Evaporite Deposits", "Hydrocarbons", "Silicates":
		return 0 // R4
	case "Cobalt", "Scandium", "Titanium", "Tungsten":
		return 1 // R8
	case "Cadmium", "Chromium", "Platinum", "Vanadium":
		return 2 // R16
	case "Caesium", "Hafnium", "Mercury", "Technetium":
		return 3 // R32
	case "Dysprosium", "Neodymium", "Promethium", "Thulium":
		return 4 // R64
	}
	return len(moonTiers) - 1
}

// matRank orders the material columns: minerals, then ice products,
// then the isotopes (which are ice products too, but belong at the end
// of the row — one ice yields exactly one of them).
func matRank(m sde.MaterialInfo) int {
	switch m.GroupID {
	case 18:
		return 0
	case 423:
		if strings.HasSuffix(m.NameEn, "Isotopes") {
			return 2
		}
		return 1
	case 422:
		return 2
	}
	return 3
}

// shortMat shortens a material name for the table header.
func shortMat(name string) string {
	if i := strings.Index(name, " Isotopes"); i > 0 {
		return name[:i] + " Iso."
	}
	if name == "Strontium Clathrates" {
		return "Strontium"
	}
	return name
}

// formatQty prints a material amount: fractions matter below ten.
func formatQty(v float64) string {
	switch {
	case v < 10:
		return strconv.FormatFloat(v, 'f', 3, 64)
	case v < 1000:
		return strconv.FormatFloat(v, 'f', 1, 64)
	}
	return formatNum(int64(v + 0.5))
}

func sortRows(rows []oreRow, desc bool, key func(oreRow) float64) {
	sort.SliceStable(rows, func(i, j int) bool {
		a, b := key(rows[i]), key(rows[j])
		if desc {
			return a > b
		}
		return a < b
	})
}

func defaultAmount(kind string) float64 {
	if kind == sde.KindMoon {
		return 1000
	}
	return 1
}

func amountUnit(kind string) string {
	if kind == sde.KindIce {
		return "бл." // ice is mined by the block; "1 блоков" reads badly
	}
	return "м³"
}

// parseAmount accepts plain numbers and the k/m/b suffixes miners type.
func parseAmount(s string, def float64) float64 {
	s = strings.TrimSpace(strings.ToLower(strings.ReplaceAll(s, " ", "")))
	if s == "" {
		return def
	}
	mult := 1.0
	switch {
	case strings.HasSuffix(s, "k"):
		mult, s = 1e3, strings.TrimSuffix(s, "k")
	case strings.HasSuffix(s, "m"):
		mult, s = 1e6, strings.TrimSuffix(s, "m")
	case strings.HasSuffix(s, "b"):
		mult, s = 1e9, strings.TrimSuffix(s, "b")
	}
	v, err := strconv.ParseFloat(strings.ReplaceAll(s, ",", "."), 64)
	if err != nil || v <= 0 {
		return def
	}
	return v * mult
}

// pctText prints a rate as a percentage without the float noise: the
// base rate is a product of three numbers and comes out as 62.6248000015.
func pctText(v float64) string {
	return strconv.FormatFloat(math.Round(v*100)/100, 'f', -1, 64)
}

// parseYield takes both "76.4" and "0.764".
func parseYield(s string) float64 {
	s = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(s), "%"))
	if s == "" {
		return 1
	}
	v, err := strconv.ParseFloat(strings.ReplaceAll(s, ",", "."), 64)
	if err != nil || v <= 0 {
		return 1
	}
	if v > 1 {
		v /= 100
	}
	if v > 1 {
		v = 1
	}
	return v
}

func trimFloat(v float64) string {
	return strconv.FormatFloat(v, 'f', -1, 64)
}

func dedupeIDs(ids []int64) []int64 {
	seen := map[int64]bool{}
	out := ids[:0:0]
	for _, id := range ids {
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}

// ── watchlist ────────────────────────────────────────────────────────

// watchItems is the short list of goods worth a glance every login.
// Region matters for exactly one of them: PLEX trades globally.
var watchItems = []struct {
	NameEn, Label string
	Region        int64
}{
	{"PLEX", "PLEX", esi.RegionPLEX},
	{"Skill Extractor", "Экстрактор навыков", esi.RegionTheForge},
	{"Large Skill Injector", "Большой инжектор навыков", esi.RegionTheForge},
	{"Hydrogen Fuel Block", "Водородный топливный блок", esi.RegionTheForge},
	{"Nitrogen Fuel Block", "Азотный топливный блок", esi.RegionTheForge},
	{"Oxygen Fuel Block", "Кислородный топливный блок", esi.RegionTheForge},
}

// watchRow is one watched good with its book and its recent history.
type watchRow struct {
	TypeID  int64
	Name    string
	NameEn  string
	Stats   esi.OrderStats
	Spread  float64 // sell over buy, percent
	Days    []watchDay
	Avg     float64 // last known daily average
	Change  float64 // over the shown window, percent
	Peak    float64
	HasHist bool
	Global  bool // traded in the global market, not in The Forge
}

type watchDay struct {
	Label string
	Price float64
	H     float64 // bar height, percent of the peak
}

func (s *Server) handleMarketWatch(w http.ResponseWriter, r *http.Request) {
	ec, stale := s.esiFor(r)
	data, _, err := s.shell(ec, 0, "")
	if err != nil {
		httpError(w, "loading characters", err)
		return
	}
	days := 90
	if d, err := strconv.Atoi(r.URL.Query().Get("d")); err == nil && d >= 14 && d <= 365 {
		days = d
	}

	rows := make([]watchRow, len(watchItems))
	var wg sync.WaitGroup
	for i, it := range watchItems {
		id := s.SDE.TypeIDByName(it.NameEn)
		rows[i] = watchRow{TypeID: id, Name: it.Label, NameEn: it.NameEn,
			Global: it.Region == esi.RegionPLEX}
		if id == 0 {
			continue
		}
		region := it.Region
		wg.Add(1)
		go func(i int, id int64) {
			defer wg.Done()
			st, err := ec.RegionOrderStats(region, id)
			if err == nil {
				rows[i].Stats = st
			}
		}(i, id)
		wg.Add(1)
		go func(i int, id int64) {
			defer wg.Done()
			hist, err := ec.RegionHistory(region, id)
			if err != nil || len(hist) == 0 {
				return
			}
			if len(hist) > days {
				hist = hist[len(hist)-days:]
			}
			var peak float64
			for _, d := range hist {
				if d.Average > peak {
					peak = d.Average
				}
			}
			row := &rows[i]
			row.HasHist = true
			row.Peak = peak
			row.Avg = hist[len(hist)-1].Average
			if first := hist[0].Average; first > 0 {
				row.Change = (row.Avg/first - 1) * 100
			}
			for _, d := range hist {
				h := 0.0
				if peak > 0 {
					h = d.Average / peak * 100
				}
				row.Days = append(row.Days, watchDay{
					Label: d.Day.Format("02.01.2006"), Price: d.Average, H: h,
				})
			}
		}(i, id)
	}
	wg.Wait()
	// Spread against the MAXIMUM buy, not the trimmed one: on injectors
	// a single million-unit 1-ISK bid holds most of the buy volume, and
	// the trimmed percentiles honestly land on it. The number a trader
	// acts on is what the best bidder actually pays.
	for i := range rows {
		if rows[i].Stats.BuyMax > 0 {
			rows[i].Spread = (rows[i].Stats.SellP98/rows[i].Stats.BuyMax - 1) * 100
		}
	}

	data["Rows"] = rows
	data["Days"] = days
	data["DayOptions"] = []int{14, 90, 365}
	s.render(w, "market_watch", data, stale)
}
