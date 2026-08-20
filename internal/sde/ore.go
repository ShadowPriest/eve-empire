package sde

// The static half of the ore table: what every minable rock is made of.
//
// Everything here comes out of two SDE datasets the importer now keeps:
// type_materials (what one portion_size batch reprocesses into) and
// compressible (raw type -> its compressed twin). Prices are not our
// business — the web layer pairs these families with the order book.
//
// GRABLE: quantities are per PORTION, not per unit. Veldspar's portion
// is 100, so its 400 Tritanium are 4 per unit and 40 per m³ at 0.1 m³
// each. Batch-compressed rocks carry a portion of 1 and the full
// hundred-fold materials, which is exactly why one of them is worth a
// hundred raw units.

import (
	"database/sql"
	"sort"
	"strings"
)

// Kinds of harvestable the tables cover.
const (
	KindOre  = "ore"
	KindIce  = "ice"
	KindMoon = "moon"
	KindGas  = "gas"
)

// Group ids that are not plain ore even though they sit in the asteroid
// category: ice and the five moon-asteroid rarities have their own tabs.
var (
	iceGroups  = []int64{465}
	moonGroups = []int64{1884, 1920, 1921, 1922, 1923}
	gasGroups  = []int64{711, 4168}
	// Ore that CCP filed outside the asteroid category: Prismaticite and
	// the unrefined-mineral rocks sit in Material (category 4), and both
	// refine into a RANGE rather than a fixed basket.
	strayOreGroups = []int64{4915, 4932}
)

// moonTier names the R-rating of the moon materials a group yields.
var moonTier = map[int64]string{
	1884: "R4", 1920: "R8", 1921: "R16", 1922: "R32", 1923: "R64",
}

// MoonTier returns the R-rating of a moon-asteroid group ("" if none).
func MoonTier(groupID int64) string { return moonTier[groupID] }

// OreType is one tradeable rock: raw, compressed or batch-compressed.
type OreType struct {
	TypeID    int64
	Name      string // localized
	NameEn    string
	Volume    float64 // m³ per unit
	Portion   int64   // reprocessing batch size
	Materials map[int64]int64
	Erratic   bool // refines into a random amount (Prismaticite & co)
}

// PerUnit returns how much of one material a single unit yields.
func (t *OreType) PerUnit(materialID int64) float64 {
	if t == nil || t.Portion == 0 {
		return 0
	}
	return float64(t.Materials[materialID]) / float64(t.Portion)
}

// OreVariant is one grade of a family plus its compressed twins.
type OreVariant struct {
	OreType
	Grade      string  // "II-Grade", "Brimful", ... ("" for the base rock)
	Bonus      float64 // yield over the base variant, percent
	Compressed *OreType
	Batch      *OreType
}

// OreFamily groups a rock with its improved grades.
type OreFamily struct {
	BaseID      int64
	Name        string
	NameEn      string
	GroupID     int64
	Volume      float64
	Variants    []OreVariant
	MaterialIDs []int64 // union over the variants, in display order
	Found       []FoundTag
	SkillID     int64 // the ore-specific reprocessing skill
	SkillName   string
}

// Dogma attributes the reprocessing maths reads.
const (
	AttrReprocessSkill = 790 // ore -> its specific processing skill
	AttrRefineMutator  = 379 // implant -> refining yield bonus, per cent
)

// FoundTag is one "where does it spawn" chip.
type FoundTag struct {
	Label string
	Title string
}

// oreGroups lists the group ids a kind is built from.
func (d *DB) oreGroups(kind string) []int64 {
	switch kind {
	case KindIce:
		return iceGroups
	case KindMoon:
		return moonGroups
	case KindGas:
		return gasGroups
	}
	// Plain ore: every asteroid group except ice and the moon rocks.
	skip := map[int64]bool{}
	for _, g := range append(append([]int64{}, iceGroups...), moonGroups...) {
		skip[g] = true
	}
	out := append([]int64{}, strayOreGroups...)
	rows, err := d.db.Query(`SELECT group_id FROM groups WHERE category_id = 25 ORDER BY group_id`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	for rows.Next() {
		var g int64
		if rows.Scan(&g) == nil && !skip[g] {
			out = append(out, g)
		}
	}
	return out
}

// gradeOf splits a variant name against its family base: "Veldspar
// II-Grade" -> "II-Grade", "Brimful Bitumens" -> "Brimful".
func gradeOf(base, name string) string {
	if name == base {
		return ""
	}
	if s, ok := strings.CutPrefix(name, base+" "); ok {
		return s
	}
	if s, ok := strings.CutSuffix(name, " "+base); ok {
		return s
	}
	return name
}

// Harvestables builds the families of one kind: base rock, its grades,
// their compressed twins and the material basket each of them yields.
func (d *DB) Harvestables(kind string) []OreFamily {
	if !d.Available() {
		return nil
	}
	groups := d.oreGroups(kind)
	if len(groups) == 0 {
		return nil
	}
	in := placeholders(len(groups))
	args := make([]any, len(groups))
	for i, g := range groups {
		args[i] = g
	}

	// ── every tradeable rock of these groups ──
	type row struct {
		OreType
		GroupID int64
	}
	rows, err := d.db.Query(`
		SELECT type_id, group_id, name_en, name_ru, volume, portion_size
		  FROM types
		 WHERE group_id IN (`+in+`) AND published = 1
		   AND market_group_id IS NOT NULL AND market_group_id <> 0`, args...)
	if err != nil {
		return nil
	}
	byID := map[int64]*row{}
	byName := map[string]*row{}
	var order []int64
	for rows.Next() {
		var r row
		var nameRu sql.NullString
		if rows.Scan(&r.TypeID, &r.GroupID, &r.NameEn, &nameRu, &r.Volume, &r.Portion) != nil {
			continue
		}
		r.Name = r.NameEn
		if nameRu.Valid && nameRu.String != "" {
			r.Name = nameRu.String
		}
		r.Materials = map[int64]int64{}
		byID[r.TypeID] = &r
		byName[r.NameEn] = &r
		order = append(order, r.TypeID)
	}
	rows.Close()
	if len(byID) == 0 {
		return nil
	}

	// ── reprocessing output ──
	ids := make([]any, 0, len(order))
	for _, id := range order {
		ids = append(ids, id)
	}
	mrows, err := d.db.Query(`SELECT type_id, material_type_id, quantity
		  FROM type_materials WHERE type_id IN (`+placeholders(len(ids))+`)`, ids...)
	if err == nil {
		for mrows.Next() {
			var t, m, q int64
			if mrows.Scan(&t, &m, &q) == nil {
				if r := byID[t]; r != nil {
					r.Materials[m] = q
				}
			}
		}
		mrows.Close()
	}
	// Erratic rocks report a range instead; take the midpoint so they
	// have a comparable value at all, and flag them in the UI.
	rrows, err := d.db.Query(`SELECT type_id, material_type_id, qty_min, qty_max
		  FROM type_materials_rand WHERE type_id IN (`+placeholders(len(ids))+`)`, ids...)
	if err == nil {
		for rrows.Next() {
			var t, m, lo, hi int64
			if rrows.Scan(&t, &m, &lo, &hi) == nil {
				if r := byID[t]; r != nil {
					r.Materials[m] = (lo + hi) / 2
					r.Erratic = true
				}
			}
		}
		rrows.Close()
	}

	// ── compression links (the table is small, read it whole) ──
	compressed := map[int64]int64{}
	if crows, err := d.db.Query(`SELECT type_id, compressed_type_id FROM compressible`); err == nil {
		for crows.Next() {
			var a, b int64
			if crows.Scan(&a, &b) == nil {
				compressed[a] = b
			}
		}
		crows.Close()
	}

	// ── families ──
	// GRABLE: variationParentTypeID is empty for every rock in the SDE
	// (CCP fills it for ships and modules only), so the family has to be
	// read off the names: a grade is always the base name plus one word,
	// either in front ("Brimful Bitumens") or behind ("Veldspar II-Grade").
	var raws []*row
	for _, id := range order {
		r := byID[id]
		// Compressed twins are attached to their raw rock, not listed.
		if strings.HasPrefix(r.NameEn, "Compressed ") || strings.HasPrefix(r.NameEn, "Batch Compressed ") {
			continue
		}
		raws = append(raws, r)
	}
	parent := map[int64]int64{}
	for _, r := range raws {
		var best *row
		for _, o := range raws {
			if o.TypeID == r.TypeID || o.GroupID != r.GroupID {
				continue
			}
			if !strings.HasPrefix(r.NameEn, o.NameEn+" ") && !strings.HasSuffix(r.NameEn, " "+o.NameEn) {
				continue
			}
			if best == nil || len(o.NameEn) > len(best.NameEn) {
				best = o
			}
		}
		if best != nil {
			parent[r.TypeID] = best.TypeID
		}
	}
	fams := map[int64]*OreFamily{}
	members := map[int64][]*row{}
	for _, r := range raws {
		key := r.TypeID
		for hops := 0; parent[key] != 0 && hops < 4; hops++ {
			key = parent[key]
		}
		members[key] = append(members[key], r)
	}
	for key, list := range members {
		// The base rock is the one with the plainest name — every grade
		// is that name plus a word.
		sort.Slice(list, func(i, j int) bool {
			if len(list[i].NameEn) != len(list[j].NameEn) {
				return len(list[i].NameEn) < len(list[j].NameEn)
			}
			return list[i].TypeID < list[j].TypeID
		})
		base := list[0]
		fam := &OreFamily{
			BaseID: base.TypeID, Name: base.Name, NameEn: base.NameEn,
			GroupID: base.GroupID, Volume: base.Volume,
			Found: foundIn[base.NameEn],
		}
		matSeen := map[int64]bool{}
		for _, r := range list {
			v := OreVariant{OreType: r.OreType}
			v.Grade = gradeOf(base.NameEn, r.NameEn)
			if n, ok := nominalGrade[v.Grade]; ok {
				v.Bonus = n
			} else {
				v.Bonus = bonusOver(base.OreType, r.OreType)
			}
			if c := byID[compressed[r.TypeID]]; c != nil {
				v.Compressed = &c.OreType
			} else if c := byName["Compressed "+r.NameEn]; c != nil {
				v.Compressed = &c.OreType
			}
			if b := byName["Batch Compressed "+r.NameEn]; b != nil {
				v.Batch = &b.OreType
			}
			for m := range r.Materials {
				matSeen[m] = true
			}
			fam.Variants = append(fam.Variants, v)
		}
		// The base rock always leads — it is the reference every grade is
		// measured against, and a 0-Grade rock yields LESS than it does.
		sort.SliceStable(fam.Variants, func(i, j int) bool {
			if a, b := fam.Variants[i].Grade == "", fam.Variants[j].Grade == ""; a != b {
				return a
			}
			return fam.Variants[i].Bonus < fam.Variants[j].Bonus
		})
		for m := range matSeen {
			fam.MaterialIDs = append(fam.MaterialIDs, m)
		}
		sort.Slice(fam.MaterialIDs, func(i, j int) bool { return fam.MaterialIDs[i] < fam.MaterialIDs[j] })
		fams[key] = fam
	}

	// ── the ore-specific reprocessing skill of every family ──
	// CCP consolidated the old per-rock skills into four ("Simple ..." to
	// "Complex Ore Processing") plus ice, the five moon rarities and a
	// few oddballs; attribute 790 on the rock itself is the only place
	// that says which one applies.
	skillOf := map[int64]int64{}
	if srows, err := d.db.Query(`SELECT type_id, value FROM type_attributes
		 WHERE attribute_id = ? AND type_id IN (`+placeholders(len(ids))+`)`,
		append([]any{int64(AttrReprocessSkill)}, ids...)...); err == nil {
		for srows.Next() {
			var t int64
			var v float64
			if srows.Scan(&t, &v) == nil {
				skillOf[t] = int64(v)
			}
		}
		srows.Close()
	}
	skillNames := map[int64]string{}
	for _, f := range fams {
		if sk := skillOf[f.BaseID]; sk != 0 {
			f.SkillID = sk
			skillNames[sk] = ""
		}
	}
	skillIDs := make([]int64, 0, len(skillNames))
	for id := range skillNames {
		skillIDs = append(skillIDs, id)
	}
	for id, name := range d.TypeNames(skillIDs) {
		skillNames[id] = name
	}
	for _, f := range fams {
		f.SkillName = skillNames[f.SkillID]
	}

	out := make([]OreFamily, 0, len(fams))
	for _, f := range fams {
		if len(f.Variants) == 0 {
			continue
		}
		// Gas is bought and sold, never refined; everything else without
		// a reprocessing recipe (mutanite, the AIR event rocks) has
		// nothing to say in a refining table.
		if kind != KindGas && len(f.MaterialIDs) == 0 {
			continue
		}
		out = append(out, *f)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].NameEn < out[j].NameEn })
	return out
}

// nominalGrade is the advertised step of the standard ore grades. The
// real quantities in the SDE are whole units and land a fraction of a
// per cent off it (Mordunium II-Grade yields 101 Pyerite where +5 %
// would be 101.85) — the columns show what the game really refines,
// the label shows what CCP calls it.
// Moon rocks name their grades instead of numbering them, one pair of
// words per rarity, and improve by a nominal 15 % / 100 %.
var nominalGrade = map[string]float64{
	"II-Grade": 5, "III-Grade": 10, "IV-Grade": 15,
	"Brimful": 15, "Copious": 15, "Replete": 15, "Lavish": 15, "Opulent": 15,
	"Glistening": 100, "Twinkling": 100, "Glowing": 100, "Shimmering": 100, "Shining": 100,
}

// bonusOver measures a grade's yield against the family's base rock.
// Quantities are whole units, so a material the rock yields five of
// rounds badly (5 -> 6 reads as +20 %); the one with the largest count
// carries the least rounding and is the honest answer.
func bonusOver(base, v OreType) float64 {
	var pick int64
	var biggest int64
	for m, q := range base.Materials {
		if q > biggest {
			biggest, pick = q, m
		}
	}
	if pick == 0 || base.Portion == 0 {
		return 0
	}
	b := float64(biggest) / float64(base.Portion)
	g := v.PerUnit(pick)
	if b <= 0 || g <= 0 {
		return 0
	}
	return (g/b - 1) * 100
}

// MaterialInfo is a reprocessing output as the table header shows it.
type MaterialInfo struct {
	TypeID  int64
	Name    string
	NameEn  string
	GroupID int64
	Volume  float64
}

// MaterialInfos loads names for the material columns.
func (d *DB) MaterialInfos(ids []int64) map[int64]MaterialInfo {
	out := map[int64]MaterialInfo{}
	if !d.Available() || len(ids) == 0 {
		return out
	}
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	rows, err := d.db.Query(`SELECT type_id, name_en, name_ru, group_id, volume
		  FROM types WHERE type_id IN (`+placeholders(len(ids))+`)`, args...)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var m MaterialInfo
		var nameRu sql.NullString
		if rows.Scan(&m.TypeID, &m.NameEn, &nameRu, &m.GroupID, &m.Volume) != nil {
			continue
		}
		m.Name = m.NameEn
		if nameRu.Valid && nameRu.String != "" {
			m.Name = nameRu.String
		}
		out[m.TypeID] = m
	}
	return out
}

// ImplantRefineBonus sums the refining bonus of the plugged implants,
// in per cent (the Zainou 'Beancounter' line gives 1, 2 or 4).
func (d *DB) ImplantRefineBonus(ids []int64) float64 {
	if !d.Available() || len(ids) == 0 {
		return 0
	}
	args := append([]any{int64(AttrRefineMutator)}, toArgs(ids)...)
	rows, err := d.db.Query(`SELECT value FROM type_attributes
		 WHERE attribute_id = ? AND type_id IN (`+placeholders(len(ids))+`)`, args...)
	if err != nil {
		return 0
	}
	defer rows.Close()
	var sum float64
	for rows.Next() {
		var v float64
		if rows.Scan(&v) == nil {
			sum += v
		}
	}
	return sum
}

func toArgs(ids []int64) []any {
	out := make([]any, len(ids))
	for i, id := range ids {
		out[i] = id
	}
	return out
}

// TypeIDByName resolves an exact English type name (watchlist lookups).
func (d *DB) TypeIDByName(name string) int64 {
	if !d.Available() {
		return 0
	}
	var id int64
	d.db.QueryRow(`SELECT type_id FROM types WHERE name_en = ? AND published = 1
		 ORDER BY type_id LIMIT 1`, name).Scan(&id)
	return id
}

func placeholders(n int) string {
	if n <= 0 {
		return "NULL"
	}
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}
