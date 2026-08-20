package web

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"eve-empire/internal/esi"
	"eve-empire/internal/sde"
	"eve-empire/internal/skillplan"
	"eve-empire/internal/store"
)

// ── skill planner ────────────────────────────────────────────────────
//
// ESI cannot write the skill queue (the only writable routes are
// contacts, mail, fleets, fittings and asset names), so this page is a
// calculator plus a clipboard: it imports a plan in the game's format,
// costs it against a character's real attributes, reorders it without
// breaking prerequisites and hands it back for pasting into the client.
//
// The maths lives in internal/skillplan; here we only marshal it.

type planStepView struct {
	N     int
	Name  string
	Level int
	Rank  int
	Pair  string
	SP    int64
	Rate  string
	Dur   string
	CumSP int64
	Done  string
	Slow  bool // trains below the plan's best rate
	Over  bool // past the SP ceiling
	Cross bool // the step that crosses it
}

type pairView struct {
	Pair   string
	SP     int64
	Share  float64
	Rate   string
	Dur    string
	Lost   string
	HasLost bool
	Skills string
}

type remapView struct {
	Label   string
	Attrs   []int
	Dur     string
	Saved   string
	Best    bool
	Current bool
}

type implantGainView struct {
	Attr  string
	Name  string
	Slot  int
	Saved string
	Days  float64
	Pct   float64
}

// implantNames maps an attribute to the implant family that boosts it.
// Grade is in the suffix: Limited (+1), Limited … Beta (+2), Basic (+3),
// Standard (+4), Improved (+5) — the +3 family needs only Cybernetics 1.
var implantNames = map[string]struct {
	Family string
	Slot   int
}{
	"perception":   {"Ocular Filter", 1},
	"memory":       {"Memory Augmentation", 2},
	"willpower":    {"Neural Boost", 3},
	"intelligence": {"Cybernetic Subprocessor", 4},
	"charisma":     {"Social Adaptation Chip", 5},
}

var gradeSuffix = map[int]string{3: "Basic", 4: "Standard", 5: "Improved"}

var errEmptyQueue = errors.New("очередь пуста — персонаж ничего не учит")

// skillCatalog adapts the SDE catalog to the planner's shape.
func (s *Server) skillCatalog() map[int64]skillplan.Skill {
	out := map[int64]skillplan.Skill{}
	for id, sk := range s.SDE.Skills() {
		out[id] = skillplan.Skill{
			ID: id, Name: sk.Name(), En: sk.NameEn, Rank: sk.Rank,
			Prim: sk.Prim, Sec: sk.Sec, Pre: sk.Pre,
		}
	}
	return out
}

func (s *Server) handleSkillPlanner(w http.ResponseWriter, r *http.Request) {
	ec, stale := s.esiFor(r)
	data, _, err := s.shell(ec, 0, "")
	if err != nil {
		httpError(w, "loading characters", err)
		return
	}
	var errs errList

	// GOTCHA: the whole plan arrives in the POST body, and the stale
	// revalidation in layout.html re-fetches location.href with GET and
	// swaps #main with the answer. On a POST result that GET carries no
	// form, so the freshly rendered plan would be replaced by the empty
	// page a moment later. Never advertise staleness on a POST render.
	if r.Method == http.MethodPost {
		r.ParseForm()
		stale = nil
	}
	field := func(k string) string {
		if r.Method == http.MethodPost {
			return strings.TrimSpace(r.FormValue(k))
		}
		return strings.TrimSpace(r.URL.Query().Get(k))
	}

	plans, err := s.Store.SkillPlans()
	if err != nil {
		errs.add("сохранённые планы", err)
	}
	data["Plans"] = plans
	data["Chars"] = empireChars(data)

	text := field("plan")
	source := field("src") // "" | queue — where the plan comes from
	planID, _ := strconv.ParseInt(field("id"), 10, 64)
	if text == "" && planID > 0 {
		for _, p := range plans {
			if p.ID == planID {
				text, data["PlanName"] = p.Body, p.Name
			}
		}
	}
	if n := field("name"); n != "" {
		data["PlanName"] = n
	}
	charID, _ := strconv.ParseInt(field("char"), 10, 64)
	mode := field("mode")
	switch mode {
	case skillplan.OrderKeep, skillplan.OrderCheap, skillplan.OrderFast, skillplan.OrderLong:
	default:
		mode = skillplan.OrderCheap
	}
	grade, _ := strconv.Atoi(field("grade"))
	if grade < 3 || grade > 5 {
		grade = 3
	}
	cap64, _ := strconv.ParseInt(strings.ReplaceAll(field("cap"), " ", ""), 10, 64)
	useRemap := field("remap") != "0"

	data["Source"] = source
	data["CharID"] = charID
	data["Mode"] = mode
	data["Grade"] = grade
	data["Cap"] = cap64
	data["UseRemap"] = useRemap
	data["PlanID"] = planID
	data["Modes"] = []struct{ Key, Label, Note string }{
		{skillplan.OrderCheap, "Дешёвые вперёд", "больше готовых навыков на каждый вложенный SP"},
		{skillplan.OrderFast, "Быстрые вперёд", "то же, но по времени: учитывает характеристики"},
		{skillplan.OrderKeep, "Как есть", "только чинит нарушенные пререквизиты"},
		{skillplan.OrderLong, "Длинные вперёд", "сначала крупные уровни"},
	}

	if !s.SDE.Available() {
		data["NoSDE"] = true
		data["Errors"] = errs.list
		s.render(w, "skill_planner", data, stale)
		return
	}
	cat := s.skillCatalog()

	// ── character context: what is already trained, and at what speed ──
	trained := map[int64]int{}
	attrs := skillplan.Alloc(skillplan.Attrs{}) // plain 17s when no character
	implantBonus := skillplan.Attrs{}
	var (
		baseSP  int64
		unalloc int64
		queue   []esi.QueueEntry
	)
	if charID > 0 {
		var (
			wg       sync.WaitGroup
			sheet    *esi.SkillSheet
			chAttrs  *esi.Attributes
			implants []sde.Implant
		)
		wg.Add(3)
		go func() {
			defer wg.Done()
			var err error
			if sheet, err = ec.Skills(charID); err != nil {
				errs.add("навыки персонажа", err)
			}
		}()
		go func() {
			defer wg.Done()
			chAttrs, implants, _, _, _ = s.cloneData(ec, charID, &errs)
		}()
		go func() {
			defer wg.Done()
			var err error
			if queue, err = ec.SkillQueue(charID); err != nil {
				errs.add("очередь навыков", err)
			}
		}()
		wg.Wait()

		if sheet != nil {
			baseSP, unalloc = sheet.TotalSP, sheet.UnallocatedSP
			for _, sk := range sheet.Skills {
				trained[sk.SkillID] = sk.TrainedLevel
			}
		}
		for _, im := range implants {
			for k, v := range im.Bonuses {
				implantBonus[k] += v
			}
		}
		if chAttrs != nil {
			// ESI reports attributes with implants already folded in,
			// same assumption as the clones page.
			attrs = skillplan.Attrs{
				"intelligence": chAttrs.Intelligence,
				"memory":       chAttrs.Memory,
				"perception":   chAttrs.Perception,
				"willpower":    chAttrs.Willpower,
				"charisma":     chAttrs.Charisma,
			}
			data["RemapsLeft"] = chAttrs.BonusRemaps
		}
	}
	data["BaseSP"] = baseSP
	data["Unalloc"] = unalloc
	data["QueueLen"] = len(queue)

	// ── where the plan comes from ──
	// "queue" pulls the character's live skill queue out of ESI, so no
	// copying by hand: the queue IS a plan, just one the game already
	// accepted. Entries already finished are dropped.
	var entries []skillplan.Entry
	var unknown []string
	now := time.Now()
	if source == "queue" && charID > 0 {
		var raw []skillplan.Entry
		for _, q := range queue {
			if !q.FinishDate.IsZero() && q.FinishDate.Before(now) {
				continue
			}
			if _, ok := cat[q.SkillID]; !ok {
				continue
			}
			raw = append(raw, skillplan.Entry{SkillID: q.SkillID, Level: q.FinishedLevel})
		}
		entries = raw
		text = skillplan.Format(raw, cat) // fills the textarea, so it can be saved
		if len(raw) == 0 {
			errs.add("очередь навыков", errEmptyQueue)
		}
	} else {
		entries, unknown = skillplan.Parse(text, func(n string) (skillplan.Skill, bool) {
			sk, ok := s.SDE.SkillByName(n)
			if !ok {
				return skillplan.Skill{}, false
			}
			return cat[sk.ID], true
		})
	}
	data["Unknown"] = unknown
	data["PlanText"] = text
	if len(entries) == 0 {
		data["Errors"] = errs.list
		s.render(w, "skill_planner", data, stale)
		return
	}

	ordered, stuck := skillplan.Order(entries, trained, cat, mode, attrs)
	if len(stuck) > 0 {
		var names []string
		for _, e := range stuck {
			names = append(names, cat[e.SkillID].Name+" "+strconv.Itoa(e.Level))
		}
		data["Stuck"] = names
	}

	// ── the remap, and the attributes the plan is actually costed with ──
	best := skillplan.Remap(ordered, cat, implantBonus, 6)
	costAttrs := attrs
	if useRemap && len(best) > 0 {
		costAttrs = best[0].Attrs
	}

	plan := skillplan.Build(ordered, cat, costAttrs, time.Now())

	// best rate in the plan — the yardstick for "slow" rows
	var bestRate float64
	for _, st := range plan.Steps {
		if st.Rate > bestRate {
			bestRate = st.Rate
		}
	}
	steps := make([]planStepView, 0, len(plan.Steps))
	for i, st := range plan.Steps {
		v := planStepView{
			N: i + 1, Name: st.Name, Level: st.Level, Rank: st.Rank,
			Pair:  skillplan.AttrRu[st.Prim] + " / " + skillplan.AttrRu[st.Sec],
			SP:    st.SP,
			Rate:  strconv.FormatFloat(st.Rate, 'f', 1, 64),
			Dur:   humanDur(st.Dur),
			CumSP: st.CumSP,
			Done:  st.Done.Format("02.01.2006"),
			Slow:  st.Rate < bestRate-0.01,
		}
		if cap64 > 0 {
			total := baseSP + st.CumSP
			v.Over = total > cap64
			v.Cross = v.Over && (i == 0 || baseSP+plan.Steps[i-1].CumSP <= cap64)
		}
		steps = append(steps, v)
	}
	data["Steps"] = steps
	data["TotalSP"] = plan.TotalSP
	data["TotalDur"] = humanDur(plan.Total)
	data["Ends"] = plan.Ends
	data["Lines"] = len(plan.Steps)
	if cap64 > 0 {
		data["CapLeft"] = cap64 - baseSP - plan.TotalSP
	}

	// ── where the plan loses time ──
	var pairs []pairView
	for _, p := range skillplan.Pairs(ordered, cat, costAttrs) {
		pairs = append(pairs, pairView{
			Pair:  skillplan.AttrRu[p.Prim] + " / " + skillplan.AttrRu[p.Sec],
			SP:    p.SP,
			Share: p.Share,
			Rate:  strconv.FormatFloat(p.Rate, 'f', 1, 64),
			Dur:   humanDur(p.Dur),
			Lost:  humanDur(p.Lost),
			HasLost: p.Lost > time.Hour,
			Skills: strings.Join(p.Skills, ", "),
		})
	}
	data["Pairs"] = pairs

	// ── remap table ──
	var remaps []remapView
	cur := skillplan.Duration(ordered, cat, attrs)
	for i, opt := range best {
		v := remapView{
			Dur:  humanDur(opt.Dur),
			Best: i == 0,
		}
		for _, k := range skillplan.AttrKeys {
			v.Attrs = append(v.Attrs, opt.Attrs[k])
		}
		if opt.Dur < cur {
			v.Saved = humanDur(cur - opt.Dur)
		}
		remaps = append(remaps, v)
	}
	data["Remaps"] = remaps
	data["CurDur"] = humanDur(cur)
	var curAttrs []int
	for _, k := range skillplan.AttrKeys {
		curAttrs = append(curAttrs, attrs[k])
	}
	data["CurAttrs"] = curAttrs
	data["AttrLabels"] = func() []string {
		var out []string
		for _, k := range skillplan.AttrKeys {
			out = append(out, skillplan.AttrRu[k])
		}
		return out
	}()

	// ── implants, ranked by what they save on THIS plan ──
	gains, full := skillplan.Implants(ordered, cat, costAttrs, grade)
	var imps []implantGainView
	for _, g := range gains {
		fam := implantNames[g.Attr]
		days := g.Saved.Hours() / 24
		v := implantGainView{
			Attr: skillplan.AttrRu[g.Attr],
			Name: fam.Family + " - " + gradeSuffix[grade],
			Slot: fam.Slot,
			Days: days,
		}
		v.Saved = humanDur(g.Saved)
		if full.Saved > 0 {
			v.Pct = g.Saved.Seconds() / full.Saved.Seconds() * 100
		}
		imps = append(imps, v)
	}
	data["Implants"] = imps
	data["ImplantFull"] = humanDur(full.Saved)

	// ── cerebral accelerator ──
	bio := trained[3405] // Biology stretches a booster by 20% per level
	boost := skillplan.Accelerator(ordered, cat, costAttrs, 12, 12, bio)
	data["Booster"] = map[string]any{
		"Bonus": 12, "Base": 12, "Biology": bio,
		"Days": strconv.FormatFloat(boost.Days, 'f', 1, 64),
		"SP":   boost.SPGain, "Saved": humanDur(boost.Saved),
	}

	// ── советы ───────────────────────────────────────────────────────
	// Everything below is already computed; this only turns the numbers
	// into "do this" lines, ranked by how much time each one is worth.
	type advice struct {
		Sev  string // "" | warn | err
		Text string
	}
	var tips []advice
	add := func(sev, text string) { tips = append(tips, advice{sev, text}) }

	if len(best) > 0 && cur-best[0].Dur > 24*time.Hour {
		var pairsTxt []string
		for _, k := range skillplan.AttrKeys {
			if v := best[0].Attrs[k] - implantBonus[k]; v != skillplan.AttrBase {
				pairsTxt = append(pairsTxt, skillplan.AttrRu[k]+" "+strconv.Itoa(v))
			}
		}
		txt := "Ремап " + strings.Join(pairsTxt, " / ") + " — минус " + humanDur(cur-best[0].Dur) +
			" к сроку плана."
		if n, ok := data["RemapsLeft"].(int); ok {
			if n > 0 {
				txt += " Доступно перераспределений: " + strconv.Itoa(n) + "."
			} else {
				txt += " Бесплатных перераспределений сейчас нет."
			}
		}
		add("", txt)
	}
	if len(imps) > 0 && gains[0].Saved > 6*time.Hour {
		txt := "Имплант «" + imps[0].Name + "» (слот " + strconv.Itoa(imps[0].Slot) + ") экономит " +
			imps[0].Saved + ", это " + strconv.FormatFloat(imps[0].Pct, 'f', 0, 64) + " % выгоды полного сета."
		need := map[int]int{3: 1, 4: 4, 5: 5}[grade]
		if have := trained[3411]; charID > 0 && have < need { // 3411 = Cybernetics
			txt += " Нужна Кибернетика " + strconv.Itoa(need) + ", у персонажа " + strconv.Itoa(have) + "."
		}
		add("", txt)
	}
	// The slowest bucket is not a mistake by itself — the remap above
	// already traded it off against the rest. It is worth naming only so
	// the owner can decide whether those skills are worth their price.
	for _, p := range pairs {
		if p.HasLost && p.Share > 5 {
			add("warn", "Самый медленный крупный кусок — пара «"+p.Pair+"»: "+
				strconv.FormatFloat(p.Share, 'f', 0, 64)+" % плана и +"+p.Lost+
				" против самой быстрой пары этого плана. Если эти навыки не нужны, срок режется здесь. Навыки: "+
				p.Skills+".")
			break
		}
	}
	if mean0, mean1 := skillplan.MeanDone(entries, cat, costAttrs),
		skillplan.MeanDone(ordered, cat, costAttrs); mean0-mean1 > 12*time.Hour {
		add("", "Порядок: суммарный срок перестановка не меняет вообще, но навыки становятся "+
			"готовы в среднем на "+humanDur(mean0-mean1)+" раньше.")
	}
	if boost.Saved > 12*time.Hour {
		add("", "Церебральный ускоритель +12 при Биологии "+strconv.Itoa(bio)+" держится "+
			strconv.FormatFloat(boost.Days, 'f', 1, 64)+" сут и сдвигает конец плана на "+
			humanDur(boost.Saved)+" — есть смысл съесть его в начале.")
	}
	if unalloc > 0 {
		add("", "У персонажа "+formatNum(unalloc)+" нераспределённых SP — их можно влить в план сразу.")
	}
	if cap64 > 0 {
		left := cap64 - baseSP - plan.TotalSP
		if left < 0 {
			add("err", "План не влезает в потолок "+formatNum(cap64)+" SP на "+formatNum(-left)+
				" SP — резать надо с конца, там самые дорогие уровни.")
		} else {
			add("", "До потолка остаётся "+formatNum(left)+" SP после всего плана.")
		}
	}
	if source == "queue" && plan.Total < 24*time.Hour {
		add("warn", "Очередь короче суток ("+humanDur(plan.Total)+") — альт скоро встанет без обучения.")
	}
	data["Tips"] = tips

	data["Export"] = skillplan.Format(ordered, cat)
	data["Errors"] = errs.list
	s.render(w, "skill_planner", data, stale)
}

func (s *Server) handleSkillPlanSave(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	name := strings.TrimSpace(r.FormValue("name"))
	if name == "" {
		name = "План " + time.Now().Format("02.01.2006 15:04")
	}
	id, _ := strconv.ParseInt(r.FormValue("id"), 10, 64)
	charID, _ := strconv.ParseInt(r.FormValue("char"), 10, 64)
	body := r.FormValue("plan")
	if strings.TrimSpace(body) == "" {
		http.Redirect(w, r, "/tools/skill-planner", http.StatusSeeOther)
		return
	}
	newID, err := s.Store.SaveSkillPlan(store.SkillPlan{
		ID: id, Name: name, CharacterID: charID, Body: body,
	})
	if err != nil {
		httpError(w, "saving plan", err)
		return
	}
	http.Redirect(w, r, "/tools/skill-planner?id="+strconv.FormatInt(newID, 10)+
		"&char="+strconv.FormatInt(charID, 10), http.StatusSeeOther)
}

func (s *Server) handleSkillPlanDelete(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	id, _ := strconv.ParseInt(r.FormValue("id"), 10, 64)
	if id > 0 {
		if err := s.Store.DeleteSkillPlan(id); err != nil {
			httpError(w, "deleting plan", err)
			return
		}
	}
	http.Redirect(w, r, "/tools/skill-planner", http.StatusSeeOther)
}
