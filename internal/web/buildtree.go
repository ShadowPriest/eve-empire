package web

// "Build or buy", one material at a time.
//
// Every material of a blueprint may itself have a blueprint, so the bill
// is really a tree: a Sabre eats T2 components, those eat minerals and
// planetary goods, and each node can either be bought off Jita or built
// from its own children. The page answers per node — and the answer of a
// child changes what its parent costs, so the tree is priced bottom-up.
//
// TWO THINGS THAT ARE EASY TO GET WRONG AND ARE HANDLED HERE:
//   - runs are whole. Needing 14 units of something a print makes 100 at
//     a time means paying for 100 and carrying 86 as surplus, and the
//     comparison has to include that waste or it lies in favour of
//     building.
//   - the installation fee is per JOB. Building the components adds one
//     fee each on top of the final assembly — a tree of 8 components is
//     9 jobs, and those fees are part of what "make" costs.

import (
	"math"
	"sort"

	"eve-empire/internal/esi"
	"eve-empire/internal/sde"
)

// Component-tree limits. Depth 1 = only the blueprint's own materials
// (the "buy everything" view); anything deeper is walked only when the
// page is actually deciding build-or-buy, and the node cap keeps a
// capital ship from turning into a thousand order-book requests.
const (
	buildDepthBuy  = 1
	buildDepthMake = 4
	// Two different limits, because two different things are expensive.
	// Prices cost NETWORK — one order-book request per distinct type — so
	// the type collector stops at a few hundred. Tree nodes cost nothing
	// but CPU and repeat the same types over and over (fuel blocks show up
	// in every reaction branch), so their cap is only a runaway guard.
	buildMaxTypes = 400
	buildMaxNodes = 5000
)

// Component decision modes.
const (
	modeBuy  = "buy"  // buy every material, only hint at what is cheaper
	modeAuto = "auto" // per node: whichever is cheaper
	modeMake = "make" // build everything that has a blueprint
)

// matCache keeps the material list of each blueprint in memory: the walk
// asks for the same lists over and over (once to price the children,
// once for the estimated item value), and they never change.
type matCache struct {
	db *sde.DB
	m  map[int64][]sde.BPItem
}

func newMatCache(db *sde.DB) *matCache {
	return &matCache{db: db, m: map[int64][]sde.BPItem{}}
}

func (c *matCache) of(rec sde.Recipe) []sde.BPItem {
	if v, ok := c.m[rec.BlueprintID]; ok {
		return v
	}
	v := c.db.RecipeMaterials(rec)
	c.m[rec.BlueprintID] = v
	return v
}

// buildNode is one line of the bill: a material that is either bought
// or built from its own recipe.
type buildNode struct {
	TypeID int64
	Name   string
	Group  string
	Depth  int
	Base   int64 // per parent run, straight from the SDE
	Need   int64 // units the parent job consumes

	Price    float64 // Jita unit price
	HasPrice bool
	BuyCost  float64

	// Filled when the type has a recipe of its own.
	CanMake  bool
	Made     bool
	Rec      sde.Recipe
	Runs     int64
	Jobs     int64 // runs split by the print's max production limit
	Output   int64 // units those runs produce
	Surplus  int64 // output over need — paid for, not consumed
	JobFee   float64
	MakeCost float64 // children + this node's fee
	Children []*buildNode

	Cost     float64 // what the parent really pays: BuyCost or MakeCost
	UnitCost float64 // Cost per needed unit — comparable with Price
	Delta    float64 // make against buy, per cent; negative = cheaper to make
	HasDelta bool
	Volume   float64 // m³ of what is actually hauled for this line
	NoVolume bool    // a hull: the SDE only knows its assembled volume
	Share    float64 // per cent of the material bill
	Toggle   string  // URL that flips this node's decision
}

// buildTree carries what the walk needs plus the totals it sums up.
type buildTree struct {
	sde      *sde.DB
	mats     *matCache
	book     map[int64]esi.OrderStats
	adjusted map[int64]esi.MarketPrice
	volumes  map[int64]float64
	hulls    map[int64]bool
	stat     string
	feeRate  float64
	compME   int
	te       int     // blueprint TE applied to sub-jobs as well
	matMul   float64 // structure material multiplier, 1 = plain station
	timeMul  float64 // structure time multiplier, manufacturing
	rtimeMul float64 // structure time multiplier, reactions
	mode     string
	makeSet  map[int64]bool // explicit "build this one"
	buySet   map[int64]bool // explicit "buy this one"
	maxDepth int
	nodes    int

	// totals, filled while pricing
	SubFee  float64 // installation fees of every sub-job
	SubJobs int64
	SubTime float64 // seconds of sub-job work, if run one after another
	Volume  float64 // m³ of everything actually bought
	Missing int     // bought lines without a price
	Hulls   int     // bought hulls, whose packaged volume is unknown
	Capped  bool    // the node cap stopped the walk somewhere
}

// collectTypes walks the recipe tree without prices, gathering every type
// the calculation will need a price for. The order book is then fetched
// once for all of them instead of level by level.
func collectTypes(mats *matCache, db *sde.DB, rec sde.Recipe, maxDepth int) []int64 {
	seen := map[int64]bool{rec.ProductID: true}
	ids := []int64{rec.ProductID}
	var walk func(r sde.Recipe, depth int, path map[int64]bool)
	walk = func(r sde.Recipe, depth int, path map[int64]bool) {
		if depth > maxDepth || len(ids) > buildMaxTypes {
			return
		}
		for _, m := range mats.of(r) {
			if !seen[m.TypeID] {
				seen[m.TypeID] = true
				ids = append(ids, m.TypeID)
			}
			if path[m.TypeID] {
				continue
			}
			sub, ok := db.RecipeForProduct(m.TypeID)
			if !ok {
				continue
			}
			path[m.TypeID] = true
			walk(sub, depth+1, path)
			delete(path, m.TypeID)
		}
	}
	walk(rec, 1, map[int64]bool{rec.ProductID: true})
	return ids
}

// materials prices one job's material list, deciding build-or-buy for
// every line, and returns the lines with what they cost together.
func (t *buildTree) materials(rec sde.Recipe, runs int64, me, depth int, path map[int64]bool) ([]*buildNode, float64) {
	list := t.mats.of(rec)
	nodes := make([]*buildNode, 0, len(list))
	var total float64
	for _, m := range list {
		n := t.node(m, rec, runs, me, depth, path)
		total += n.Cost
		nodes = append(nodes, n)
	}
	sort.Slice(nodes, func(i, j int) bool {
		if nodes[i].Cost != nodes[j].Cost {
			return nodes[i].Cost > nodes[j].Cost
		}
		return nodes[i].Need > nodes[j].Need
	})
	for _, n := range nodes {
		if total > 0 {
			n.Share = n.Cost / total * 100
		}
	}
	return nodes, total
}

func (t *buildTree) node(m sde.BPItem, parent sde.Recipe, runs int64, me, depth int, path map[int64]bool) *buildNode {
	n := &buildNode{
		TypeID: m.TypeID, Name: m.Name, Group: m.Group, Depth: depth,
		Base: m.Quantity,
		Need: buildQty(m.Quantity, runs, me, parent.IsReaction(), t.matMul),
	}
	t.nodes++
	if st, ok := t.book[m.TypeID]; ok {
		if p := st.Pick(t.stat); p > 0 {
			n.Price, n.HasPrice = p, true
			n.BuyCost = p * float64(n.Need)
		}
	}

	// Can it be built, and are we allowed to look this deep? An explicit
	// "build this one" is always honoured, even past the depth limit.
	sub, hasRecipe := t.sde.RecipeForProduct(m.TypeID)
	switch {
	case !hasRecipe || path[m.TypeID]:
	case t.nodes > buildMaxNodes:
		t.Capped = true
	// maxDepth counts levels that may be BUILT. A node one level past it
	// is still priced both ways — that is what makes the "cheaper to
	// make" hint appear while the mode is still "buy everything".
	case depth > t.maxDepth && !t.makeSet[m.TypeID]:
	default:
		n.CanMake = true
		n.Rec = sub
		// Runs are whole: a print that makes 100 at a time is paid for in
		// hundreds even when 14 are needed.
		perRun := sub.ProductQty
		if perRun < 1 {
			perRun = 1
		}
		n.Runs = int64(math.Ceil(float64(n.Need) / float64(perRun)))
		n.Output = n.Runs * perRun
		n.Surplus = n.Output - n.Need
		n.Jobs = 1

		path[m.TypeID] = true
		children, childCost := t.materials(sub, n.Runs, t.compME, depth+1, path)
		delete(path, m.TypeID)

		var eiv float64
		for _, cm := range t.mats.of(sub) {
			eiv += t.adjusted[cm.TypeID].Adjusted * float64(cm.Quantity) * float64(n.Runs)
		}
		n.Children = children
		n.JobFee = eiv * t.feeRate
		n.MakeCost = childCost + n.JobFee
	}

	n.Made = t.decide(n)
	if n.Made {
		n.Cost = n.MakeCost
		t.SubFee += n.JobFee
		t.SubJobs += n.Jobs
		// Sub-jobs inherit the page's TE and the structure's time bonus;
		// reactions take neither TE nor the manufacturing multiplier.
		mul := t.timeMul * (1 - float64(t.te)/100)
		if n.Rec.IsReaction() {
			mul = t.rtimeMul
		}
		t.SubTime += float64(n.Rec.Time) * float64(n.Runs) * mul
	} else {
		n.Cost = n.BuyCost
		n.Children = nil // a bought line has no bill of its own
		if !n.HasPrice {
			t.Missing++
		}
		if t.hulls[m.TypeID] {
			n.NoVolume = true
			t.Hulls++
		} else {
			n.Volume = float64(n.Need) * t.volumes[m.TypeID]
			t.Volume += n.Volume
		}
	}
	if n.Need > 0 {
		n.UnitCost = n.Cost / float64(n.Need)
	}
	if n.CanMake && n.HasPrice && n.BuyCost > 0 {
		n.Delta = (n.MakeCost/n.BuyCost - 1) * 100
		n.HasDelta = true
	}
	return n
}

// decide answers build-or-buy for one node: an explicit click wins, then
// the mode, and "auto" compares the two costs. A material Jita does not
// price at all is built whenever it can be — otherwise a missing price
// would silently count as free.
func (t *buildTree) decide(n *buildNode) bool {
	if !n.CanMake {
		return false
	}
	if t.buySet[n.TypeID] {
		return false
	}
	if t.makeSet[n.TypeID] {
		return true
	}
	switch t.mode {
	case modeMake:
		return true
	case modeAuto:
		if !n.HasPrice {
			return true
		}
		return n.MakeCost < n.BuyCost
	default:
		return false
	}
}

// flatten turns the tree into the row list the template walks, parents
// before their own children.
func flatten(nodes []*buildNode) []*buildNode {
	var out []*buildNode
	for _, n := range nodes {
		out = append(out, n)
		out = append(out, flatten(n.Children)...)
	}
	return out
}
