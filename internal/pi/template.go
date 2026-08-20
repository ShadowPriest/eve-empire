// Package pi parses and generates EVE planetary colony templates in the
// game's own export/import JSON format.
//
// Format (as exported by the client):
//
//	CmdCtrLv  command center level
//	Cmt       free-form comment shown in the client
//	Diam      planet diameter in km (link length maths)
//	Pln       planet type id (2016 = Barren, ...)
//	P         pins: {T: structure type, La/Lo: radians, H: height, S: schematic product}
//	L         links: {S: source pin, D: destination pin, Lv: level} — 1-based pin indexes
//	R         routes: {P: [pin path], Q: quantity per cycle, T: commodity type}
package pi

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
)

// Pin is one structure on the planet.
type Pin struct {
	H  float64  `json:"H"`
	La float64  `json:"La"`
	Lo float64  `json:"Lo"`
	S  *int64   `json:"S"` // schematic product type, nil for launchpads/storage
	T  int64    `json:"T"` // structure type id
}

// Link connects two pins (1-based indexes into P).
type Link struct {
	D  int `json:"D"`
	Lv int `json:"Lv"`
	S  int `json:"S"`
}

// Route moves a commodity along a path of pins.
type Route struct {
	P []int `json:"P"`
	Q int64 `json:"Q"`
	T int64 `json:"T"`
}

// Template is the whole colony layout.
type Template struct {
	CmdCtrLv int     `json:"CmdCtrLv"`
	Cmt      string  `json:"Cmt"`
	Diam     float64 `json:"Diam"`
	L        []Link  `json:"L"`
	P        []Pin   `json:"P"`
	Pln      int64   `json:"Pln"`
	R        []Route `json:"R"`
}

// Parse reads a template from the game's JSON.
func Parse(data []byte) (*Template, error) {
	var t Template
	if err := json.Unmarshal(data, &t); err != nil {
		return nil, fmt.Errorf("не похоже на шаблон EVE: %w", err)
	}
	if len(t.P) == 0 {
		return nil, fmt.Errorf("в шаблоне нет структур")
	}
	return &t, nil
}

// JSON serialises the template exactly the way the client writes it:
// `", "` / `": "` separators, Diam always with a decimal point and
// plain ASCII in the comment. The game's importer is picky about this —
// a bare `4460` instead of `4460.0` is enough for it to reject the file.
func (t *Template) JSON() ([]byte, error) {
	var b strings.Builder
	num := func(f float64) string { return strconv.FormatFloat(f, 'g', -1, 64) }

	b.WriteString(`{"CmdCtrLv": `)
	b.WriteString(strconv.Itoa(t.CmdCtrLv))
	b.WriteString(`, "Cmt": `)
	cmt, err := json.Marshal(asciiSafe(t.Cmt))
	if err != nil {
		return nil, err
	}
	b.Write(cmt)
	b.WriteString(`, "Diam": `)
	b.WriteString(strconv.FormatFloat(t.Diam, 'f', 1, 64)) // 4460 -> "4460.0"

	b.WriteString(`, "L": [`)
	for i, l := range t.L {
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, `{"D": %d, "Lv": %d, "S": %d}`, l.D, l.Lv, l.S)
	}
	b.WriteString(`], "P": [`)
	for i, p := range t.P {
		if i > 0 {
			b.WriteString(", ")
		}
		s := "null"
		if p.S != nil {
			s = strconv.FormatInt(*p.S, 10)
		}
		fmt.Fprintf(&b, `{"H": %s, "La": %s, "Lo": %s, "S": %s, "T": %d}`,
			num(p.H), num(p.La), num(p.Lo), s, p.T)
	}
	b.WriteString(`], "Pln": `)
	b.WriteString(strconv.FormatInt(t.Pln, 10))

	b.WriteString(`, "R": [`)
	for i, r := range t.R {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(`{"P": [`)
		for j, p := range r.P {
			if j > 0 {
				b.WriteString(", ")
			}
			b.WriteString(strconv.Itoa(p))
		}
		fmt.Fprintf(&b, `], "Q": %d, "T": %d}`, r.Q, r.T)
	}
	b.WriteString(`]}`)
	return []byte(b.String()), nil
}

// asciiSafe replaces typography the client's importer may choke on.
func asciiSafe(s string) string {
	r := strings.NewReplacer("×", "x", "—", "-", "–", "-", "«", `"`, "»", `"`, "…", "...")
	s = r.Replace(s)
	var out strings.Builder
	for _, c := range s {
		if c < 128 {
			out.WriteRune(c)
		}
	}
	return strings.TrimSpace(out.String())
}

// Summary describes a template for the UI.
type Summary struct {
	Factories  map[int64]int // produced type -> factory count
	Structures map[int64]int // structure type -> count
	Imports    []int64       // commodities that have to be brought in
	Products   []int64       // commodities produced here
	Final      int64         // the highest-tier product
	Pins       int
	Links      int
	Routes     int
}

// Describe extracts what the colony makes and what it needs. tierOf
// reports the P-tier of a commodity ("P0".."P4", "" if unknown).
func (t *Template) Describe(tierOf func(int64) string) Summary {
	s := Summary{
		Factories:  map[int64]int{},
		Structures: map[int64]int{},
		Pins:       len(t.P),
		Links:      len(t.L),
		Routes:     len(t.R),
	}
	produced := map[int64]bool{}
	for _, p := range t.P {
		s.Structures[p.T]++
		if p.S != nil {
			s.Factories[*p.S]++
			produced[*p.S] = true
		}
	}

	// Everything routed but not produced here must be imported.
	routed := map[int64]bool{}
	for _, r := range t.R {
		routed[r.T] = true
	}
	for id := range routed {
		if !produced[id] {
			s.Imports = append(s.Imports, id)
		}
	}
	for id := range produced {
		s.Products = append(s.Products, id)
	}
	sort.Slice(s.Imports, func(i, j int) bool { return s.Imports[i] < s.Imports[j] })
	sort.Slice(s.Products, func(i, j int) bool { return s.Products[i] < s.Products[j] })

	// The final product is the one nothing else consumes.
	best := ""
	for id := range produced {
		tier := tierOf(id)
		if tier > best {
			best, s.Final = tier, id
		}
	}
	return s
}

// ── generator ────────────────────────────────────────────────────────

// PlanetStructures are the structure type ids of one planet type.
type PlanetStructures struct {
	PlanetType   int64
	CommandCtr   int64
	Launchpad    int64
	Storage      int64
	BasicFactory int64
	AdvFactory   int64
	HighTech     int64
}

// GenSpec describes the colony to build.
type GenSpec struct {
	Name     string
	CmdCtrLv int
	Diameter float64
	Struct   PlanetStructures
	// Factories to place: product type -> how many.
	Factories []GenFactory
	// Inputs that arrive from outside (routed launchpad -> factory).
	Imports []int64
}

// GenFactory is one production line to place.
type GenFactory struct {
	Product int64
	Count   int
	Tier    string
	// Inputs of one cycle: type -> quantity.
	Inputs map[int64]int64
	Output int64 // produced per cycle
}

// Generate lays out a colony: a launchpad in the centre, factories on
// rings around it, links from every factory to the launchpad and routes
// for inputs and outputs. The result imports/exports through the
// launchpad, which is how "factory planet" templates are built.
func Generate(spec GenSpec) *Template {
	t := &Template{
		CmdCtrLv: spec.CmdCtrLv,
		Cmt:      spec.Name,
		Diam:     spec.Diameter,
		Pln:      spec.Struct.PlanetType,
	}
	if t.Diam == 0 {
		t.Diam = 4000
	}
	if t.CmdCtrLv == 0 {
		t.CmdCtrLv = 5
	}

	// Centre launchpad — index 1 in the game's 1-based numbering.
	const baseLa, baseLo = 1.10, 1.60
	t.P = append(t.P, Pin{La: baseLa, Lo: baseLo, T: spec.Struct.Launchpad})
	launchpad := 1

	// Ring layout: keeps links short enough to stay cheap.
	placed := 0
	for _, f := range spec.Factories {
		ft := factoryType(spec.Struct, f.Tier)
		for i := 0; i < f.Count; i++ {
			ring := 1 + (placed / 8)
			angle := float64(placed%8) * math.Pi / 4
			r := 0.018 * float64(ring)
			product := f.Product
			t.P = append(t.P, Pin{
				La: baseLa + r*math.Sin(angle),
				Lo: baseLo + r*math.Cos(angle),
				T:  ft,
				S:  &product,
			})
			pin := len(t.P) // 1-based index of the pin just added
			placed++

			// Link factory <-> launchpad.
			t.L = append(t.L, Link{S: launchpad, D: pin})

			// Route the finished goods back to the launchpad.
			t.R = append(t.R, Route{P: []int{pin, launchpad}, Q: f.Output, T: f.Product})

			// Route every input from the launchpad to this factory.
			ins := make([]int64, 0, len(f.Inputs))
			for in := range f.Inputs {
				ins = append(ins, in)
			}
			sort.Slice(ins, func(a, b int) bool { return ins[a] < ins[b] })
			for _, in := range ins {
				t.R = append(t.R, Route{P: []int{launchpad, pin}, Q: f.Inputs[in], T: in})
			}
		}
	}
	return t
}

// ── reskin: reuse a proven layout for another resource ───────────────

// ReskinSpec retargets a donor layout at another planet and recipe.
type ReskinSpec struct {
	Name string
	// Structure types of the target planet.
	PlanetType   int64
	Launchpad    int64
	BasicFactory int64
	AdvFactory   int64
	Storage      int64
	CommandCtr   int64
	// New commodities.
	Product int64 // what the factories make
	Input   int64 // what they consume
	// Roles of the donor's structure types, so they can be mapped over.
	RoleOf func(typeID int64) string // "launchpad" | "basic" | "advanced" | "storage" | "cc" | "extractor"
	// DropExtractors removes extractor pins (and everything referencing
	// them), renumbering the remaining pins.
	DropExtractors bool
}

// Reskin copies the donor's geometry — pin coordinates, links and route
// paths — and swaps in another planet's structures and another recipe.
// Hand-built layouts stay compact, which is what keeps link powergrid
// costs low, so reusing one beats generating a fresh ring layout.
func Reskin(donor *Template, s ReskinSpec) *Template {
	out := &Template{
		CmdCtrLv: donor.CmdCtrLv,
		Cmt:      s.Name,
		Diam:     donor.Diam,
		Pln:      s.PlanetType,
	}

	// Map old pin index (1-based) -> new index, dropping extractors.
	remap := make(map[int]int, len(donor.P))
	for i, p := range donor.P {
		old := i + 1
		role := ""
		if s.RoleOf != nil {
			role = s.RoleOf(p.T)
		}
		if s.DropExtractors && role == "extractor" {
			continue
		}
		np := Pin{H: p.H, La: p.La, Lo: p.Lo}
		switch role {
		case "launchpad":
			np.T = s.Launchpad
		case "advanced":
			np.T = s.AdvFactory
		case "storage":
			np.T = s.Storage
		case "cc":
			np.T = s.CommandCtr
		default: // basic factory
			np.T = s.BasicFactory
		}
		if p.S != nil {
			product := s.Product
			np.S = &product
		}
		out.P = append(out.P, np)
		remap[old] = len(out.P)
	}

	for _, l := range donor.L {
		ns, okS := remap[l.S]
		nd, okD := remap[l.D]
		if !okS || !okD {
			continue // touched a dropped pin
		}
		out.L = append(out.L, Link{S: ns, D: nd, Lv: l.Lv})
	}

	for _, r := range donor.R {
		path := make([]int, 0, len(r.P))
		ok := true
		for _, p := range r.P {
			np, exists := remap[p]
			if !exists {
				ok = false
				break
			}
			path = append(path, np)
		}
		if !ok {
			continue
		}
		// The donor's own commodities map onto the new recipe: whatever
		// leaves a factory is the product, whatever enters it the input.
		t := r.T
		if isProductRoute(donor, r) {
			t = s.Product
		} else {
			t = s.Input
		}
		out.R = append(out.R, Route{P: path, Q: r.Q, T: t})
	}
	return out
}

// isProductRoute reports whether a route carries goods away from a
// factory (its first pin is a factory pin).
func isProductRoute(t *Template, r Route) bool {
	if len(r.P) == 0 {
		return false
	}
	first := r.P[0]
	if first < 1 || first > len(t.P) {
		return false
	}
	return t.P[first-1].S != nil
}

// RefinerySpec describes a P0→P1 refinery planet: a launchpad ringed by
// basic factories, no extractor (those depend on the actual planet's
// resource hotspots and are placed by hand in the client).
type RefinerySpec struct {
	Name       string
	CmdCtrLv   int
	Diameter   float64
	PlanetType int64
	Launchpad  int64
	Factory    int64
	Product    int64 // P1 produced
	Input      int64 // P0 consumed
	InputQty   int64 // per cycle (3000)
	OutputQty  int64 // per cycle (20)
	Count      int   // how many factories
	// Hub connects to at most HubFanout factories directly; the rest
	// daisy-chain through them, exactly like hand-built colonies do.
	HubFanout int
}

// GenerateRefinery builds the extractor-less refinery layout.
func GenerateRefinery(s RefinerySpec) *Template {
	if s.Count <= 0 {
		s.Count = 13
	}
	if s.HubFanout <= 0 {
		s.HubFanout = 5
	}
	if s.CmdCtrLv == 0 {
		s.CmdCtrLv = 5
	}
	if s.Diameter == 0 {
		s.Diameter = 4460
	}
	t := &Template{
		CmdCtrLv: s.CmdCtrLv,
		Cmt:      s.Name,
		Diam:     s.Diameter,
		Pln:      s.PlanetType,
	}

	const baseLa, baseLo = 1.7244, 4.53465 // same neighbourhood as hand-built ones
	t.P = append(t.P, Pin{La: baseLa, Lo: baseLo, T: s.Launchpad})
	const launchpad = 1

	// parents[i] is the pin every factory routes through (launchpad or a
	// first-ring factory).
	parents := make([]int, 0, s.Count)
	for i := 0; i < s.Count; i++ {
		product := s.Product
		ring := 1
		slot := i
		if i >= s.HubFanout {
			ring = 2 + (i-s.HubFanout)/s.HubFanout
			slot = (i - s.HubFanout) % s.HubFanout
		}
		angle := 2 * math.Pi * float64(slot) / float64(s.HubFanout)
		r := 0.012 * float64(ring)
		t.P = append(t.P, Pin{
			La: round5(baseLa + r*math.Sin(angle)),
			Lo: round5(baseLo + r*math.Cos(angle)),
			T:  s.Factory,
			S:  &product,
		})
		pin := len(t.P)

		parent := launchpad
		if i >= s.HubFanout {
			parent = parents[(i-s.HubFanout)%s.HubFanout] // hang off a first-ring factory
		}
		parents = append(parents, pin)
		t.L = append(t.L, Link{S: parent, D: pin})

		// Routes carry goods along the physical path, hop by hop.
		path := []int{launchpad}
		if parent != launchpad {
			path = append(path, parent)
		}
		path = append(path, pin)
		t.R = append(t.R, Route{P: path, Q: s.InputQty, T: s.Input})

		back := make([]int, len(path))
		for i := range path {
			back[i] = path[len(path)-1-i]
		}
		t.R = append(t.R, Route{P: back, Q: s.OutputQty, T: s.Product})
	}
	return t
}

func round5(v float64) float64 { return math.Round(v*1e5) / 1e5 }

// factoryType picks the processor matching the product tier.
func factoryType(s PlanetStructures, tier string) int64 {
	switch tier {
	case "P1":
		return s.BasicFactory
	case "P4":
		if s.HighTech != 0 {
			return s.HighTech
		}
		return s.AdvFactory
	default: // P2, P3
		return s.AdvFactory
	}
}
