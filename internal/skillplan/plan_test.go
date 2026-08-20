package skillplan

import (
	"strings"
	"testing"
	"time"
)

// a small catalog in the shape of the real one
var cat = map[int64]Skill{
	1: {ID: 1, Name: "Кибернетика", En: "Cybernetics", Rank: 3, Prim: "intelligence", Sec: "memory", Pre: map[int64]int{}},
	2: {ID: 2, Name: "Установка модификаторов", En: "Jury Rigging", Rank: 2, Prim: "intelligence", Sec: "memory", Pre: map[int64]int{}},
	3: {ID: 3, Name: "Модификаторы двигателей", En: "Astronautics Rigging", Rank: 3, Prim: "intelligence", Sec: "memory", Pre: map[int64]int{2: 3}},
	4: {ID: 4, Name: "Дроны", En: "Drones", Rank: 1, Prim: "memory", Sec: "perception", Pre: map[int64]int{}},
	5: {ID: 5, Name: "Модификации наземной базы", En: "Command Center Upgrades", Rank: 4, Prim: "charisma", Sec: "intelligence", Pre: map[int64]int{}},
}

func lookup(name string) (Skill, bool) {
	for _, s := range cat {
		if strings.EqualFold(s.En, name) || strings.EqualFold(s.Name, name) {
			return s, true
		}
	}
	return Skill{}, false
}

func TestLevelSP(t *testing.T) {
	// canonical numbers: rank 4 to level 4 is 181 020, and level 5 alone
	// costs 842 980 — 4.6× everything below it
	if got := TotalSP(4, 4); got != 181020 {
		t.Errorf("TotalSP(4,4) = %d, хочу 181020", got)
	}
	if got := LevelSP(4, 5); got != 842980 {
		t.Errorf("LevelSP(4,5) = %d, хочу 842980", got)
	}
	if got := LevelSP(1, 2); got != 1164 {
		t.Errorf("LevelSP(1,2) = %d, хочу 1164", got)
	}
	if got := LevelSP(3, 3); got != 19758 {
		t.Errorf("LevelSP(3,3) = %d, хочу 19758", got)
	}
}

func TestParse(t *testing.T) {
	in := `<localized hint="Cybernetics">Кибернетика*</localized> 2
Drones V
Jury Rigging 3
Совсем не навык 9`
	got, unknown := Parse(in, lookup)
	if len(got) != 3 {
		t.Fatalf("разобрано %d строк, хочу 3: %+v", len(got), got)
	}
	if got[0] != (Entry{SkillID: 1, Level: 2}) {
		t.Errorf("первая строка = %+v", got[0])
	}
	if got[1] != (Entry{SkillID: 4, Level: 5}) {
		t.Errorf("римская цифра не разобрана: %+v", got[1])
	}
	if len(unknown) != 1 {
		t.Errorf("непонятых строк %d, хочу 1: %v", len(unknown), unknown)
	}
}

func TestOrderKeepsPrerequisites(t *testing.T) {
	// Astronautics Rigging needs Jury Rigging 3, and it is cheaper at
	// level 1 than Jury Rigging 3 — a naive cheapest-first would put it
	// first and produce a queue the game rejects.
	entries := []Entry{
		{3, 1}, {2, 1}, {2, 2}, {2, 3},
	}
	out, stuck := Order(entries, map[int64]int{}, cat, OrderCheap, Alloc(Attrs{}))
	if len(stuck) != 0 {
		t.Fatalf("остались недостижимые: %+v", stuck)
	}
	seen := map[int64]int{}
	for _, e := range out {
		if e.SkillID == 3 && seen[2] < 3 {
			t.Fatalf("Astronautics Rigging встал раньше Jury Rigging 3: %+v", out)
		}
		seen[e.SkillID] = e.Level
	}
	if len(out) != len(entries) {
		t.Errorf("потеряны строки: %d из %d", len(out), len(entries))
	}
}

func TestOrderCheapestFirst(t *testing.T) {
	entries := []Entry{{1, 3}, {4, 2}, {4, 1}}
	out, _ := Order(entries, map[int64]int{}, cat, OrderCheap, Alloc(Attrs{}))
	// Drones 1 (250) < Drones 2 (1164) < Cybernetics 3 (19758 with the
	// implied levels 1-2 already trained)
	if out[0] != (Entry{4, 1}) || out[1] != (Entry{4, 2}) {
		t.Errorf("порядок не по возрастанию цены: %+v", out)
	}
}

func TestRemapPrefersTheDominantPair(t *testing.T) {
	// a plan that is overwhelmingly intelligence/memory must map to
	// 27/21 — the classic maximum with 14 points
	entries := []Entry{{1, 5}, {2, 5}, {3, 5}, {4, 1}}
	best := Remap(entries, cat, Attrs{}, 3)
	if len(best) == 0 {
		t.Fatal("Remap ничего не вернул")
	}
	got := best[0].Attrs
	if got["intelligence"] != 27 || got["memory"] != 21 {
		t.Errorf("ремап = инт %d / пам %d, хочу 27/21 (%+v)", got["intelligence"], got["memory"], got)
	}
	// spending all 14 points is never worse than leaving some unspent
	var sum int
	for _, k := range AttrKeys {
		sum += best[0].Points[k]
	}
	if sum != RemapPool {
		t.Errorf("вложено %d очков из %d", sum, RemapPool)
	}
}

func TestImplantsRankedByPlanWeight(t *testing.T) {
	entries := []Entry{{1, 5}, {2, 5}} // pure intelligence/memory
	attrs := Alloc(Attrs{"intelligence": 10, "memory": 4})
	gains, full := Implants(entries, cat, attrs, 3)
	if gains[0].Attr != "intelligence" {
		t.Errorf("первый имплант = %s, хочу intelligence", gains[0].Attr)
	}
	if gains[len(gains)-1].Saved != 0 {
		t.Errorf("имплант в неиспользуемый атрибут экономит %s, хочу 0", gains[len(gains)-1].Saved)
	}
	if full.Saved <= gains[0].Saved {
		t.Errorf("полный сет (%s) должен быть лучше одного импланта (%s)", full.Saved, gains[0].Saved)
	}
}

func TestAcceleratorDurationFollowsBiology(t *testing.T) {
	entries := []Entry{{1, 5}, {2, 5}, {3, 5}}
	attrs := Alloc(Attrs{"intelligence": 10, "memory": 4})
	b := Accelerator(entries, cat, attrs, 12, 12, 3)
	if b.Days < 19.19 || b.Days > 19.21 {
		t.Errorf("длительность = %.2f сут, хочу 19.2 (12 × 1.6)", b.Days)
	}
	// +12 to both attributes lifts 37.5 SP/min to 55.5 — the window must
	// yield the difference
	want := int64((55.5 - 37.5) * 19.2 * 24 * 60)
	if diff := b.SPGain - want; diff > 1000 || diff < -1000 {
		t.Errorf("прирост %d SP, хочу ≈%d", b.SPGain, want)
	}
	if b.Saved <= 0 {
		t.Error("ускоритель должен экономить время")
	}
}

func TestMeanDoneRewardsShortestFirst(t *testing.T) {
	// same set, two orders: the total is identical, the average moment a
	// skill becomes usable is not
	long := []Entry{{1, 5}, {4, 1}, {4, 2}}
	short := []Entry{{4, 1}, {4, 2}, {1, 5}}
	attrs := Alloc(Attrs{"intelligence": 10, "memory": 4})
	if a, b := Duration(long, cat, attrs), Duration(short, cat, attrs); a != b {
		t.Errorf("суммарное время зависит от порядка: %s против %s", a, b)
	}
	if MeanDone(short, cat, attrs) >= MeanDone(long, cat, attrs) {
		t.Errorf("короткие вперёд должны снижать среднюю готовность: %s против %s",
			MeanDone(short, cat, attrs), MeanDone(long, cat, attrs))
	}
}

func TestBuildRunningTotals(t *testing.T) {
	entries := []Entry{{4, 1}, {4, 2}}
	p := Build(entries, cat, Alloc(Attrs{"memory": 10, "perception": 4}), time.Now())
	if p.TotalSP != 1414 {
		t.Errorf("SP плана = %d, хочу 1414", p.TotalSP)
	}
	if p.Steps[1].CumSP != 1414 || p.Steps[0].CumSP != 250 {
		t.Errorf("накопительные SP считаются неверно: %+v", p.Steps)
	}
	if p.Total != p.Steps[1].CumDur {
		t.Error("итоговое время не совпадает с последним шагом")
	}
}
