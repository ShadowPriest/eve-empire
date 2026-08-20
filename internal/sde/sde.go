// Package sde reads the local static-data database built by
// cmd/sdeimport (types, dogma attributes, blueprints, icons).
// The database is optional: when it is missing every lookup returns
// zero values so the app keeps working on ESI data alone.
package sde

import (
	"database/sql"
	"fmt"
	"html"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"

	_ "modernc.org/sqlite"
)

// Dogma attribute ids used for implants.
const (
	AttrCharisma     = 175
	AttrIntelligence = 176
	AttrMemory       = 177
	AttrPerception   = 178
	AttrWillpower    = 179
	AttrImplantness  = 331 // implant slot number
)

// AttrByName maps our attribute keys to dogma ids.
var AttrByName = map[string]int64{
	"charisma":     AttrCharisma,
	"intelligence": AttrIntelligence,
	"memory":       AttrMemory,
	"perception":   AttrPerception,
	"willpower":    AttrWillpower,
}

type DB struct {
	db *sql.DB

	mu    sync.RWMutex
	ready bool
}

// Open opens the static-data database. A missing file is not an error —
// the returned DB simply reports Available() == false.
func Open(path string) *DB {
	if _, err := os.Stat(path); err != nil {
		return &DB{}
	}
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&mode=ro")
	if err != nil {
		return &DB{}
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return &DB{}
	}
	return &DB{db: db, ready: true}
}

func (d *DB) Available() bool {
	if d == nil {
		return false
	}
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.ready
}

func (d *DB) Close() error {
	if d == nil || d.db == nil {
		return nil
	}
	return d.db.Close()
}

// Implant describes one plugged-in implant with its attribute bonuses.
type Implant struct {
	TypeID  int64
	Name    string
	Slot    int
	Bonuses map[string]int // attribute key -> points
}

// Implants loads names, slots and attribute bonuses for the given type
// ids, sorted by slot.
func (d *DB) Implants(ids []int64) []Implant {
	if !d.Available() || len(ids) == 0 {
		return nil
	}
	out := make([]Implant, 0, len(ids))
	for _, id := range ids {
		im := Implant{TypeID: id, Bonuses: map[string]int{}}
		var nameRu, nameEn sql.NullString
		if err := d.db.QueryRow(
			`SELECT name_ru, name_en FROM types WHERE type_id = ?`, id).Scan(&nameRu, &nameEn); err != nil {
			im.Name = fmt.Sprintf("Имплант %d", id)
		} else if nameRu.Valid && nameRu.String != "" {
			im.Name = nameRu.String
		} else {
			im.Name = nameEn.String
		}

		rows, err := d.db.Query(
			`SELECT attribute_id, value FROM type_attributes WHERE type_id = ?`, id)
		if err == nil {
			for rows.Next() {
				var attr int64
				var val float64
				if rows.Scan(&attr, &val) != nil {
					continue
				}
				switch attr {
				case AttrImplantness:
					im.Slot = int(val)
				default:
					for key, aid := range AttrByName {
						if attr == aid && val != 0 {
							im.Bonuses[key] = int(val)
						}
					}
				}
			}
			rows.Close()
		}
		out = append(out, im)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].Slot < out[j-1].Slot; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// TypeNames resolves type ids to localized names.
func (d *DB) TypeNames(ids []int64) map[int64]string {
	out := map[int64]string{}
	if !d.Available() || len(ids) == 0 {
		return out
	}
	for _, id := range ids {
		var nameRu, nameEn sql.NullString
		if d.db.QueryRow(`SELECT name_ru, name_en FROM types WHERE type_id = ?`, id).
			Scan(&nameRu, &nameEn) != nil {
			continue
		}
		if nameRu.Valid && nameRu.String != "" {
			out[id] = nameRu.String
		} else if nameEn.Valid {
			out[id] = nameEn.String
		}
	}
	return out
}

// Dogma ids used by the item info modal.
const (
	AttrTechLevel     = 422
	AttrMetaLevel     = 633
	AttrFitsToShipType = 1380
	AttrVolume        = 161
)

// TypeAttr is one displayable attribute of a type.
type TypeAttr struct {
	ID    int64
	Name  string // human name (falls back to the dogma key)
	Value string // formatted with its unit
	Raw   float64
}

// SkillReq is a skill requirement, possibly with nested prerequisites.
type SkillReq struct {
	TypeID int64
	Name   string
	Level  int
	Nested []SkillReq
}

// Variation is a sibling item (same market group and name core).
type Variation struct {
	TypeID    int64
	Name      string
	MetaGroup int
	MetaName  string
}

// metaPrefixes are quality words EVE puts in front of a variant name.
var metaPrefixes = []string{
	"Limited", "Compact", "Scoped", "Enduring", "Ample", "Restrained",
	"Upgraded", "Experimental", "Prototype", "Small", "Medium", "Large",
}

// nameCore strips the variant suffix (" - Improved") and the quality
// prefix ("Limited ") so siblings of one item share the same core.
func nameCore(name string) string {
	core := name
	if i := strings.Index(core, " - "); i > 0 {
		core = core[:i]
	}
	for _, p := range metaPrefixes {
		if strings.HasPrefix(core, p+" ") {
			core = strings.TrimPrefix(core, p+" ")
			break
		}
	}
	return strings.TrimSpace(core)
}

// BPMaterial is one material line of a blueprint activity.
type BPMaterial struct {
	TypeID   int64
	Name     string
	Quantity int64
}

// TypeInfo is everything the item modal shows.
type TypeInfo struct {
	TypeID      int64
	Name        string
	GroupName   string
	Description string
	Volume      float64
	BasePrice   float64
	Attrs       []TypeAttr
	Skills      []SkillReq
	Variations  []Variation
	BlueprintID int64
	BlueprintNm string
	Materials   []BPMaterial
	Found       bool
}

// unitSuffix renders the dogma unit of a value.
var unitSuffix = map[int64]string{
	1: " м", 2: " кг", 3: " с", 9: " м³", 10: " м/с", 11: " м/с²",
	101: " мс", 102: " мм", 103: " МПа", 104: "x", 105: "%", 106: " тф",
	107: " МВт", 108: "%", 109: "%", 111: "%", 113: " ед. HP", 114: " ГДж",
	119: " очк.", 120: "%", 121: " слот", 122: " ", 124: " с", 125: "%",
	126: " Н", 127: " св. лет", 128: "%", 129: " Мбит/с", 133: " ISK",
	134: " м³/ч", 135: " а.е.", 136: " слот", 138: " ед.", 139: "", 140: " ур.",
	141: " шт.",
}

func fmtValue(v float64, unit int64) string {
	s := ""
	abs := v
	if abs < 0 {
		abs = -abs
	}
	switch {
	case v == float64(int64(v)) && abs < 1e15:
		s = fmt.Sprintf("%d", int64(v))
	case abs < 1:
		// small values (item volumes like 0.005 m³) keep their precision
		s = strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.4f", v), "0"), ".")
	case abs < 10:
		s = fmt.Sprintf("%.2f", v)
	default:
		s = fmt.Sprintf("%.1f", v)
	}
	return s + unitSuffix[unit]
}

func (d *DB) typeName(id int64) string {
	var ru, en sql.NullString
	if d.db.QueryRow(`SELECT name_ru, name_en FROM types WHERE type_id = ?`, id).Scan(&ru, &en) != nil {
		return fmt.Sprintf("Тип %d", id)
	}
	if ru.Valid && ru.String != "" {
		return ru.String
	}
	return en.String
}

// skillReqs loads skill requirements recursively (depth-limited, as the
// game shows prerequisites of prerequisites).
func (d *DB) skillReqs(typeID int64, depth int) []SkillReq {
	if depth <= 0 {
		return nil
	}
	rows, err := d.db.Query(
		`SELECT skill_type_id, level FROM type_skills WHERE type_id = ? ORDER BY skill_type_id`, typeID)
	if err != nil {
		return nil
	}
	type pair struct {
		id  int64
		lvl int
	}
	var pairs []pair
	for rows.Next() {
		var p pair
		if rows.Scan(&p.id, &p.lvl) == nil {
			pairs = append(pairs, p)
		}
	}
	rows.Close()

	out := make([]SkillReq, 0, len(pairs))
	for _, p := range pairs {
		out = append(out, SkillReq{
			TypeID: p.id, Name: d.typeName(p.id), Level: p.lvl,
			Nested: d.skillReqs(p.id, depth-1),
		})
	}
	return out
}

var (
	reBR       = regexp.MustCompile(`(?i)<br\s*/?>`)
	reShowInfo = regexp.MustCompile(`(?is)<a\s+href=showinfo:(\d+)[^>]*>(.*?)</a>`)
	reAnyTag   = regexp.MustCompile(`(?s)<[^>]*>`)
)

// DescriptionHTML converts CCP's in-game markup to safe HTML: showinfo
// links become clickable item chips, <br> becomes a line break and every
// other tag is dropped.
func DescriptionHTML(desc string) string {
	if desc == "" {
		return ""
	}
	s := reBR.ReplaceAllString(desc, "\n")
	// Protect showinfo links with placeholders before stripping tags.
	type link struct{ id, text string }
	var links []link
	s = reShowInfo.ReplaceAllStringFunc(s, func(m string) string {
		g := reShowInfo.FindStringSubmatch(m)
		links = append(links, link{g[1], reAnyTag.ReplaceAllString(g[2], "")})
		return fmt.Sprintf("\x00%d\x00", len(links)-1)
	})
	s = reAnyTag.ReplaceAllString(s, "")
	s = html.UnescapeString(s)
	s = html.EscapeString(s)
	s = strings.ReplaceAll(s, "\n", "<br>")
	for i, l := range links {
		chip := fmt.Sprintf(
			`<span class="itm" data-type="%s"><img class="itmico sm" src="/icons/%s" alt=""><span class="itmnm">%s</span></span>`,
			l.id, l.id, html.EscapeString(l.text))
		s = strings.ReplaceAll(s, fmt.Sprintf("\x00%d\x00", i), chip)
	}
	return s
}

// TypeInfo assembles the full item card for the info modal.
func (d *DB) TypeInfo(typeID int64) TypeInfo {
	info := TypeInfo{TypeID: typeID}
	if !d.Available() {
		return info
	}
	var (
		groupID              int64
		nameRu, nameEn       sql.NullString
		descRu, descEn       sql.NullString
		groupRu, groupEn     sql.NullString
	)
	err := d.db.QueryRow(`SELECT t.group_id, t.name_ru, t.name_en, t.description_ru, t.description_en,
		t.volume, t.base_price, g.name_ru, g.name_en
		FROM types t LEFT JOIN groups g ON g.group_id = t.group_id WHERE t.type_id = ?`, typeID).
		Scan(&groupID, &nameRu, &nameEn, &descRu, &descEn, &info.Volume, &info.BasePrice, &groupRu, &groupEn)
	if err != nil {
		return info
	}
	info.Found = true
	pick := func(a, b sql.NullString) string {
		if a.Valid && a.String != "" {
			return a.String
		}
		return b.String
	}
	info.Name = pick(nameRu, nameEn)
	info.Description = pick(descRu, descEn)
	info.GroupName = pick(groupRu, groupEn)

	// Attributes (skip the requiredSkill plumbing — shown separately).
	skipAttrs := map[int64]bool{}
	for _, p := range [][2]int64{{182, 277}, {183, 278}, {184, 279}, {1285, 1286}, {1289, 1287}, {1290, 1288}} {
		skipAttrs[p[0]], skipAttrs[p[1]] = true, true
	}
	// Only published attributes with a display name — the same subset the
	// game shows on the "Характеристики" tab.
	var fitsTo float64
	rows, err := d.db.Query(`SELECT ta.attribute_id, ta.value, d.display_name_ru, d.display_name_en, d.unit_id
		FROM type_attributes ta JOIN dogma_attributes d ON d.attribute_id = ta.attribute_id
		WHERE ta.type_id = ? AND d.published = 1 ORDER BY d.display_name_ru`, typeID)
	if err == nil {
		for rows.Next() {
			var (
				aid    int64
				val    float64
				ru, en sql.NullString
				unit   sql.NullInt64
			)
			if rows.Scan(&aid, &val, &ru, &en, &unit) != nil {
				continue
			}
			if aid == AttrFitsToShipType {
				fitsTo = val
			}
			name := pick(ru, en)
			if skipAttrs[aid] || val == 0 || name == "" {
				continue
			}
			ta := TypeAttr{ID: aid, Name: name, Raw: val, Value: fmtValue(val, unit.Int64)}
			// unit 116 = typeID, 115 = groupID: render the referenced name.
			switch unit.Int64 {
			case 116:
				ta.Value = d.typeName(int64(val))
			case 115:
				var gname sql.NullString
				if d.db.QueryRow(`SELECT COALESCE(NULLIF(name_ru,''), name_en) FROM groups WHERE group_id=?`,
					int64(val)).Scan(&gname) == nil {
					ta.Value = gname.String
				}
			}
			info.Attrs = append(info.Attrs, ta)
		}
		rows.Close()
	}

	// Volume lives on the type itself, not in dogma, but the client always
	// lists it — put it first.
	if info.Volume > 0 {
		hasVolume := false
		for _, a := range info.Attrs {
			if a.ID == AttrVolume {
				hasVolume = true
				break
			}
		}
		if !hasVolume {
			info.Attrs = append([]TypeAttr{{
				ID: AttrVolume, Name: "Занимаемый объём", Raw: info.Volume,
				Value: fmtValue(info.Volume, 9),
			}}, info.Attrs...)
		}
	}

	info.Skills = d.skillReqs(typeID, 3)

	// Variations: siblings in the same market group sharing the name core
	// (the game's "Варианты" tab). Hull-locked items additionally match
	// on the hull, which is what makes T3 subsystems line up.
	var marketGroup sql.NullInt64
	d.db.QueryRow(`SELECT market_group_id FROM types WHERE type_id = ?`, typeID).Scan(&marketGroup)
	core := nameCore(pick(nameEn, nameRu)) // match on English: variant naming is stable there
	vq := `SELECT t.type_id, t.name_ru, t.name_en, t.meta_group_id,
			COALESCE(NULLIF(m.name_ru,''), m.name_en, '')
		FROM types t LEFT JOIN meta_groups m ON m.meta_group_id = t.meta_group_id
		WHERE t.published = 1 AND t.name_en LIKE ?`
	args := []any{"%" + core + "%"}
	switch {
	case fitsTo != 0:
		vq += ` AND EXISTS (SELECT 1 FROM type_attributes a WHERE a.type_id = t.type_id
			AND a.attribute_id = ? AND a.value = ?)`
		args = append(args, AttrFitsToShipType, fitsTo)
	case marketGroup.Valid && marketGroup.Int64 != 0:
		vq += ` AND t.market_group_id = ?`
		args = append(args, marketGroup.Int64)
	default:
		vq += ` AND t.group_id = ?`
		args = append(args, groupID)
	}
	vq += ` ORDER BY t.meta_group_id, t.name_en LIMIT 40`
	if vrows, err := d.db.Query(vq, args...); err == nil {
		for vrows.Next() {
			var v Variation
			var ru, en sql.NullString
			var mg sql.NullInt64
			if vrows.Scan(&v.TypeID, &ru, &en, &mg, &v.MetaName) != nil {
				continue
			}
			v.Name, v.MetaGroup = pick(ru, en), int(mg.Int64)
			info.Variations = append(info.Variations, v)
		}
		vrows.Close()
	}

	// Manufacturing: the blueprint producing this type and its materials.
	var bpID int64
	if d.db.QueryRow(`SELECT blueprint_type_id FROM bp_products
		WHERE product_type_id = ? AND activity = 'manufacturing' LIMIT 1`, typeID).Scan(&bpID) == nil && bpID != 0 {
		info.BlueprintID, info.BlueprintNm = bpID, d.typeName(bpID)
		if mrows, err := d.db.Query(`SELECT material_type_id, quantity FROM bp_materials
			WHERE blueprint_type_id = ? AND activity = 'manufacturing' ORDER BY quantity DESC`, bpID); err == nil {
			for mrows.Next() {
				var m BPMaterial
				if mrows.Scan(&m.TypeID, &m.Quantity) == nil {
					m.Name = d.typeName(m.TypeID)
					info.Materials = append(info.Materials, m)
				}
			}
			mrows.Close()
		}
	}
	return info
}

// ── planetary industry ───────────────────────────────────────────────

// piTiers maps the planetary groups to their tier label.
var piTiers = map[int64]string{
	1032: "P0", 1033: "P0", 1035: "P0",
	1042: "P1", 1034: "P2", 1040: "P3", 1041: "P4",
}

// piTierName is the Russian tier label shown next to an item.
var piTierName = map[string]string{
	"P0": "Сырьё", "P1": "Класс 1", "P2": "Класс 2", "P3": "Класс 3", "P4": "Класс 4",
}

// rawByPlanet lists which P0 resource each planet type yields. The SDE
// no longer ships this mapping, and it has been stable for years.
var rawByPlanet = map[string][]int64{
	"Temperate": {2073, 2268, 2287, 2288, 2305, 2311},
	"Barren":    {2073, 2267, 2268, 2270, 2288, 2310},
	"Oceanic":   {2073, 2268, 2286, 2287, 2288, 2311},
	"Ice":       {2073, 2268, 2286, 2288, 2310, 2309},
	"Gas":       {2268, 2270, 2306, 2308, 2310, 2311},
	"Storm":     {2267, 2268, 2272, 2308, 2309, 2310},
	"Lava":      {2267, 2270, 2272, 2306, 2307, 2308},
	"Plasma":    {2267, 2270, 2272, 2306, 2307, 2309},
}

// planetTypeIDs are the type ids of the eight harvestable planet types.
var planetTypeIDs = map[string]int64{
	"Temperate": 11, "Ice": 12, "Gas": 13, "Oceanic": 2014,
	"Lava": 2015, "Barren": 2016, "Storm": 2017, "Plasma": 2063,
}

// PlanetTypeID maps the planet_type ESI reports ("temperate", "gas", …)
// to its type id, so the planet can be drawn with its own icon.
func PlanetTypeID(planetType string) int64 {
	for name, id := range planetTypeIDs {
		if strings.EqualFold(name, planetType) {
			return id
		}
	}
	return 0
}

// PIItem is one item in the planetary industry tab.
type PIItem struct {
	TypeID   int64
	Name     string
	Tier     string // P0..P4
	TierName string // "Класс 1"
	Quantity int64
}

// PIInfo is the "Планетарная промышленность" tab of an item card.
type PIInfo struct {
	Tier        string   // tier of the item itself
	TierName    string
	Planets     []PIItem // P0 only: planet types it is extracted from
	Inputs      []PIItem // what this item is made of
	MadeInto    []PIItem // what it is used for
	CycleTime   int64    // seconds per production cycle
	OutputQty   int64
	IsPlanetary bool
}

// PlanetaryInfo assembles the planetary industry card for a type.
func (d *DB) PlanetaryInfo(typeID int64) PIInfo {
	var info PIInfo
	if !d.Available() {
		return info
	}
	var groupID int64
	if d.db.QueryRow(`SELECT group_id FROM types WHERE type_id = ?`, typeID).Scan(&groupID) != nil {
		return info
	}
	tier, ok := piTiers[groupID]
	if !ok {
		return info
	}
	info.IsPlanetary, info.Tier, info.TierName = true, tier, piTierName[tier]

	item := func(id int64, qty int64) PIItem {
		var g int64
		d.db.QueryRow(`SELECT group_id FROM types WHERE type_id = ?`, id).Scan(&g)
		t := piTiers[g]
		return PIItem{TypeID: id, Name: d.typeName(id), Tier: t, TierName: piTierName[t], Quantity: qty}
	}

	// Raw resources: which planets they come from.
	if tier == "P0" {
		for name, ids := range rawByPlanet {
			for _, id := range ids {
				if id == typeID {
					pid := planetTypeIDs[name]
					info.Planets = append(info.Planets, PIItem{
						TypeID: pid, Name: d.typeName(pid),
					})
				}
			}
		}
		sort.Slice(info.Planets, func(i, j int) bool { return info.Planets[i].Name < info.Planets[j].Name })
	}

	// The schematic producing this item → its inputs.
	var schemID int64
	if d.db.QueryRow(`SELECT schematic_id FROM pi_schematic_types
		WHERE type_id = ? AND is_input = 0 LIMIT 1`, typeID).Scan(&schemID) == nil && schemID != 0 {
		d.db.QueryRow(`SELECT cycle_time FROM pi_schematics WHERE schematic_id = ?`, schemID).Scan(&info.CycleTime)
		d.db.QueryRow(`SELECT quantity FROM pi_schematic_types
			WHERE schematic_id = ? AND type_id = ? AND is_input = 0`, schemID, typeID).Scan(&info.OutputQty)
		if rows, err := d.db.Query(`SELECT type_id, quantity FROM pi_schematic_types
			WHERE schematic_id = ? AND is_input = 1`, schemID); err == nil {
			for rows.Next() {
				var id, q int64
				if rows.Scan(&id, &q) == nil {
					info.Inputs = append(info.Inputs, item(id, q))
				}
			}
			rows.Close()
		}
	}

	// Schematics consuming this item → what it is used for.
	if rows, err := d.db.Query(`SELECT DISTINCT o.type_id FROM pi_schematic_types i
		JOIN pi_schematic_types o ON o.schematic_id = i.schematic_id AND o.is_input = 0
		WHERE i.type_id = ? AND i.is_input = 1`, typeID); err == nil {
		for rows.Next() {
			var id int64
			if rows.Scan(&id) == nil {
				info.MadeInto = append(info.MadeInto, item(id, 0))
			}
		}
		rows.Close()
		sort.Slice(info.MadeInto, func(i, j int) bool { return info.MadeInto[i].Name < info.MadeInto[j].Name })
	}
	return info
}

// PINode is one commodity in the full planetary chain view.
type PINode struct {
	TypeID   int64
	Name     string
	Tier     string
	Inputs   []int64 // type ids it is made from
	Outputs  []int64 // type ids it feeds into
	Planets  []string
	Quantity int64 // input quantity per cycle (of its own schematic)
	OutQty   int64 // produced per cycle
	Cycle    int64
}

// PIChain is the whole P0..P4 tree used by the planetary tool.
type PIChain struct {
	Tiers   []string
	Nodes   map[string][]PINode // tier -> nodes
	Planets []PIItem            // the eight harvestable planet types
}

// PlanetaryChain builds the complete planetary production tree.
func (d *DB) PlanetaryChain() PIChain {
	chain := PIChain{
		Tiers: []string{"P0", "P1", "P2", "P3", "P4"},
		Nodes: map[string][]PINode{},
	}
	if !d.Available() {
		return chain
	}

	// Planet types, ordered as in the game's tool.
	for _, name := range []string{"Barren", "Gas", "Ice", "Lava", "Oceanic", "Plasma", "Storm", "Temperate"} {
		id := planetTypeIDs[name]
		chain.Planets = append(chain.Planets, PIItem{TypeID: id, Name: name})
	}
	planetsOf := map[int64][]string{}
	for name, ids := range rawByPlanet {
		for _, id := range ids {
			planetsOf[id] = append(planetsOf[id], name)
		}
	}

	// Schematic links, both directions.
	inputs := map[int64][]int64{}  // product -> inputs
	outputs := map[int64][]int64{} // input -> products
	qtyIn := map[int64]int64{}
	cycle := map[int64]int64{}
	outQty := map[int64]int64{}
	rows, err := d.db.Query(`SELECT s.schematic_id, s.cycle_time, t.type_id, t.is_input, t.quantity
		FROM pi_schematics s JOIN pi_schematic_types t ON t.schematic_id = s.schematic_id`)
	if err == nil {
		type link struct {
			ins  []int64
			outs []int64
		}
		bySchem := map[int64]*link{}
		cycleOf := map[int64]int64{}
		qtyOf := map[[2]int64]int64{}
		for rows.Next() {
			var sid, ct, tid, q int64
			var isIn int
			if rows.Scan(&sid, &ct, &tid, &isIn, &q) != nil {
				continue
			}
			cycleOf[sid] = ct
			l := bySchem[sid]
			if l == nil {
				l = &link{}
				bySchem[sid] = l
			}
			if isIn == 1 {
				l.ins = append(l.ins, tid)
			} else {
				l.outs = append(l.outs, tid)
			}
			qtyOf[[2]int64{sid, tid}] = q
		}
		rows.Close()
		for sid, l := range bySchem {
			for _, out := range l.outs {
				inputs[out] = append(inputs[out], l.ins...)
				cycle[out] = cycleOf[sid]
				outQty[out] = qtyOf[[2]int64{sid, out}]
				for _, in := range l.ins {
					outputs[in] = append(outputs[in], out)
					qtyIn[in] = qtyOf[[2]int64{sid, in}]
				}
			}
		}
	}

	// Commodities per tier, ordered by name like the reference tool.
	for group, tier := range piTiers {
		trows, err := d.db.Query(`SELECT type_id, COALESCE(NULLIF(name_ru,''), name_en), name_en
			FROM types WHERE group_id = ? AND published = 1`, group)
		if err != nil {
			continue
		}
		for trows.Next() {
			var n PINode
			var nameEn string
			if trows.Scan(&n.TypeID, &n.Name, &nameEn) != nil {
				continue
			}
			n.Tier = tier
			n.Inputs = inputs[n.TypeID]
			n.Outputs = outputs[n.TypeID]
			n.Planets = planetsOf[n.TypeID]
			n.Quantity = qtyIn[n.TypeID]
			n.OutQty = outQty[n.TypeID]
			n.Cycle = cycle[n.TypeID]
			chain.Nodes[tier] = append(chain.Nodes[tier], n)
		}
		trows.Close()
	}
	for _, tier := range chain.Tiers {
		ns := chain.Nodes[tier]
		sort.Slice(ns, func(i, j int) bool { return ns[i].Name < ns[j].Name })
	}
	return chain
}

// PIRecipe is one planetary schematic in calculator-friendly form.
type PIRecipe struct {
	TypeID int64     `json:"id"`
	Name   string    `json:"n"`
	Tier   string    `json:"t"`
	OutQty int64     `json:"o"` // produced per cycle
	Cycle  int64     `json:"c"` // seconds per cycle
	Inputs [][2]int64 `json:"i"` // [typeID, qty per cycle]
}

// PIRecipes returns every planetary commodity keyed by type id, so the
// requirements calculator can expand a target down to raw materials.
func (d *DB) PIRecipes() map[int64]PIRecipe {
	out := map[int64]PIRecipe{}
	if !d.Available() {
		return out
	}
	// Every planetary commodity, including raw ones (no inputs).
	rows, err := d.db.Query(`SELECT t.type_id, COALESCE(NULLIF(t.name_ru,''), t.name_en), t.group_id
		FROM types t WHERE t.published = 1 AND t.group_id IN (1032,1033,1035,1042,1034,1040,1041)`)
	if err != nil {
		return out
	}
	for rows.Next() {
		var id, gid int64
		var name string
		if rows.Scan(&id, &name, &gid) == nil {
			out[id] = PIRecipe{TypeID: id, Name: name, Tier: piTiers[gid]}
		}
	}
	rows.Close()

	srows, err := d.db.Query(`SELECT s.schematic_id, s.cycle_time, t.type_id, t.is_input, t.quantity
		FROM pi_schematics s JOIN pi_schematic_types t ON t.schematic_id = s.schematic_id`)
	if err != nil {
		return out
	}
	type schem struct {
		cycle  int64
		out    int64
		outQty int64
		ins    [][2]int64
	}
	bySchem := map[int64]*schem{}
	for srows.Next() {
		var sid, ct, tid, q int64
		var isIn int
		if srows.Scan(&sid, &ct, &tid, &isIn, &q) != nil {
			continue
		}
		s := bySchem[sid]
		if s == nil {
			s = &schem{cycle: ct}
			bySchem[sid] = s
		}
		if isIn == 1 {
			s.ins = append(s.ins, [2]int64{tid, q})
		} else {
			s.out, s.outQty = tid, q
		}
	}
	srows.Close()

	for _, s := range bySchem {
		r, ok := out[s.out]
		if !ok {
			continue
		}
		r.Cycle, r.OutQty, r.Inputs = s.cycle, s.outQty, s.ins
		out[s.out] = r
	}
	return out
}

// PlanetStructs are the structure type ids available on a planet type.
type PlanetStructs struct {
	PlanetType   int64
	PlanetName   string
	CommandCtr   int64
	Launchpad    int64
	Storage      int64
	BasicFactory int64
	AdvFactory   int64
	HighTech     int64
}

// PlanetStructures resolves the buildable structures of every planet
// type by name prefix ("Barren Launchpad", "Barren Storage Facility"...).
func (d *DB) PlanetStructures() map[string]PlanetStructs {
	out := map[string]PlanetStructs{}
	if !d.Available() {
		return out
	}
	for name, pid := range planetTypeIDs {
		ps := PlanetStructs{PlanetType: pid, PlanetName: name}
		rows, err := d.db.Query(`SELECT t.type_id, t.name_en, g.name_en
			FROM types t JOIN groups g ON g.group_id = t.group_id
			WHERE t.published = 1 AND t.name_en LIKE ?
			  AND g.name_en IN ('Spaceports','Processors','Storage Facilities','Command Centers')`,
			name+" %")
		if err != nil {
			continue
		}
		for rows.Next() {
			var id int64
			var tn, gn string
			if rows.Scan(&id, &tn, &gn) != nil {
				continue
			}
			switch {
			case gn == "Spaceports":
				ps.Launchpad = id
			case gn == "Storage Facilities":
				ps.Storage = id
			case gn == "Command Centers":
				ps.CommandCtr = id
			case strings.Contains(tn, "Basic Industry"):
				ps.BasicFactory = id
			case strings.Contains(tn, "Advanced Industry"):
				ps.AdvFactory = id
			case strings.Contains(tn, "High-Tech"):
				ps.HighTech = id
			}
		}
		rows.Close()
		out[name] = ps
	}
	return out
}

// Capacity returns the storage volume (m³) of a type, 0 when it has none.
func (d *DB) Capacity(typeID int64) float64 {
	if !d.Available() {
		return 0
	}
	var c float64
	d.db.QueryRow(`SELECT capacity FROM types WHERE type_id = ?`, typeID).Scan(&c)
	return c
}

// Volumes returns the unit volume (m³) of each type id.
func (d *DB) Volumes(ids []int64) map[int64]float64 {
	out := map[int64]float64{}
	if !d.Available() {
		return out
	}
	for _, id := range ids {
		var v float64
		if d.db.QueryRow(`SELECT volume FROM types WHERE type_id = ?`, id).Scan(&v) == nil {
			out[id] = v
		}
	}
	return out
}

// SchematicProduct returns the product name and type id of a planetary
// schematic id (as reported by ESI for factory pins).
func (d *DB) SchematicProduct(schematicID int64) (string, int64) {
	name, id, _ := d.SchematicInfo(schematicID)
	return name, id
}

// SchematicInputs lists what one cycle of a schematic consumes.
func (d *DB) SchematicInputs(schematicID int64) []BPItem {
	if !d.Available() || schematicID == 0 {
		return nil
	}
	rows, err := d.db.Query(`SELECT type_id, quantity FROM pi_schematic_types
		WHERE schematic_id = ? AND is_input = 1`, schematicID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []BPItem
	for rows.Next() {
		var it BPItem
		if rows.Scan(&it.TypeID, &it.Quantity) == nil {
			it.Name = d.typeName(it.TypeID)
			out = append(out, it)
		}
	}
	return out
}

// SchematicInfo also reports the schematic's cycle time in seconds.
func (d *DB) SchematicInfo(schematicID int64) (string, int64, int) {
	if !d.Available() || schematicID == 0 {
		return "", 0, 0
	}
	var typeID int64
	if d.db.QueryRow(`SELECT type_id FROM pi_schematic_types
		WHERE schematic_id = ? AND is_input = 0 LIMIT 1`, schematicID).Scan(&typeID) != nil {
		return "", 0, 0
	}
	var cycle int
	d.db.QueryRow(`SELECT cycle_time FROM pi_schematics WHERE schematic_id = ?`, schematicID).Scan(&cycle)
	return d.typeName(typeID), typeID, cycle
}

// StructureRole classifies a planetary structure so layouts can be
// mapped between planet types.
func (d *DB) StructureRole(typeID int64) string {
	if !d.Available() {
		return ""
	}
	var name, group string
	if d.db.QueryRow(`SELECT t.name_en, g.name_en FROM types t
		JOIN groups g ON g.group_id = t.group_id WHERE t.type_id = ?`, typeID).
		Scan(&name, &group) != nil {
		return ""
	}
	switch group {
	case "Spaceports":
		return "launchpad"
	case "Storage Facilities":
		return "storage"
	case "Command Centers":
		return "cc"
	case "Extractors", "Extractor Control Units":
		return "extractor"
	case "Processors":
		switch {
		case strings.Contains(name, "Advanced Industry"):
			return "advanced"
		case strings.Contains(name, "High-Tech"):
			return "hightech"
		default:
			return "basic"
		}
	}
	// Extractor control units live in their own group in some dumps.
	if strings.Contains(name, "Extractor") {
		return "extractor"
	}
	return ""
}

// RawPlanets maps each P0 resource to the planet types yielding it.
func (d *DB) RawPlanets() map[int64][]string {
	out := map[int64][]string{}
	for name, ids := range rawByPlanet {
		for _, id := range ids {
			out[id] = append(out[id], name)
		}
	}
	return out
}

// TierOf reports the planetary tier of a commodity ("" when it is not
// a planetary item).
func (d *DB) TierOf(typeID int64) string {
	if !d.Available() {
		return ""
	}
	var g int64
	if d.db.QueryRow(`SELECT group_id FROM types WHERE type_id = ?`, typeID).Scan(&g) != nil {
		return ""
	}
	return piTiers[g]
}

// ── blueprints ───────────────────────────────────────────────────────

// BPItem is a product/material/skill line of a blueprint activity.
type BPItem struct {
	TypeID   int64
	Name     string
	Quantity int64
	Level    int    // skills only
	Group    string // grouping label for materials
}

// BPActivity is one blueprint activity (manufacturing, copying, ...).
type BPActivity struct {
	Key       string // ESI/SDE activity key
	Label     string // Russian label as in the client
	Time      int64  // seconds per run
	Products  []BPItem
	Materials []BPItem
	Skills    []BPItem
}

// BlueprintInfo is everything the blueprint modal shows.
type BlueprintInfo struct {
	TypeID     int64
	Name       string
	MaxRuns    int64
	Activities []BPActivity
}

var activityLabels = map[string]string{
	"manufacturing":     "Производство",
	"copying":           "Копирование",
	"research_material": "Исследование материалов",
	"research_time":     "Исследование времени",
	"invention":         "Инвент",
	"reaction":          "Реакции",
}

// activityOrder matches the icon row order in the client.
var activityOrder = []string{
	"manufacturing", "research_time", "research_material", "copying", "invention", "reaction",
}

// IsBlueprint reports whether the type has blueprint data.
func (d *DB) IsBlueprint(typeID int64) bool {
	if !d.Available() {
		return false
	}
	var n int
	d.db.QueryRow(`SELECT 1 FROM blueprints WHERE blueprint_type_id = ?`, typeID).Scan(&n)
	return n == 1
}

// materialGroup labels a material the way the client groups them: by
// category, except the catch-all "Материал" category where the item
// group (Минералы, Замерзшие вещества) is more informative.
func (d *DB) materialGroup(typeID int64) string {
	var grp, cat sql.NullString
	d.db.QueryRow(`SELECT COALESCE(NULLIF(g.name_ru,''), g.name_en), COALESCE(NULLIF(c.name_ru,''), c.name_en)
		FROM types t LEFT JOIN groups g ON g.group_id = t.group_id
		LEFT JOIN categories c ON c.category_id = g.category_id
		WHERE t.type_id = ?`, typeID).Scan(&grp, &cat)
	switch cat.String {
	case "Материал", "Material", "":
		if grp.String != "" {
			return grp.String
		}
	}
	if cat.String != "" {
		return cat.String
	}
	return "Прочее"
}

// Blueprint assembles the blueprint card.
func (d *DB) Blueprint(typeID int64) BlueprintInfo {
	info := BlueprintInfo{TypeID: typeID}
	if !d.Available() {
		return info
	}
	d.db.QueryRow(`SELECT max_production_limit FROM blueprints WHERE blueprint_type_id = ?`, typeID).
		Scan(&info.MaxRuns)
	info.Name = d.typeName(typeID)

	times := map[string]int64{}
	if rows, err := d.db.Query(`SELECT activity, time FROM bp_activities WHERE blueprint_type_id = ?`, typeID); err == nil {
		for rows.Next() {
			var a string
			var t int64
			if rows.Scan(&a, &t) == nil {
				times[a] = t
			}
		}
		rows.Close()
	}

	load := func(q string, act string, withGroup bool) []BPItem {
		var out []BPItem
		rows, err := d.db.Query(q, typeID, act)
		if err != nil {
			return nil
		}
		for rows.Next() {
			var it BPItem
			if rows.Scan(&it.TypeID, &it.Quantity) != nil {
				continue
			}
			it.Name = d.typeName(it.TypeID)
			if withGroup {
				it.Group = d.materialGroup(it.TypeID)
			}
			out = append(out, it)
		}
		rows.Close()
		return out
	}

	for _, key := range activityOrder {
		t, ok := times[key]
		if !ok {
			continue
		}
		act := BPActivity{Key: key, Label: activityLabels[key], Time: t}
		act.Products = load(`SELECT product_type_id, quantity FROM bp_products
			WHERE blueprint_type_id = ? AND activity = ? ORDER BY quantity DESC`, key, false)
		act.Materials = load(`SELECT material_type_id, quantity FROM bp_materials
			WHERE blueprint_type_id = ? AND activity = ? ORDER BY quantity DESC`, key, true)
		if rows, err := d.db.Query(`SELECT skill_type_id, level FROM bp_skills
			WHERE blueprint_type_id = ? AND activity = ? ORDER BY level DESC`, typeID, key); err == nil {
			for rows.Next() {
				var it BPItem
				if rows.Scan(&it.TypeID, &it.Level) == nil {
					it.Name = d.typeName(it.TypeID)
					act.Skills = append(act.Skills, it)
				}
			}
			rows.Close()
		}
		info.Activities = append(info.Activities, act)
	}
	return info
}

// Icon returns the stored PNG for a type (nil when absent). Blueprint
// copies have their own artwork, requested with copy = true.
func (d *DB) Icon(typeID int64, copy bool) []byte {
	if !d.Available() {
		return nil
	}
	var png []byte
	if copy {
		if d.db.QueryRow(`SELECT png FROM icons_bpc WHERE type_id = ?`, typeID).Scan(&png) == nil && len(png) > 0 {
			return png
		}
	}
	if d.db.QueryRow(`SELECT png FROM icons WHERE type_id = ?`, typeID).Scan(&png) != nil {
		return nil
	}
	return png
}

// Meta returns a value from the meta table (e.g. "build").
func (d *DB) Meta(key string) string {
	if !d.Available() {
		return ""
	}
	var v string
	_ = d.db.QueryRow(`SELECT value FROM meta WHERE key = ?`, key).Scan(&v)
	return v
}
