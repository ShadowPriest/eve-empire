// Package skillplan holds the maths behind the skill planner: what a plan
// costs, in which order it may legally be trained, and which attribute
// remap / implants make it finish soonest.
//
// Four facts drive everything here.
//
//  1. A level costs SP = rank × base[level], base = {250, 1414, 8000,
//     45255, 256000}. Those numbers are CUMULATIVE, so one level of a plan
//     costs rank × (base[L] − base[L−1]) — the increments are
//     {250, 1164, 6586, 37255, 210745}. Level 5 alone is 4.6× levels 1-4
//     together, which is why "everything to 4" is so much cheaper than
//     "everything to 5".
//
//  2. Training rate is `primary + secondary/2` SP per minute, where each
//     attribute is 17 (base) + remap points + implants + boosters. The
//     rate depends on the SKILL, not on the plan, so two plans of equal SP
//     can differ by weeks.
//
//  3. Time is therefore Σ SP_i / rate(skill_i) — a sum of independent
//     terms. Reordering never changes it; only changing the SET of skills
//     or the attributes does. That is why Order() optimizes for "useful
//     things sooner" while Remap() optimizes for total time.
//
//  4. Prerequisites constrain the order: level N needs N−1 of the same
//     skill plus every prerequisite skill at its required level. Any
//     reordering must stay inside that partial order.
package skillplan

import (
	"regexp"
	"sort"
	"strings"
	"time"
)

// Skill is the catalog entry the planner needs (filled from internal/sde).
type Skill struct {
	ID   int64
	Name string // localized name, shown everywhere
	En   string // English name — what the game's hint="" attribute carries
	Rank int
	Prim string // character attribute key: intelligence, memory, ...
	Sec  string
	Pre  map[int64]int // prerequisite skill id -> level
}

// Entry is one line of a plan: train SkillID up to Level.
type Entry struct {
	SkillID int64
	Level   int
}

// AttrKeys is the fixed attribute order used by every table we render.
var AttrKeys = []string{"intelligence", "memory", "perception", "willpower", "charisma"}

// AttrRu gives the Russian label for an attribute key.
var AttrRu = map[string]string{
	"intelligence": "интеллект",
	"memory":       "память",
	"perception":   "восприятие",
	"willpower":    "воля",
	"charisma":     "харизма",
}

// Attrs is a set of character attribute values (already including
// implants when the caller wants them included).
type Attrs map[string]int

// Character attribute rules: everyone starts at 17 with 14 points to
// spend and no attribute may go above 27 (that is +10).
const (
	AttrBase   = 17
	RemapPool  = 14
	RemapMax   = 10
	planLimit  = 400 // sanity cap on imported lines
)

// cumulative SP multipliers per level
var cumBase = [6]int64{0, 250, 1414, 8000, 45255, 256000}

// TotalSP is the SP a character has once the skill sits at `level`.
func TotalSP(rank, level int) int64 {
	if rank <= 0 || level <= 0 {
		return 0
	}
	if level > 5 {
		level = 5
	}
	return int64(rank) * cumBase[level]
}

// LevelSP is the cost of the single step (level−1) → level.
func LevelSP(rank, level int) int64 {
	return TotalSP(rank, level) - TotalSP(rank, level-1)
}

// Rate returns SP per minute for a skill trained with these attributes.
func (a Attrs) Rate(prim, sec string) float64 {
	p, s := a[prim], a[sec]
	if p == 0 {
		p = AttrBase
	}
	if s == 0 {
		s = AttrBase
	}
	return float64(p) + float64(s)/2
}

// Plus returns a copy with `add` folded in (used for implants/boosters).
func (a Attrs) Plus(add Attrs) Attrs {
	out := Attrs{}
	for _, k := range AttrKeys {
		out[k] = a[k] + add[k]
	}
	return out
}

// Alloc turns remap points into attribute values.
func Alloc(points Attrs) Attrs {
	out := Attrs{}
	for _, k := range AttrKeys {
		out[k] = AttrBase + points[k]
	}
	return out
}

// ── import ───────────────────────────────────────────────────────────

// The game puts plan lines on the clipboard as
//
//	<localized hint="Biology">Биология*</localized> 4
//
// but people also paste plain "Biology 4" or "Biology V", so we accept
// all three.
var (
	reHint  = regexp.MustCompile(`hint="([^"]+)"`)
	reTail  = regexp.MustCompile(`^(.*?)[\s\x{00a0}]+([IVX]+|[1-5])\s*$`)
	roman   = map[string]int{"I": 1, "II": 2, "III": 3, "IV": 4, "V": 5}
	reStrip = regexp.MustCompile(`<[^>]*>`)
)

// Parse reads a pasted plan. Unknown skill names are returned separately
// instead of being silently dropped — a typo must be visible.
func Parse(text string, lookup func(string) (Skill, bool)) (entries []Entry, unknown []string) {
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "//") {
			continue
		}
		var name string
		if m := reHint.FindStringSubmatch(line); m != nil {
			name = m[1] // English name from the hint — the reliable one
		}
		plain := strings.TrimSpace(reStrip.ReplaceAllString(line, " "))
		m := reTail.FindStringSubmatch(plain)
		if m == nil {
			unknown = append(unknown, line)
			continue
		}
		level := roman[m[2]]
		if level == 0 {
			level = int(m[2][0] - '0')
		}
		if name == "" {
			name = strings.TrimSuffix(strings.TrimSpace(m[1]), "*")
		}
		sk, ok := lookup(name)
		if !ok {
			// second chance: the localized name in front of the tag
			if sk, ok = lookup(strings.TrimSuffix(strings.TrimSpace(m[1]), "*")); !ok {
				unknown = append(unknown, line)
				continue
			}
		}
		if level < 1 || level > 5 {
			unknown = append(unknown, line)
			continue
		}
		entries = append(entries, Entry{SkillID: sk.ID, Level: level})
		if len(entries) >= planLimit {
			break
		}
	}
	return entries, unknown
}

// Format renders entries back in the game's clipboard format, so an
// optimized plan can go straight back into the client.
func Format(entries []Entry, cat map[int64]Skill) string {
	var b strings.Builder
	for _, e := range entries {
		sk := cat[e.SkillID]
		en := sk.En
		if en == "" {
			en = sk.Name
		}
		b.WriteString(`<localized hint="`)
		b.WriteString(en)
		b.WriteString(`">`)
		b.WriteString(sk.Name)
		b.WriteString(`*</localized> `)
		b.WriteByte(byte('0' + e.Level))
		b.WriteByte('\n')
	}
	return b.String()
}

// ── ordering ─────────────────────────────────────────────────────────

// Order modes.
const (
	OrderKeep  = "keep"  // original order, only fixing prerequisite violations
	OrderCheap = "cheap" // cheapest level first  — most skills finished per SP
	OrderFast  = "fast"  // shortest level first  — most skills finished per hour
	OrderLong  = "long"  // longest first — when you want the big rocks done
)

// Order re-sorts a plan without ever breaking a prerequisite.
//
// The algorithm is a greedy topological sort over the partial order from
// fact (4): repeatedly collect every entry whose predecessor level and
// prerequisite skills are already satisfied, pick the best one by the
// chosen key, apply it, repeat. Greedy is optimal for OrderCheap/OrderFast
// in the only sense that matters here — after k steps you have finished
// the k cheapest (fastest) levels reachable at all.
//
// `trained` holds levels the character already has. Skills that are not
// in the plan are assumed trained to whatever their dependents need,
// which is what an imported plan implies.
func Order(entries []Entry, trained map[int64]int, cat map[int64]Skill, mode string, attrs Attrs) (out, stuck []Entry) {
	cur := map[int64]int{}
	inPlan := map[int64]bool{}
	for _, e := range entries {
		inPlan[e.SkillID] = true
	}
	for id := range inPlan {
		cur[id] = trained[id]
	}
	// a level below the lowest planned one is implicitly already trained
	lowest := map[int64]int{}
	for _, e := range entries {
		if l, ok := lowest[e.SkillID]; !ok || e.Level < l {
			lowest[e.SkillID] = e.Level
		}
	}
	for id, l := range lowest {
		if cur[id] < l-1 {
			cur[id] = l - 1
		}
	}
	level := func(id int64) int {
		if inPlan[id] {
			return cur[id]
		}
		if l, ok := trained[id]; ok {
			return l
		}
		return 5 // outside the plan → assume it is already there
	}
	ready := func(e Entry) bool {
		if level(e.SkillID) != e.Level-1 {
			return false
		}
		for pre, need := range cat[e.SkillID].Pre {
			if level(pre) < need {
				return false
			}
		}
		return true
	}
	cost := func(e Entry) float64 {
		sk := cat[e.SkillID]
		sp := float64(LevelSP(sk.Rank, e.Level))
		switch mode {
		case OrderFast:
			return sp / attrs.Rate(sk.Prim, sk.Sec)
		case OrderLong:
			return -sp
		default:
			return sp
		}
	}

	left := append([]Entry(nil), entries...)
	pos := map[Entry]int{}
	for i, e := range entries {
		if _, seen := pos[e]; !seen {
			pos[e] = i
		}
	}
	for len(left) > 0 {
		best := -1
		for i, e := range left {
			if !ready(e) {
				continue
			}
			if mode == OrderKeep {
				best = i // first ready wins → original order preserved
				break
			}
			if best < 0 || cost(e) < cost(left[best]) ||
				(cost(e) == cost(left[best]) && pos[e] < pos[left[best]]) {
				best = i
			}
		}
		if best < 0 {
			return out, left // prerequisites can never be met
		}
		e := left[best]
		cur[e.SkillID] = e.Level
		out = append(out, e)
		left = append(left[:best], left[best+1:]...)
	}
	return out, nil
}

// ── costing ──────────────────────────────────────────────────────────

// Step is one plan line with everything the page shows.
type Step struct {
	Entry
	Name   string
	Rank   int
	Prim   string
	Sec    string
	SP     int64
	Rate   float64 // SP per minute
	Dur    time.Duration
	CumSP  int64
	CumDur time.Duration
	Done   time.Time
}

// Plan is a costed plan.
type Plan struct {
	Steps   []Step
	TotalSP int64
	Total   time.Duration
	Ends    time.Time
}

// Build costs a plan: per-level SP, rate, duration and running totals.
func Build(entries []Entry, cat map[int64]Skill, attrs Attrs, start time.Time) Plan {
	var p Plan
	for _, e := range entries {
		sk := cat[e.SkillID]
		sp := LevelSP(sk.Rank, e.Level)
		rate := attrs.Rate(sk.Prim, sk.Sec)
		dur := time.Duration(float64(sp) / rate * float64(time.Minute))
		p.TotalSP += sp
		p.Total += dur
		p.Steps = append(p.Steps, Step{
			Entry: e, Name: sk.Name, Rank: sk.Rank, Prim: sk.Prim, Sec: sk.Sec,
			SP: sp, Rate: rate, Dur: dur, CumSP: p.TotalSP, CumDur: p.Total,
			Done: start.Add(p.Total),
		})
	}
	p.Ends = start.Add(p.Total)
	return p
}

// MeanDone is the average moment a level of the plan is finished.
//
// Total time is fixed (fact 3), but the ORDER decides how long each level
// waits for its turn, and that is a classic scheduling problem: putting
// the shortest job first minimizes the mean completion time. That is the
// whole justification for OrderCheap/OrderFast — they do not shorten the
// plan, they make it useful sooner, and this number measures by how much.
func MeanDone(entries []Entry, cat map[int64]Skill, attrs Attrs) time.Duration {
	if len(entries) == 0 {
		return 0
	}
	var running, sum float64
	for _, e := range entries {
		sk := cat[e.SkillID]
		running += float64(LevelSP(sk.Rank, e.Level)) / attrs.Rate(sk.Prim, sk.Sec)
		sum += running
	}
	return time.Duration(sum / float64(len(entries)) * float64(time.Minute))
}

// Duration is Σ SP/rate for a whole plan — the objective every optimizer
// below minimizes.
func Duration(entries []Entry, cat map[int64]Skill, attrs Attrs) time.Duration {
	var total float64
	for _, e := range entries {
		sk := cat[e.SkillID]
		total += float64(LevelSP(sk.Rank, e.Level)) / attrs.Rate(sk.Prim, sk.Sec)
	}
	return time.Duration(total * float64(time.Minute))
}

// ── attribute pairs ──────────────────────────────────────────────────

// Pair is one primary/secondary bucket of a plan.
type Pair struct {
	Prim, Sec string
	SP        int64
	Share     float64 // % of plan SP
	Rate      float64
	Dur       time.Duration
	Lost      time.Duration // vs training the same SP in the best pair
	Skills    []string
}

// Pairs groups a plan by attribute pair — this is the diagnostic that
// shows WHERE a plan wastes time, since only the pair decides the rate.
func Pairs(entries []Entry, cat map[int64]Skill, attrs Attrs) []Pair {
	type acc struct {
		sp     int64
		skills map[string]int64
	}
	buckets := map[[2]string]*acc{}
	var total int64
	best := 0.0
	for _, e := range entries {
		sk := cat[e.SkillID]
		k := [2]string{sk.Prim, sk.Sec}
		b := buckets[k]
		if b == nil {
			b = &acc{skills: map[string]int64{}}
			buckets[k] = b
		}
		sp := LevelSP(sk.Rank, e.Level)
		b.sp += sp
		b.skills[sk.Name] += sp
		total += sp
		if r := attrs.Rate(sk.Prim, sk.Sec); r > best {
			best = r
		}
	}
	var out []Pair
	for k, b := range buckets {
		rate := attrs.Rate(k[0], k[1])
		dur := time.Duration(float64(b.sp) / rate * float64(time.Minute))
		ideal := time.Duration(float64(b.sp) / best * float64(time.Minute))
		p := Pair{Prim: k[0], Sec: k[1], SP: b.sp, Rate: rate, Dur: dur, Lost: dur - ideal}
		if total > 0 {
			p.Share = float64(b.sp) / float64(total) * 100
		}
		type ns struct {
			n  string
			sp int64
		}
		var list []ns
		for n, sp := range b.skills {
			list = append(list, ns{n, sp})
		}
		sort.Slice(list, func(i, j int) bool { return list[i].sp > list[j].sp })
		for i, x := range list {
			if i >= 6 {
				break
			}
			p.Skills = append(p.Skills, x.n)
		}
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].SP > out[j].SP })
	return out
}

// ── remap ────────────────────────────────────────────────────────────

// RemapOption is one candidate attribute allocation.
type RemapOption struct {
	Points Attrs // points spent above the base 17
	Attrs  Attrs // resulting values
	Dur    time.Duration
}

// Remap brute-forces the best attribute allocation for a plan.
//
// The search space is tiny: 14 points over 5 attributes, none above +10,
// which is 1 001 compositions — so there is no need for a clever
// heuristic, and unlike "put everything in the most-used attribute" the
// exhaustive search gets the secondary right (it is worth half a point
// per point, so the last points often belong in the secondary).
//
// `implants` is added on top of the allocation, because the optimum
// shifts slightly when a +3/+5 set is already plugged in.
func Remap(entries []Entry, cat map[int64]Skill, implants Attrs, top int) []RemapOption {
	var out []RemapOption
	var pts [5]int
	var walk func(i, left int)
	walk = func(i, left int) {
		if i == len(AttrKeys)-1 {
			if left > RemapMax {
				return
			}
			pts[i] = left
			p := Attrs{}
			for j, k := range AttrKeys {
				p[k] = pts[j]
			}
			a := Alloc(p).Plus(implants)
			out = append(out, RemapOption{Points: p, Attrs: a, Dur: Duration(entries, cat, a)})
			return
		}
		for n := 0; n <= RemapMax && n <= left; n++ {
			pts[i] = n
			walk(i+1, left-n)
		}
	}
	walk(0, RemapPool)
	sort.Slice(out, func(i, j int) bool { return out[i].Dur < out[j].Dur })
	if top > 0 && len(out) > top {
		out = out[:top]
	}
	return out
}

// ── implants ─────────────────────────────────────────────────────────

// Gain is the time one implant saves on a plan.
type Gain struct {
	Attr  string
	Bonus int
	Saved time.Duration
	Dur   time.Duration
}

// Implants ranks single implants of the given grade by time saved, then
// appends the full set. Marginal value is what matters: an implant in an
// attribute the plan barely uses is worth days, one in the primary is
// worth weeks.
func Implants(entries []Entry, cat map[int64]Skill, attrs Attrs, bonus int) (gains []Gain, full Gain) {
	base := Duration(entries, cat, attrs)
	for _, k := range AttrKeys {
		a := attrs.Plus(Attrs{k: bonus})
		d := Duration(entries, cat, a)
		gains = append(gains, Gain{Attr: k, Bonus: bonus, Dur: d, Saved: base - d})
	}
	sort.Slice(gains, func(i, j int) bool { return gains[i].Saved > gains[j].Saved })
	set := Attrs{}
	for _, k := range AttrKeys {
		set[k] = bonus
	}
	d := Duration(entries, cat, attrs.Plus(set))
	full = Gain{Attr: "all", Bonus: bonus, Dur: d, Saved: base - d}
	return gains, full
}

// ── boosters ─────────────────────────────────────────────────────────

// Booster is what a cerebral accelerator does to a plan: it raises every
// attribute by Bonus for a while, and Biology stretches that while by
// 20% per level.
type Booster struct {
	Bonus    int
	BaseDays float64
	Biology  int

	Days    float64       // effective duration
	SPGain  int64         // extra SP earned inside the window
	Saved   time.Duration // how much earlier the plan ends
}

// Accelerator evaluates a booster against the first part of a plan. The
// window is short compared to the plan, so the extra SP is earned at the
// boosted rate of whatever is being trained then — we take the plan's
// leading steps until the window is full.
func Accelerator(entries []Entry, cat map[int64]Skill, attrs Attrs, bonus int, baseDays float64, biology int) Booster {
	b := Booster{Bonus: bonus, BaseDays: baseDays, Biology: biology}
	b.Days = baseDays * (1 + 0.2*float64(biology))
	boosted := Attrs{}
	for _, k := range AttrKeys {
		boosted[k] = bonus
	}
	up := attrs.Plus(boosted)

	left := b.Days * 24 * 60 // minutes of booster left
	var plain, fast float64  // SP the window yields without / with the booster
	for _, e := range entries {
		sk := cat[e.SkillID]
		sp := float64(LevelSP(sk.Rank, e.Level))
		r0, r1 := attrs.Rate(sk.Prim, sk.Sec), up.Rate(sk.Prim, sk.Sec)
		mins := sp / r1
		if mins > left {
			mins = left
		}
		plain += mins * r0
		fast += mins * r1
		left -= mins
		if left <= 0 {
			break
		}
	}
	b.SPGain = int64(fast - plain)
	// the gained SP is time you no longer have to spend at the plain rate
	if avg := avgRate(entries, cat, attrs); avg > 0 {
		b.Saved = time.Duration(float64(b.SPGain) / avg * float64(time.Minute))
	}
	return b
}

func avgRate(entries []Entry, cat map[int64]Skill, attrs Attrs) float64 {
	var sp float64
	d := Duration(entries, cat, attrs)
	for _, e := range entries {
		sp += float64(LevelSP(cat[e.SkillID].Rank, e.Level))
	}
	if d == 0 {
		return 0
	}
	return sp / d.Minutes()
}
