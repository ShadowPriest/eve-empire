package web

// Сводка империи (GET /): плитки по одной на каждую вкладку обзора,
// лента «Требует внимания» со всех вкладок сразу, хронология ближайших
// событий и сворачиваемая матрица персонажей. Цифры — те же функции,
// что рендерят сами вкладки (industryOverview, planetsOverview, …),
// вызванные разом; данные идут из кэша ESI Refresher.

import (
	"fmt"
	"net/http"
	"sort"
	"sync"
	"time"

	"eve-empire/internal/store"
)

// sumEventsMax — сколько строк хронологии показывать; остальное
// считается и упоминается числом.
const sumEventsMax = 60

// sumAlert — одна строка ленты «Требует внимания».
type sumAlert struct {
	Level   string // err — уже случилось; warn — вот-вот
	Tag     string // короткая метка проблемы: очередь, буры, топливо…
	Who     string // персонаж, аккаунт или структура
	WhoHref string
	Text    string
	Note    string // подробность серым
	Src     string // вкладка-источник
	SrcHref string
}

// sumEvent — одна строка хронологии.
type sumEvent struct {
	At      time.Time
	Kind    string // навык, работа, бур, топливо, реинфорс, аккаунт, AIR…
	Who     string
	WhoHref string
	Text    string
}

// sumChar — строка персонажа в матрице внизу сводки.
type sumChar struct {
	sideChar
	AirDays        int
	AirFinal       bool
	PlanetsHas     bool // есть хоть одна колония
	Planets        int
	PlanetsAllowed int
	PlanetStops    int // колоний, где что-то уже встало
	Ready          int // готовых к сдаче работ
	Mining30       float64
}

// sumGroup — аккаунт в матрице: заголовок с омегой и его персонажи.
type sumGroup struct {
	Name  string
	Omega omegaView
	Chars []sumChar
}

// sumMining — добыча за последние 30 дней по сохранённому леджеру.
type sumMining struct {
	ISK    float64
	Chars  int
	ByChar map[int64]float64
}

// sumAccounting — состояние реестра учёта.
type sumAccounting struct {
	Opened       bool
	Att          store.Attention
	ReconChecked int
	ReconDiffs   int
}

var romanLevels = [...]string{"", "I", "II", "III", "IV", "V"}

func romanLvl(n int) string {
	if n >= 0 && n < len(romanLevels) {
		return romanLevels[n]
	}
	return fmt.Sprint(n)
}

// handleIndex renders the empire summary (or the welcome page while
// no characters are added yet).
func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	ec, stale := s.esiFor(r)
	data, _, err := s.shell(r, ec, 0, "")
	if err != nil {
		httpError(w, "loading characters", err)
		return
	}
	chars := empireChars(data)
	if len(chars) == 0 {
		s.render(w, "welcome", data, stale)
		return
	}
	user := userFrom(r)
	groups, _ := data["Groups"].([]accountGroup)
	corps, _ := data["Corporations"].([]corpEntry)
	now := time.Now()

	// ── всё разом: каждая вкладка своей горутиной ──
	var (
		errs     errList
		corpRows []corpWalletRow
		ind      industryOverview
		pl       planetsOverview
		str      structuresOverview
		air      airOverview
		airDiag  store.AirWalletDiag
		mine     sumMining
		acc      sumAccounting
		wg       sync.WaitGroup
	)
	wg.Add(7)
	// Сводка тянет всё сразу, но каждый блок — только по тем
	// персонажам, у кого есть нужное право (scopes.go): узкий токен
	// молча пропускается, а не превращается в 401 в списке ошибок.
	go func() { defer wg.Done(); corpRows = s.corpWallets(ec, corps, false) }()
	go func() { defer wg.Done(); ind = s.industryOverview(ec, charsWith(chars, scopeJobs), now) }()
	go func() { defer wg.Done(); pl = s.planetsOverview(ec, charsWith(chars, scopePlanets), now, &errs) }()
	go func() {
		defer wg.Done()
		str = s.structuresOverview(ec, charsWith(chars, scopeCorpStruct), corps, now, &errs)
	}()
	go func() {
		defer wg.Done()
		var err error
		if air, err = s.airOverview(user.ID, charsWith(chars, scopeWallet), now.UTC(), nil); err != nil {
			errs.add("AIR", err)
		}
		airDiag = s.Store.AirWalletDiag(user.ID, now.UTC())
	}()
	go func() {
		defer wg.Done()
		mine.ByChar = map[int64]float64{}
		rows, err := s.Store.MiningRows(now.AddDate(0, 0, -30))
		if err != nil {
			errs.add("добыча", err)
			return
		}
		for _, row := range rows {
			v := float64(row.Quantity) * row.Price
			mine.ISK += v
			mine.ByChar[row.CharacterID] += v
		}
		mine.Chars = len(mine.ByChar)
	}()
	go func() {
		defer wg.Done()
		acc.Opened = !s.Store.LedgerEmpty()
		if !acc.Opened {
			return
		}
		var err error
		if acc.Att, err = s.Store.Attention(); err != nil {
			errs.add("учёт", err)
			return
		}
		sum, err := s.Store.Reconcile()
		if err != nil {
			errs.add("сверка", err)
			return
		}
		acc.ReconChecked, acc.ReconDiffs = sum.Checked, len(sum.Lines)
	}()
	wg.Wait()

	// ── кошельки и обучение — уже посчитаны проходом сайдбара ──
	var charTotal, corpTotal float64
	for _, ch := range chars {
		charTotal += ch.Wallet
	}
	for _, row := range corpRows {
		corpTotal += row.Total
	}
	idle, paused := 0, 0
	for _, ch := range chars {
		switch {
		case ch.QueuePaused:
			paused++
		case ch.QueueSkillID == 0:
			idle++
		}
	}

	// ── производство: сводные числа по всем типам линий ──
	indBusy := ind.Lines.MfgBusy + ind.Lines.SciBusy + ind.Lines.ReaBusy
	indTotal := ind.Lines.MfgTotal + ind.Lines.SciTotal + ind.Lines.ReaTotal
	readyBy, pausedBy := map[int64]int{}, map[int64]int{}
	indReady, indPaused := 0, 0
	for _, j := range ind.Jobs {
		switch {
		case j.Ready:
			readyBy[j.CharID]++
			indReady++
		case j.Status == "paused":
			pausedBy[j.CharID]++
			indPaused++
		}
	}

	alerts := s.summaryAlerts(chars, groups, ind, pl, str, acc, airDiag, readyBy, pausedBy, now)
	events, eventsMore := summaryEvents(chars, groups, ind, pl, str, air, now)

	// ── матрица персонажей по аккаунтам ──
	airBy := map[int64]airCharRow{}
	for _, row := range air.Rows {
		airBy[row.ID] = row
	}
	plBy := map[int64]charPlanets{}
	for _, cp := range pl.Chars {
		plBy[cp.ID] = cp
	}
	stopsBy := map[int64]int{}
	for _, a := range pl.Alerts {
		if a.Level == "err" {
			stopsBy[a.CharID]++
		}
	}
	var sumGroups []sumGroup
	for _, g := range groups {
		sg := sumGroup{Name: g.Name, Omega: g.Omega}
		for _, ch := range g.Chars {
			sc := sumChar{sideChar: ch, Ready: readyBy[ch.ID], Mining30: mine.ByChar[ch.ID]}
			if a, ok := airBy[ch.ID]; ok {
				sc.AirDays, sc.AirFinal = a.Days, a.Days >= store.AirFinalDay
			}
			if cp, ok := plBy[ch.ID]; ok {
				sc.PlanetsHas, sc.Planets, sc.PlanetsAllowed = true, cp.Used, cp.Allowed
				sc.PlanetStops = stopsBy[ch.ID]
			}
			sg.Chars = append(sg.Chars, sc)
		}
		sumGroups = append(sumGroups, sg)
	}

	data["Chars"] = chars
	data["SumGroups"] = sumGroups
	data["CharTotal"] = charTotal
	data["CorpTotal"] = corpTotal
	data["GrandTotal"] = charTotal + corpTotal
	data["Learning"] = len(chars) - idle - paused
	data["Idle"] = idle
	data["Paused"] = paused
	data["Air"] = air
	data["Ind"] = ind
	data["IndBusy"] = indBusy
	data["IndTotal"] = indTotal
	data["IndReady"] = indReady
	data["IndPaused"] = indPaused
	data["Pl"] = pl.Totals
	data["Str"] = str.Totals
	data["Mine"] = mine
	data["Acc"] = acc
	data["Alerts"] = alerts
	data["AlertsErr"], data["AlertsWarn"] = countLevels(alerts)
	data["Events"] = events
	data["EventsMore"] = eventsMore
	data["Now"] = now
	data["Errors"] = errs.list
	s.render(w, "empire", data, stale)
}

func countLevels(alerts []sumAlert) (errN, warnN int) {
	for _, a := range alerts {
		if a.Level == "err" {
			errN++
		} else {
			warnN++
		}
	}
	return
}

// summaryAlerts folds every tab's "something is wrong" into one list:
// what has already stopped first, then what is about to.
func (s *Server) summaryAlerts(chars []sideChar, groups []accountGroup, ind industryOverview,
	pl planetsOverview, str structuresOverview, acc sumAccounting, airDiag store.AirWalletDiag,
	readyBy, pausedBy map[int64]int, now time.Time) []sumAlert {

	var out []sumAlert
	add := func(a sumAlert) { out = append(out, a) }
	charHref := func(id int64, section string) string {
		return fmt.Sprintf("/characters/%d/%s", id, section)
	}

	// обучение — по данным сайдбара
	for _, ch := range chars {
		a := sumAlert{Who: ch.Name, WhoHref: charHref(ch.ID, "skills"), Tag: "очередь",
			Src: "обучение", SrcHref: "/training"}
		switch {
		case ch.QueuePaused:
			a.Level, a.Text = "warn", "очередь на паузе"
		case ch.QueueSkillID == 0:
			a.Level, a.Text = "err", "очередь пуста"
		case !ch.QueuePlanEnds.IsZero() && ch.QueuePlanEnds.Sub(now) < 48*time.Hour:
			a.Level, a.Text = "warn", "очередь закончится через "+humanUntil(ch.QueuePlanEnds, now)
			a.Note = fmt.Sprintf("осталось навыков: %d", ch.QueueLen)
		default:
			continue
		}
		add(a)
	}

	// омега и учебные места — введённые руками даты
	for _, g := range groups {
		if g.Name == "" || !g.Omega.Any {
			continue
		}
		for _, sl := range []struct {
			name string
			slot omegaSlot
		}{{"Омега", g.Omega.Omega}, {"Уч. место 1", g.Omega.MCT1}, {"Уч. место 2", g.Omega.MCT2}} {
			if sl.slot.Raw == "" || sl.slot.Class == "" {
				continue
			}
			a := sumAlert{Level: sl.slot.Class, Tag: "омега", Who: g.Name, WhoHref: "/settings",
				Src: "настройки", SrcHref: "/settings", Note: sl.slot.Disp}
			if sl.slot.Expired {
				a.Text = sl.name + " истекла"
			} else {
				a.Text = fmt.Sprintf("%s истекает через %dд", sl.name, sl.slot.Days)
			}
			add(a)
		}
	}

	// производство: готовые работы ждут сдачи, паузы — офлайн структуры
	for _, ch := range chars {
		if n := readyBy[ch.ID]; n > 0 {
			add(sumAlert{Level: "warn", Tag: "готово", Who: ch.Name, WhoHref: charHref(ch.ID, "industry"),
				Text: fmt.Sprintf("работ готово: %d — забрать", n), Src: "производство", SrcHref: "/industry"})
		}
		if n := pausedBy[ch.ID]; n > 0 {
			add(sumAlert{Level: "warn", Tag: "пауза", Who: ch.Name, WhoHref: charHref(ch.ID, "industry"),
				Text: fmt.Sprintf("работ на паузе: %d", n), Note: "структура офлайн",
				Src: "производство", SrcHref: "/industry"})
		}
	}

	// планеты — готовый список колоний с проблемами
	for _, ca := range pl.Alerts {
		a := sumAlert{Level: ca.Level, Tag: "планета", Who: ca.Char, WhoHref: charHref(ca.CharID, "planets"),
			Src: "планеты", SrcHref: "/planets"}
		// two dead extractors on one colony say the same thing twice:
		// fold repeats into "×2", notes stay one per part
		var whats []string
		times := map[string]int{}
		for _, p := range ca.Parts {
			if times[p.What] == 0 {
				whats = append(whats, p.What)
			}
			times[p.What]++
			if p.Note != "" {
				if a.Note != "" {
					a.Note += " · "
				}
				a.Note += p.Note
			}
		}
		for i, w := range whats {
			if i > 0 {
				a.Text += " и "
			}
			a.Text += w
			if times[w] > 1 {
				a.Text += fmt.Sprintf(" ×%d", times[w])
			}
		}
		a.Text = ca.Planet + ": " + a.Text
		add(a)
	}

	// структуры: топливо, реинфорс, снятие с якоря, атаки за неделю
	for _, cv := range str.Corps {
		for _, st := range cv.Structures {
			base := sumAlert{Who: st.Name, WhoHref: "/structures", Src: "структуры", SrcHref: "/structures",
				Note: st.System}
			switch {
			case st.LowPower:
				a := base
				a.Level, a.Tag, a.Text = "err", "топливо", "без топлива (low power)"
				add(a)
			case st.FuelSev == "sev-err":
				a := base
				a.Level, a.Tag, a.Text = "err", "топливо", "топливо кончится через "+st.FuelLeft
				add(a)
			case st.FuelSev == "sev-warn":
				a := base
				a.Level, a.Tag, a.Text = "warn", "топливо", "топлива на "+st.FuelLeft
				add(a)
			}
			if st.StateSev == "sev-err" {
				a := base
				a.Level, a.Tag, a.Text = "err", "реинфорс", st.State
				if st.StateEnd != nil {
					a.Text += " · до " + st.StateEnd.Format("02.01 15:04")
				}
				add(a)
			}
			if st.Unanchors != nil {
				a := base
				a.Level, a.Tag, a.Text = "warn", "якорь", "снятие с якоря "+st.Unanchors.Format("02.01 15:04")
				add(a)
			}
		}
	}
	weekAgo := now.Add(-7 * 24 * time.Hour)
	for _, ev := range str.Events {
		if ev.Sev != "sev-err" || ev.Time.Before(weekAgo) {
			continue
		}
		add(sumAlert{Level: "err", Tag: "атака", Who: ev.Structure, WhoHref: "/structures",
			Text: ev.Label + " · " + ev.Time.Format("02.01 15:04"), Note: ev.Details,
			Src: "структуры", SrcHref: "/structures"})
	}

	// учёт: сверка реестра с имуществом
	if acc.Opened && acc.ReconDiffs > 0 {
		add(sumAlert{Level: "warn", Tag: "сверка", Who: "Реестр", WhoHref: "/accounting",
			Text: fmt.Sprintf("расходится позиций: %d из %d", acc.ReconDiffs, acc.ReconChecked),
			Src:  "учёт", SrcHref: "/accounting"})
	}

	// AIR: сырьё для счётчика дней не поступает
	if airDiag.InWindow == 0 {
		a := sumAlert{Level: "warn", Tag: "AIR", Who: "Сбор дней", WhoHref: "/air", Src: "AIR", SrcHref: "/air"}
		switch {
		case airDiag.Wallet.LastTry.IsZero():
			a.Text = "сбор валетов ещё не запускался"
			a.Note = "коллектор выключен?"
		default:
			a.Text = "записей daily goals в окне месяца нет"
			a.Note = "сбор не работает, токены мертвы или награды не забирались"
		}
		add(a)
	}

	// то, что уже случилось, — выше того, что только грозит
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].Level == "err" && out[j].Level != "err"
	})
	return out
}

// summaryEvents merges every dated thing the tabs know about into one
// timeline, nearest first. Returns the shown rows and how many more
// there were past the cap.
func summaryEvents(chars []sideChar, groups []accountGroup, ind industryOverview,
	pl planetsOverview, str structuresOverview, air airOverview, now time.Time) ([]sumEvent, int) {

	var out []sumEvent
	add := func(at time.Time, kind, who, whoHref, text string) {
		if at.IsZero() || !at.After(now) {
			return
		}
		out = append(out, sumEvent{At: at, Kind: kind, Who: who, WhoHref: whoHref, Text: text})
	}
	charHref := func(id int64, section string) string {
		return fmt.Sprintf("/characters/%d/%s", id, section)
	}

	for _, ch := range chars {
		if ch.QueueSkillID == 0 {
			continue
		}
		add(ch.QueueEnds, "навык", ch.Name, charHref(ch.ID, "skills"),
			ch.QueueSkill+" "+romanLvl(ch.QueueLevel))
		if ch.QueueLen > 1 && ch.QueuePlanEnds.After(ch.QueueEnds) {
			add(ch.QueuePlanEnds, "очередь", ch.Name, charHref(ch.ID, "skills"),
				fmt.Sprintf("конец плана обучения · навыков в нём: %d", ch.QueueLen))
		}
	}

	for _, j := range ind.Jobs {
		if j.Ready || j.Status == "paused" {
			continue
		}
		text := fmt.Sprintf("%s ×%d · %s", j.BlueprintName, j.Runs, j.Activity)
		if j.IsCorp {
			text += " · корп"
		}
		add(j.EndDate, "работа", j.Char, charHref(j.CharID, "industry"), text)
	}

	for _, cp := range pl.Chars {
		for _, sl := range cp.Slots {
			if sl.Locked || sl.Empty {
				continue
			}
			for _, e := range sl.Extractors {
				add(e.Expiry, "бур", cp.Name, charHref(cp.ID, "planets"),
					sl.PlanetName+": программа бура · "+e.Product)
			}
		}
	}

	for _, cv := range str.Corps {
		for _, st := range cv.Structures {
			if st.Fuel != nil && !st.LowPower {
				add(*st.Fuel, "топливо", st.Name, "/structures", "топливо кончится · "+st.System)
			}
			if st.StateEnd != nil {
				add(*st.StateEnd, "реинфорс", st.Name, "/structures", "выход из состояния: "+st.State)
			}
			if st.Unanchors != nil {
				add(*st.Unanchors, "якорь", st.Name, "/structures", "снятие с якоря · "+st.System)
			}
		}
	}

	for _, g := range groups {
		if g.Name == "" || !g.Omega.Any {
			continue
		}
		for _, sl := range []struct {
			name string
			slot omegaSlot
		}{{"Омега", g.Omega.Omega}, {"Уч. место 1", g.Omega.MCT1}, {"Уч. место 2", g.Omega.MCT2}} {
			if sl.slot.Raw == "" || sl.slot.Expired {
				continue
			}
			add(sl.slot.Deadline, "аккаунт", g.Name, "/settings", sl.name+" истекает")
		}
	}

	add(air.ResetAt, "AIR", "", "", "новый месяц AIR · счётчики дней обнулятся")

	sort.SliceStable(out, func(i, j int) bool { return out[i].At.Before(out[j].At) })
	more := 0
	if len(out) > sumEventsMax {
		more = len(out) - sumEventsMax
		out = out[:sumEventsMax]
	}
	return out, more
}
