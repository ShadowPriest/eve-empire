package sde

// Where the refining happens: the base rate before any skill touches it.
//
// Every number here is READ FROM THE SDE, not typed in from a wiki. The
// service module carries the plain 50 %, the rig carries the rate it
// raises it to plus the three security multipliers, and the hull carries
// its own percentage on top:
//
//	база = выход рига × множитель секурити × (1 + бонус корпуса/100)
//
// Сверка: Tatara + Monitor II в нулях = 0.53 × 1.12 × 1.055 = 0.6262,
// а с идеальными навыками и имплантом +4 % это 90.63 % — ровно то число,
// которое в игре считается пределом переработки.

// Type ids of the reprocessing gear.
const (
	typeReprocessingFacility = 35899 // Standup Reprocessing Facility I
	typeRigMonitorL1         = 46639 // Standup L-Set Reprocessing Monitor I
	typeRigMonitorL2         = 46640 // ... Monitor II (XL-Set is identical)
	typeAthanor              = 35835
	typeTatara               = 35836
)

// Dogma attributes of that gear.
const (
	attrRefineYield  = 717  // refiningYieldMultiplier: the rate itself
	attrHiSec        = 2355 // hiSecModifier
	attrLowSec       = 2356 // lowSecModifier
	attrNullSec      = 2357 // nullSecModifier (nullsec AND wormholes)
	attrStrRefineBon = 2722 // strRefiningYieldBonus, per cent, on the hull
)

// RigOption is a reprocessing rig: the rate it gives and how space
// multiplies its bonus.
type RigOption struct {
	Key   string
	Name  string
	Short string  // for the one-line summary: the real names are long
	Yield float64 // 0 = no rig, the station rate stands
	Sec   map[string]float64
}

// StructOption is the hull doing the refining.
type StructOption struct {
	Key   string
	Name  string
	Bonus float64 // per cent on top, 0 for an NPC station
	Rigs  bool    // NPC stations take no rigs
}

// RefineryModel is everything the reprocessing settings window offers.
type RefineryModel struct {
	StationBase float64
	Structures  []StructOption
	Rigs        []RigOption
	Secs        []SecOption
}

// SecOption is one security band.
type SecOption struct {
	Key   string
	Name  string
	Short string
}

// SecBands are the three bands a rig's bonus is scaled by.
var SecBands = []SecOption{
	{"hi", "Хайсек", "хай"},
	{"low", "Лоусек", "лоу"},
	{"null", "Нули и червоточины", "нули/ВХ"},
}

// RefineryModel reads the reprocessing numbers out of the static data.
// Without sde.db it falls back to the plain NPC station, which is what
// the rest of the app does everywhere else.
func (d *DB) RefineryModel() RefineryModel {
	m := RefineryModel{StationBase: 0.5, Secs: SecBands}
	attr := func(typeID, attrID int64) float64 {
		if !d.Available() {
			return 0
		}
		var v float64
		d.db.QueryRow(`SELECT value FROM type_attributes WHERE type_id = ? AND attribute_id = ?`,
			typeID, attrID).Scan(&v)
		return v
	}
	if base := attr(typeReprocessingFacility, attrRefineYield); base > 0 {
		m.StationBase = base
	}
	names := d.TypeNames([]int64{typeRigMonitorL1, typeRigMonitorL2, typeAthanor, typeTatara})
	name := func(id int64, fallback string) string {
		if n := names[id]; n != "" {
			return n
		}
		return fallback
	}

	m.Structures = []StructOption{
		{Key: "npc", Name: "НПС-станция"},
		{Key: "athanor", Name: name(typeAthanor, "Athanor"),
			Bonus: attr(typeAthanor, attrStrRefineBon), Rigs: true},
		{Key: "tatara", Name: name(typeTatara, "Tatara"),
			Bonus: attr(typeTatara, attrStrRefineBon), Rigs: true},
	}
	m.Rigs = []RigOption{{Key: "none", Name: "без рига", Short: "без рига"}}
	for _, r := range []struct {
		key, fallback, short string
		id                   int64
	}{
		{"t1", "Reprocessing Monitor I", "риг I", typeRigMonitorL1},
		{"t2", "Reprocessing Monitor II", "риг II", typeRigMonitorL2},
	} {
		y := attr(r.id, attrRefineYield)
		if y <= 0 {
			continue
		}
		m.Rigs = append(m.Rigs, RigOption{
			Key: r.key, Name: name(r.id, r.fallback), Short: r.short, Yield: y,
			Sec: map[string]float64{
				"hi":   attr(r.id, attrHiSec),
				"low":  attr(r.id, attrLowSec),
				"null": attr(r.id, attrNullSec),
			},
		})
	}
	return m
}

// Base computes the base refining rate of one setup.
func (m RefineryModel) Base(structKey, rigKey, secKey string) float64 {
	var st StructOption
	for _, s := range m.Structures {
		if s.Key == structKey {
			st = s
		}
	}
	if st.Key == "" || !st.Rigs {
		return m.StationBase // an NPC station is 50 %, and that is that
	}
	base := m.StationBase
	for _, r := range m.Rigs {
		if r.Key != rigKey || r.Yield <= 0 {
			continue
		}
		base = r.Yield
		// Space only multiplies a rig's bonus — an unrigged refinery is
		// worth the same in hisec and in a wormhole.
		if mul := r.Sec[secKey]; mul > 0 {
			base *= mul
		}
	}
	return base * (1 + st.Bonus/100)
}

// Describe spells the setup out for the summary line.
func (m RefineryModel) Describe(structKey, rigKey, secKey string) string {
	var out string
	for _, s := range m.Structures {
		if s.Key == structKey {
			out = s.Name
			if !s.Rigs {
				return out
			}
		}
	}
	if out == "" {
		return "НПС-станция"
	}
	for _, r := range m.Rigs {
		if r.Key == rigKey && r.Yield > 0 {
			out += " + " + r.Short
		}
	}
	for _, s := range m.Secs {
		if s.Key == secKey {
			out += " · " + s.Short
		}
	}
	return out
}
