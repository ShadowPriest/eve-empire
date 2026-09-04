package web

// SP-ферма: живые цены PLEX / экстрактора / инжектора, расчёт годового
// профита по модели владельца (Excel «EVE.xlsx», Лист2) и журнал закупа
// PLEX. История цен собирается из двух источников: ESI отдаёт ~13 месяцев
// дневной истории сразу, свои снапшоты (spfarm_price) живут вечно и
// подклеиваются слева, когда окно ESI уезжает.

import (
	"encoding/json"
	"fmt"
	"html/template"
	"log"
	"math"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"eve-empire/internal/esi"
	"eve-empire/internal/store"
)

// farmMinutes — минут обучения в году. В Excel было 360 дней; здесь
// честные 365, поэтому цифры чуть выше табличных.
const farmMinutes = 60 * 24 * 365

// extractorSP — сколько SP забирает один экстрактор.
const extractorSP = 500_000

// debugFarmStats включает отладочный лог сбора статистики фермы.
var debugFarmStats = os.Getenv("SPFARM_DEBUG") == "1"

// farmParams — модель фермы. Правится формой на странице, хранится в
// app_settings одной JSON-строкой.
type farmParams struct {
	Accounts    int       `json:"accounts"`     // окон — запасной вариант, пока ростер пуст
	LineRates   []float64 `json:"line_rates"`   // SP/мин линий — запасной вариант
	AirSP       float64   `json:"air_sp"`       // SP от AIR в год на персонажа
	UseAIR      bool      `json:"use_air"`      // учитывать ли AIR в прогнозе
	PackagePLEX float64   `json:"package_plex"` // PLEX/год — запасной вариант без выбранного предложения
	BoosterPLEX float64   `json:"booster_plex"` // бустеры и прочее, PLEX в год на аккаунт
	TaxPct      float64   `json:"tax_pct"`      // налог + брокерка с продажи инжекторов
	OfferID     int64     `json:"offer_id"`     // выбранное предложение магазина EVE
	PlexSide    string    `json:"plex_side"`    // sell|buy — по какой стороне стакана считать
	ExtSide     string    `json:"ext_side"`
	InjSide     string    `json:"inj_side"`
}

func defaultFarmParams() farmParams {
	return farmParams{
		Accounts:    5,
		LineRates:   []float64{45, 45},
		AirSP:       6_300_000,
		UseAIR:      true,
		PackagePLEX: 4100,
		BoosterPLEX: 2699,
		TaxPct:      0,
		PlexSide:    "sell", ExtSide: "sell", InjSide: "sell",
	}
}

// packageYear — PLEX в год на аккаунт: выбранное предложение магазина
// EVE, нормализованное к году, или запасное значение из параметров.
func (s *Server) packageYear(p farmParams) (float64, string) {
	if p.OfferID != 0 {
		if o, err := s.Store.FarmOffer(p.OfferID); err == nil {
			return o.PlexPerYear(), o.Name
		}
	}
	return p.PackagePLEX, ""
}

func (s *Server) farmParams() farmParams {
	p := defaultFarmParams()
	if raw := s.Store.Setting("spfarm_params"); raw != "" {
		_ = json.Unmarshal([]byte(raw), &p)
	}
	if p.Accounts < 1 {
		p.Accounts = 1
	}
	if len(p.LineRates) == 0 {
		p.LineRates = []float64{45, 45}
	}
	for _, side := range []*string{&p.PlexSide, &p.ExtSide, &p.InjSide} {
		if *side != "buy" {
			*side = "sell"
		}
	}
	return p
}

// RatesStr renders the line rates back into the form field.
func (p farmParams) RatesStr() string {
	parts := make([]string, len(p.LineRates))
	for i, r := range p.LineRates {
		parts[i] = trimFloat(r)
	}
	return strings.Join(parts, ", ")
}

// charAirSP — вклад AIR в годовой прогноз персонажа. Пока всегда 0:
// статическая добавка убрана по просьбе владельца, AIR станет отдельной
// сущностью (авто-сбор из валетов) — подключать её сюда, гейт уже есть
// (params.UseAIR). Параметр AirSP в JSON сохранён под неё же.
func (p farmParams) charAirSP(charID int64) float64 {
	if !p.UseAIR {
		return 0
	}
	_ = charID // будущая сущность считает AIR по конкретному персонажу
	return 0   // TODO: + SP за AIR
}

// spPerAccount — SP в год на один аккаунт из запасных параметров:
// линии крутятся весь год.
func (p farmParams) spPerAccount() float64 {
	var sp float64
	for _, r := range p.LineRates {
		sp += r * farmMinutes
	}
	return sp
}

// sidePrice picks the order-book side the params ask for.
func sidePrice(st esi.OrderStats, side string) float64 {
	if side == "buy" {
		return st.BuyMax
	}
	return st.SellMin
}

func sideLabel(side string) string {
	if side == "buy" {
		return "бай"
	}
	return "селл"
}

// farmInputs — вводные модели: суммарные SP фермы в год и число окон.
// Live-ростер (живые скорости персонажей) или запасные параметры.
type farmInputs struct {
	SPTotal    float64
	Accounts   int
	PerPlexAcc float64 // PLEX в год на аккаунт: предложение + бустеры
	TaxPct     float64
	Live       bool // собрано с ростера, а не из запасных параметров
}

// farmProfitTotal — профит всей фермы за год при заданных ценах.
func farmProfitTotal(in farmInputs, plexP, extP, injP float64) float64 {
	injectors := in.SPTotal / extractorSP
	revenue := injectors * (injP*(1-in.TaxPct/100) - extP)
	return revenue - in.PerPlexAcc*float64(in.Accounts)*plexP
}

// farmCalc — всё, что показывает карточка расчёта.
type farmCalc struct {
	SPYear      float64 // в среднем на аккаунт
	Injectors   float64 // в среднем на аккаунт в год
	PlexPrice   float64
	ExtPrice    float64
	InjPrice    float64
	InjNet      float64 // после налога
	Spread      float64 // чистый инжектор минус экстрактор
	ProfitAcc   float64
	ProfitTotal float64
	BreakEven   float64 // цена PLEX, при которой профит нулевой
	PlexYear    float64 // PLEX в год на всю ферму
	Accounts    int     // окон в расчёте (ростер или запасные параметры)
	Live        bool    // расчёт с живого ростера
	Ready       bool    // все три цены получены

	// Шкала вердикта: текущая цена PLEX и порог окупаемости внутри
	// годового диапазона, в процентах ширины.
	BarPos, TickPos float64
	HasRange        bool
}

func newFarmCalc(in farmInputs, p farmParams, plexSt, extSt, injSt esi.OrderStats) farmCalc {
	accounts := float64(in.Accounts)
	if accounts < 1 {
		accounts = 1
	}
	c := farmCalc{
		SPYear:    in.SPTotal / accounts,
		PlexPrice: sidePrice(plexSt, p.PlexSide),
		ExtPrice:  sidePrice(extSt, p.ExtSide),
		InjPrice:  sidePrice(injSt, p.InjSide),
		PlexYear:  in.PerPlexAcc * accounts,
		Accounts:  int(accounts),
		Live:      in.Live,
	}
	c.Injectors = c.SPYear / extractorSP
	c.Ready = c.PlexPrice > 0 && c.ExtPrice > 0 && c.InjPrice > 0
	if !c.Ready {
		return c
	}
	c.InjNet = c.InjPrice * (1 - in.TaxPct/100)
	c.Spread = c.InjNet - c.ExtPrice
	revenueTotal := in.SPTotal / extractorSP * c.Spread
	c.ProfitTotal = revenueTotal - c.PlexYear*c.PlexPrice
	c.ProfitAcc = c.ProfitTotal / accounts
	if c.PlexYear > 0 {
		c.BreakEven = revenueTotal / c.PlexYear
	}
	return c
}

// queueRate — текущий тренируемый навык и его скорость в SP/мин из
// живой очереди. Пустая или ставшая на паузу очередь — нули.
//
// ГРАБЛЯ: делить надо (level_end_sp − training_start_sp), а не диапазон
// всего уровня. Частично прокачанный когда-то навык дотренировывается за
// короткое окно, и деление полного уровня на это окно давало 210 СП/мин
// при физическом потолке ~63 (профильный ремап, +5 импланты, бустер).
func queueRate(queue []esi.QueueEntry, now time.Time) (skill string, rate float64) {
	for _, q := range queue {
		if q.FinishDate.IsZero() || q.FinishDate.Before(now) {
			continue
		}
		mins := q.FinishDate.Sub(q.StartDate).Minutes()
		if mins <= 0 {
			continue
		}
		return q.SkillName, float64(q.LevelEndSP-q.TrainingStartSP) / mins
	}
	return "", 0
}

// poolLineRe — строка плана из игрового буфера обмена:
// <localized hint="Cybernetics">Кибернетика*</localized> 5
var poolLineRe = regexp.MustCompile(`(?i)<localized hint="([^"]*)">([^<]*)</localized>`)

// poolSkills разбирает пул: игровой формат и простые строки-названия.
// Возвращает множество имён в нижнем регистре — у игровых строк и
// английский hint, и русское название, чтобы матчиться при любом языке
// ESI.
func poolSkills(pool string) map[string]bool {
	out := map[string]bool{}
	for _, line := range strings.Split(pool, "\n") {
		line = strings.TrimSpace(strings.TrimSuffix(line, "\r"))
		if line == "" {
			continue
		}
		if m := poolLineRe.FindStringSubmatch(line); m != nil {
			for _, name := range m[1:3] {
				name = strings.TrimSuffix(strings.TrimSpace(name), "*")
				if name != "" {
					out[strings.ToLower(name)] = true
				}
			}
			continue
		}
		// Простая строка; хвостовой уровень («Кибернетика 5» или
		// «Cybernetics V») отбрасывается как второй вариант имени.
		low := strings.ToLower(line)
		out[low] = true
		if i := strings.LastIndexByte(low, ' '); i > 0 {
			switch low[i+1:] {
			case "1", "2", "3", "4", "5", "i", "ii", "iii", "iv", "v":
				out[strings.TrimSpace(low[:i])] = true
			}
		}
	}
	return out
}

// poolCount — сколько строк-навыков в пуле (для карточки персонажа).
func poolCount(pool string) int {
	n := 0
	for _, line := range strings.Split(pool, "\n") {
		if strings.TrimSpace(strings.TrimSuffix(line, "\r")) != "" {
			n++
		}
	}
	return n
}

// poolHas сообщает, входит ли навык в пул (пустой пул — входит всё).
func poolHas(pool, skill string) bool {
	set := poolSkills(pool)
	if len(set) == 0 {
		return true
	}
	if skill == "" {
		return false
	}
	return set[strings.ToLower(strings.TrimSpace(skill))]
}

// farmCharView — один персонаж фермы на странице модели.
type farmCharView struct {
	ID        int64
	Name      string
	Selected  bool
	Pool      string
	PoolCount int     // строк-навыков в пуле
	Skill     string  // тренируется сейчас
	Rate      float64 // SP/мин
	InPool    bool
	Paused    bool
	AirSP     float64 // вклад AIR (пока 0 — заготовка под сущность AIR)
	SPYear    float64 // прогноз: тренировка + AirSP
}

// farmInputsFromRoster собирает вводные с живого ростера. Возвращает
// ok=false, когда ростер пуст — тогда работают запасные параметры.
func (s *Server) farmInputsFromRoster(ec *esi.Client, p farmParams, packageYear float64) (farmInputs, bool) {
	farmChars, err := s.Store.FarmChars()
	if err != nil || len(farmChars) == 0 {
		return farmInputs{}, false
	}
	farmAccts, _ := s.Store.FarmAccounts()

	type res struct {
		id    int64
		skill string
		rate  float64
	}
	results := make([]res, 0, len(farmChars))
	var mu sync.Mutex
	var wg sync.WaitGroup
	now := time.Now()
	for id := range farmChars {
		wg.Add(1)
		go func(id int64) {
			defer wg.Done()
			queue, _ := ec.SkillQueue(id)
			skill, rate := queueRate(queue, now)
			mu.Lock()
			results = append(results, res{id, skill, rate})
			mu.Unlock()
		}(id)
	}
	wg.Wait()

	in := farmInputs{
		Accounts:   len(farmAccts),
		PerPlexAcc: packageYear + p.BoosterPLEX,
		TaxPct:     p.TaxPct,
		Live:       true,
	}
	for _, r := range results {
		sp := p.charAirSP(r.id)
		if r.rate > 0 && poolHas(farmChars[r.id], r.skill) {
			sp += r.rate * farmMinutes
		}
		in.SPTotal += sp
	}
	if in.Accounts < 1 {
		in.Accounts = 1
	}
	return in, true
}

// farmGoodView — карточка одного товара с мини-графиком.
type farmGoodView struct {
	Key     string // plex | ext | inj — id для тоглов и пересчёта на лету
	TypeID  int64
	Name    string
	Global  bool
	Side    string // выбранная в параметрах сторона
	Used    float64
	Stats   esi.OrderStats
	Spark   template.HTML // линия цены на сетке за выбранное окно
	Avg     float64
	Change  float64
	HasHist bool
	From    string // с какой даты есть история (вместе со снапшотами)

	// Положение текущей цены в годовом диапазоне — бейдж «ловить
	// просадку или ждать»: ферма живёт на закупе в ямах.
	YearMin, YearMax float64
	Badge            string
	BadgeClass       string // '' | 'pos' | 'warn'
	WantLow          bool   // товар покупается (низкая цена — хорошо)
}

// rangeBadge оценивает текущую цену против годового диапазона дневных
// средних. wantLow — товар покупается, дешёвое хорошо; иначе продаётся.
func rangeBadge(cur, lo, hi float64, wantLow bool) (string, string) {
	if cur <= 0 || hi <= 0 || hi <= lo {
		return "", ""
	}
	nearLo := cur <= lo*1.03
	nearHi := cur >= hi*0.97
	var text string
	switch {
	case nearHi:
		text = "у максимума года"
	case nearLo:
		text = "у минимума года"
	default:
		if off := (1 - cur/hi) * 100; off >= 3 {
			text = fmt.Sprintf("−%.0f%% к пику года", off)
		} else {
			text = "чуть ниже пика"
		}
	}
	mid := (lo + hi) / 2
	class := ""
	switch {
	case (wantLow && (nearLo || cur < mid)) && !nearHi:
		class = "pos"
	case (!wantLow && (nearHi || cur > mid)) && !nearLo:
		class = "pos"
	case wantLow && nearHi, !wantLow && nearLo:
		class = "warn"
	}
	return text, class
}

func clampPct(v float64) float64 {
	return math.Max(0, math.Min(100, v))
}

// mergedSeries склеивает свои дневные снапшоты (слева) с историей ESI:
// где есть ESI — верить ей, свои дни нужны только до начала её окна.
func mergedSeries(db []store.FarmDay, hist esi.PriceSeries) esi.PriceSeries {
	if len(hist) == 0 {
		out := make(esi.PriceSeries, 0, len(db))
		for _, d := range db {
			out = append(out, esi.PriceDay{Day: d.Day, Average: d.Sell})
		}
		return out
	}
	first := hist[0].Day
	var out esi.PriceSeries
	for _, d := range db {
		if d.Day.Before(first) {
			out = append(out, esi.PriceDay{Day: d.Day, Average: d.Sell})
		}
	}
	return append(out, hist...)
}

// ── страница ─────────────────────────────────────────────────────────

func (s *Server) handleSPFarm(w http.ResponseWriter, r *http.Request) {
	ec, stale := s.esiFor(r)
	data, _, err := s.shell(ec, 0, "")
	if err != nil {
		httpError(w, "loading characters", err)
		return
	}
	p := s.farmParams()

	days := 365
	if d, err := strconv.Atoi(r.URL.Query().Get("d")); err == nil && d >= 14 && d <= 3650 {
		days = d
	}

	goods := []farmGoodView{
		{Key: "plex", TypeID: esi.TypePLEX, Name: "PLEX", Global: true, Side: p.PlexSide, WantLow: true},
		{Key: "ext", TypeID: esi.TypeSkillExtractor, Name: "Экстрактор навыков", Side: p.ExtSide, WantLow: true},
		{Key: "inj", TypeID: esi.TypeLargeInjector, Name: "Большой инжектор навыков", Side: p.InjSide},
	}
	series := make([]esi.PriceSeries, len(goods))
	var wg sync.WaitGroup
	for i := range goods {
		region := int64(esi.RegionTheForge)
		if goods[i].Global {
			region = esi.RegionPLEX
		}
		wg.Add(1)
		go func(i int, region int64) {
			defer wg.Done()
			if st, err := ec.RegionOrderStats(region, goods[i].TypeID); err == nil {
				goods[i].Stats = st
			}
		}(i, region)
		wg.Add(1)
		go func(i int, region int64) {
			defer wg.Done()
			hist, err := ec.RegionHistory(region, goods[i].TypeID)
			if err != nil {
				return
			}
			db, _ := s.Store.FarmDailyPrices(goods[i].TypeID)
			series[i] = mergedSeries(db, hist)
		}(i, region)
	}
	wg.Wait()

	// Свой снапшот, если коллектор давно (или вовсе) не писал — так
	// история копится и на копии с COLLECTOR=off.
	s.maybeSnapFarm(goods)

	pkgYear, _ := s.packageYear(p)
	in, live := s.farmInputsFromRoster(ec, p, pkgYear)
	if !live {
		in = farmInputs{
			SPTotal:    p.spPerAccount() * float64(p.Accounts),
			Accounts:   p.Accounts,
			PerPlexAcc: pkgYear + p.BoosterPLEX,
			TaxPct:     p.TaxPct,
		}
	}
	calc := newFarmCalc(in, p, goods[0].Stats, goods[1].Stats, goods[2].Stats)
	yearCut := time.Now().UTC().AddDate(0, 0, -365)
	for i := range goods {
		g := &goods[i]
		g.Used = sidePrice(g.Stats, g.Side)
		for _, d := range series[i] {
			if d.Day.Before(yearCut) || d.Average <= 0 {
				continue
			}
			if g.YearMin == 0 || d.Average < g.YearMin {
				g.YearMin = d.Average
			}
			if d.Average > g.YearMax {
				g.YearMax = d.Average
			}
		}
		g.Badge, g.BadgeClass = rangeBadge(g.Used, g.YearMin, g.YearMax, g.WantLow)
		fillGoodChart(g, series[i], days)
	}
	// Шкала вердикта: где текущий PLEX и порог окупаемости лежат в
	// годовом диапазоне.
	if lo, hi := goods[0].YearMin, goods[0].YearMax; calc.Ready && hi > lo {
		calc.HasRange = true
		calc.BarPos = clampPct((calc.PlexPrice - lo) / (hi - lo) * 100)
		calc.TickPos = clampPct((calc.BreakEven - lo) / (hi - lo) * 100)
	}

	// Профит по дням: дневные средние всех трёх товаров через одну
	// модель. Стороны стакана тут не применить — история хранит только
	// среднюю цену дня.
	dpts := alignFarmSeries(series, days)
	profitSVG, profitNote := renderFarmProfitChart(in, dpts)

	// Данные для пересчёта вердикта на лету: тогл стороны и «своя цена»
	// пересчитывают профит в браузере, без перезагрузки. История нужна
	// графику: товар со «своей ценой» считается по ней как по константе,
	// остальные — по дневной истории.
	histDates := make([]string, len(dpts))
	histPlex := make([]float64, len(dpts))
	histExt := make([]float64, len(dpts))
	histInj := make([]float64, len(dpts))
	for i, q := range dpts {
		histDates[i] = q.Day.Format("2006-01-02")
		histPlex[i], histExt[i], histInj[i] = q.Plex, q.Ext, q.Inj
	}
	farmJSON, _ := json.Marshal(map[string]any{
		"hist": map[string]any{
			"dates": histDates, "plex": histPlex, "ext": histExt, "inj": histInj,
		},
		"goods": map[string]any{
			"plex": map[string]float64{"sell": goods[0].Stats.SellMin, "buy": goods[0].Stats.BuyMax},
			"ext":  map[string]float64{"sell": goods[1].Stats.SellMin, "buy": goods[1].Stats.BuyMax},
			"inj":  map[string]float64{"sell": goods[2].Stats.SellMin, "buy": goods[2].Stats.BuyMax},
		},
		"spTotal":  in.SPTotal,
		"accounts": in.Accounts,
		"perPlex":  in.PerPlexAcc,
		"tax":      in.TaxPct,
		"plexLo":   goods[0].YearMin,
		"plexHi":   goods[0].YearMax,
	})
	data["FarmJSON"] = template.JS(farmJSON)

	data["P"] = p
	data["Calc"] = calc
	data["Goods"] = goods
	data["ProfitChart"] = profitSVG
	data["ProfitNote"] = profitNote
	data["FarmStats"] = s.farmStats(ec)
	data["Year"] = time.Now().UTC().Year()
	data["Days"] = days
	data["DayOptions"] = []int{90, 365, 730}
	data["Err"] = r.URL.Query().Get("err")
	s.render(w, "spfarm", data, stale)
}

// farmStatRow — строка статистики фермы под графиками: выбранный
// персонаж с настроенным пулом, его прогресс по пулу и AIR за год.
type farmStatRow struct {
	ID        int64
	Name      string
	Skill     string
	Rate      float64
	InPool    bool
	Paused    bool
	PoolSP    int64 // SP, уже выученные по навыкам из пула
	PoolKnown int   // сколько навыков пула персонаж уже знает
	PoolTotal int   // строк-навыков в пуле
	AirSP     int64 // SP от AIR в этом году
}

// farmStats собирает статистику для выбранных персонажей фермы с
// настроенным пулом: выучено SP из пула (полный скилл-лист ESI) и AIR
// за текущий год (закрытые месяцы + текущий прогресс).
func (s *Server) farmStats(ec *esi.Client) []farmStatRow {
	farmChars, err := s.Store.FarmChars()
	if err != nil || len(farmChars) == 0 {
		return nil
	}
	chars, err := s.Store.Characters()
	if err != nil {
		return nil
	}
	names := map[int64]string{}
	order := []int64{}
	for _, ch := range chars {
		if _, ok := farmChars[ch.ID]; ok {
			names[ch.ID] = ch.Name
			order = append(order, ch.ID)
		}
	}
	airSP, _ := s.Store.AirYearSP(time.Now().UTC())

	now := time.Now()
	var mu sync.Mutex
	var wg sync.WaitGroup
	var rows []farmStatRow
	for _, id := range order {
		pool := farmChars[id]
		if poolCount(pool) == 0 {
			continue // статистика по пулу без пула бессмысленна
		}
		wg.Add(1)
		go func(id int64, pool string) {
			defer wg.Done()
			row := farmStatRow{
				ID: id, Name: names[id],
				PoolTotal: poolCount(pool),
				AirSP:     airSP[id],
			}
			queue, _ := ec.SkillQueue(id)
			row.Skill, row.Rate = queueRate(queue, now)
			row.Paused = row.Rate == 0
			row.InPool = poolHas(pool, row.Skill)
			if sheet, err := ec.Skills(id); err == nil && sheet != nil {
				set := poolSkills(pool)
				for _, sk := range sheet.Skills {
					if set[strings.ToLower(strings.TrimSpace(sk.SkillName))] {
						row.PoolSP += sk.Skillpoints
						row.PoolKnown++
						if debugFarmStats {
							log.Printf("farmStats %s: пул-навык %q sp=%d", row.Name, sk.SkillName, sk.Skillpoints)
						}
					}
				}
				if debugFarmStats {
					log.Printf("farmStats %s: skills=%d, пул=%d, известно=%d, sp=%d",
						row.Name, len(sheet.Skills), row.PoolTotal, row.PoolKnown, row.PoolSP)
				}
			} else if debugFarmStats {
				log.Printf("farmStats %s: skills err=%v", row.Name, err)
			}
			mu.Lock()
			rows = append(rows, row)
			mu.Unlock()
		}(id, pool)
	}
	wg.Wait()
	sort.Slice(rows, func(i, j int) bool { return rows[i].Name < rows[j].Name })
	return rows
}

// modelAccountView — один аккаунт кабинета на странице модели: участие
// в ферме и персонажи с живыми скоростями.
type modelAccountView struct {
	Key      string
	Name     string
	InFarm   bool
	Chars    []farmCharView
	SelCount int     // выбранных персонажей
	SPSum    float64 // их суммарный прогноз SP/год
}

// handleSPFarmModel — «Модель фермы»: ростер фермы, предложения магазина
// EVE и прочие параметры. В главное меню не выведена — вход через
// вкладки SP-фермы.
func (s *Server) handleSPFarmModel(w http.ResponseWriter, r *http.Request) {
	ec, stale := s.esiFor(r)
	data, _, err := s.shell(ec, 0, "")
	if err != nil {
		httpError(w, "loading characters", err)
		return
	}
	p := s.farmParams()

	farmAccts, _ := s.Store.FarmAccounts()
	farmChars, _ := s.Store.FarmChars()

	// Живые скорости: очередь навыков каждого персонажа (кэш ESI уже
	// прогрет сайдбаром, так что это дёшево).
	groups, _ := data["Groups"].([]accountGroup)
	now := time.Now()
	var views []modelAccountView
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, g := range groups {
		av := modelAccountView{Key: g.Key, Name: g.Name, InFarm: farmAccts[g.Key]}
		av.Chars = make([]farmCharView, len(g.Chars))
		for i, ch := range g.Chars {
			pool, selected := farmChars[ch.ID]
			av.Chars[i] = farmCharView{
				ID: ch.ID, Name: ch.Name, Selected: selected, Pool: pool,
				PoolCount: poolCount(pool),
			}
			wg.Add(1)
			go func(cv *farmCharView, id int64) {
				defer wg.Done()
				queue, _ := ec.SkillQueue(id)
				skill, rate := queueRate(queue, now)
				mu.Lock()
				cv.Skill, cv.Rate = skill, rate
				cv.Paused = rate == 0
				cv.InPool = poolHas(cv.Pool, skill)
				cv.AirSP = p.charAirSP(id)
				sp := cv.AirSP
				if rate > 0 && cv.InPool {
					sp += rate * farmMinutes
				}
				cv.SPYear = sp
				mu.Unlock()
			}(&av.Chars[i], ch.ID)
		}
		views = append(views, av)
	}
	wg.Wait()

	// Итог по выбранным — тот же расчёт, что на вкладке анализа.
	var spTotal float64
	var selChars int
	for i := range views {
		for _, cv := range views[i].Chars {
			if cv.Selected {
				views[i].SelCount++
				views[i].SPSum += cv.SPYear
				spTotal += cv.SPYear
				selChars++
			}
		}
	}

	offers, _ := s.Store.FarmOffers()
	pkgYear, pkgName := s.packageYear(p)

	// Планы прокачки — заготовки для модалки пула навыков.
	plans, _ := s.Store.FarmPlans()
	plansJSON, _ := json.Marshal(plans)
	data["PlansJSON"] = template.JS(plansJSON)

	data["P"] = p
	data["Roster"] = views
	data["SPTotal"] = spTotal
	data["SelChars"] = selChars
	data["FarmAccounts"] = len(farmAccts)
	data["Offers"] = offers
	data["PkgYear"] = pkgYear
	data["PkgName"] = pkgName
	data["Err"] = r.URL.Query().Get("err")
	s.render(w, "spfarm_model", data, stale)
}

// handleSPFarmRoster сохраняет ростер фермы: отмеченные аккаунты,
// персонажи и их пулы навыков — одной формой, целиком.
func (s *Server) handleSPFarmRoster(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	chars := map[int64]string{}
	for _, v := range r.Form["char"] {
		id, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			continue
		}
		chars[id] = strings.TrimSpace(r.FormValue(fmt.Sprintf("pool-%d", id)))
	}
	if err := s.Store.SetFarmRoster(r.Form["acct"], chars); err != nil {
		httpError(w, "saving farm roster", err)
		return
	}
	farmRedirect(w, r, "/tools/spfarm/model", "")
}

// ── планы прокачки ───────────────────────────────────────────────────
// Модалка пула навыков ходит сюда fetch'ем, без перезагрузки страницы —
// несохранённые галочки ростера при этом не теряются.

func (s *Server) handleSPFarmPlanAdd(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.FormValue("name"))
	body := r.FormValue("body")
	if name == "" || strings.TrimSpace(body) == "" {
		http.Error(w, "нужны название и список навыков", http.StatusBadRequest)
		return
	}
	id, err := s.Store.AddFarmPlan(name, body)
	if err != nil {
		httpError(w, "saving plan", err)
		return
	}
	w.Header().Set("Content-Type", "text/plain")
	fmt.Fprint(w, id)
}

func (s *Server) handleSPFarmPlanDelete(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.FormValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	if err := s.Store.DeleteFarmPlan(id); err != nil {
		httpError(w, "deleting plan", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ── предложения магазина EVE ─────────────────────────────────────────

func (s *Server) handleSPFarmOfferAdd(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.FormValue("name"))
	plex, err1 := parseFarmNum(r.FormValue("plex"))
	months, err2 := parseFarmNum(r.FormValue("months"))
	if name == "" || err1 != nil || plex <= 0 {
		farmRedirect(w, r, "/tools/spfarm/model", "предложение: нужны название и цена в PLEX")
		return
	}
	if err2 != nil || months <= 0 {
		months = 12
	}
	if err := s.Store.AddFarmOffer(store.FarmOffer{
		Name: name, Plex: plex, Months: int(months),
	}); err != nil {
		httpError(w, "saving offer", err)
		return
	}
	farmRedirect(w, r, "/tools/spfarm/model", "")
}

func (s *Server) handleSPFarmOfferDelete(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.FormValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	if err := s.Store.DeleteFarmOffer(id); err != nil {
		httpError(w, "deleting offer", err)
		return
	}
	// Удалили выбранное — модель откатывается на запасное значение.
	if p := s.farmParams(); p.OfferID == id {
		p.OfferID = 0
		raw, _ := json.Marshal(p)
		_ = s.Store.SetSetting("spfarm_params", string(raw))
	}
	farmRedirect(w, r, "/tools/spfarm/model", "")
}

// handleSPFarmOfferSelect выбирает предложение для модели (0 — никакое,
// работает запасное значение PLEX/год).
func (s *Server) handleSPFarmOfferSelect(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.FormValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	p := s.farmParams()
	p.OfferID = id
	raw, _ := json.Marshal(p)
	if err := s.Store.SetSetting("spfarm_params", string(raw)); err != nil {
		httpError(w, "saving farm params", err)
		return
	}
	farmRedirect(w, r, "/tools/spfarm/model", "")
}

// handlePlexVault — «Хранилище PLEX»: журнал закупа отдельным
// инструментом. Цена PLEX нужна для сводки «сейчас / против текущей».
func (s *Server) handlePlexVault(w http.ResponseWriter, r *http.Request) {
	ec, stale := s.esiFor(r)
	data, _, err := s.shell(ec, 0, "")
	if err != nil {
		httpError(w, "loading characters", err)
		return
	}
	p := s.farmParams()

	var plexSt esi.OrderStats
	if st, err := ec.RegionOrderStats(esi.RegionPLEX, esi.TypePLEX); err == nil {
		plexSt = st
	}

	buys, err := s.Store.PlexPurchases()
	if err != nil {
		httpError(w, "loading purchases", err)
		return
	}

	data["Purchases"] = buys
	data["Buy"] = newBuySummary(buys, sidePrice(plexSt, p.PlexSide))
	data["Today"] = time.Now().UTC().Format("2006-01-02")
	data["Err"] = r.URL.Query().Get("err")
	s.render(w, "plex_vault", data, stale)
}

// maybeSnapFarm пишет снапшот цен не чаще раза в час, из уже полученных
// стаканов. Кэш ESI (5 минут) может сделать точку чуть несвежей — для
// годового графика это неважно.
func (s *Server) maybeSnapFarm(goods []farmGoodView) {
	if time.Since(s.Store.LastFarmSnapAt()) < 55*time.Minute {
		return
	}
	now := time.Now()
	var snaps []store.FarmSnap
	for _, g := range goods {
		st := g.Stats
		if st.SellMin == 0 && st.BuyMax == 0 {
			continue
		}
		snaps = append(snaps, store.FarmSnap{
			TypeID: g.TypeID, At: now,
			SellMin: st.SellMin, SellP98: st.SellP98,
			BuyMax: st.BuyMax, BuyP98: st.BuyP98,
		})
	}
	if len(snaps) > 0 {
		_ = s.Store.SaveFarmSnaps(snaps)
	}
}

// fillGoodChart готовит мини-график товара за окно в days дней.
func fillGoodChart(g *farmGoodView, hist esi.PriceSeries, days int) {
	if len(hist) == 0 {
		return
	}
	g.From = hist[0].Day.Format("02.01.2006")
	cut := time.Now().UTC().AddDate(0, 0, -days)
	start := 0
	for i, d := range hist {
		if !d.Day.Before(cut) {
			start = i
			break
		}
	}
	hist = hist[start:]
	if len(hist) < 2 {
		return
	}
	g.Avg = hist[len(hist)-1].Average
	if first := hist[0].Average; first > 0 {
		g.Change = (g.Avg/first - 1) * 100
	}
	g.Spark = renderSpark(hist)
	g.HasHist = g.Spark != ""
}

// Спарклайн цены: линия на сетке, шкала — минимум и максимум за
// выбранное окно (не от нуля: колебания цены иначе не разглядеть).
const (
	sparkW    = 320.0
	sparkH    = 120.0
	sparkPad  = 12.0 // верх/низ под сетку
	sparkSide = 4.0
)

// sparkLabel сжимает цену до подписи сетки: 4.84M, 469M, 1.02B.
func sparkLabel(v float64) string {
	switch {
	case v >= 1e9:
		return strconv.FormatFloat(v/1e9, 'f', 2, 64) + "B"
	case v >= 1e7:
		return strconv.FormatFloat(v/1e6, 'f', 0, 64) + "M"
	case v >= 1e6:
		return strconv.FormatFloat(v/1e6, 'f', 2, 64) + "M"
	}
	return formatNum(int64(v + 0.5))
}

func renderSpark(hist esi.PriceSeries) template.HTML {
	// На длинном окне точек больше, чем пикселей — сжимаем корзинами.
	if n := len(hist); n > 240 {
		bucket := (n + 239) / 240
		var packed esi.PriceSeries
		for i := 0; i < n; i += bucket {
			end := i + bucket
			if end > n {
				end = n
			}
			var sum float64
			for _, d := range hist[i:end] {
				sum += d.Average
			}
			packed = append(packed, esi.PriceDay{
				Day: hist[i].Day, Average: sum / float64(end-i),
			})
		}
		hist = packed
	}
	lo, hi := hist[0].Average, hist[0].Average
	for _, d := range hist {
		lo = math.Min(lo, d.Average)
		hi = math.Max(hi, d.Average)
	}
	if hi <= lo {
		hi = lo + 1
	}
	x := func(i int) float64 {
		return sparkSide + float64(i)*(sparkW-2*sparkSide)/float64(len(hist)-1)
	}
	y := func(v float64) float64 {
		return (sparkH - sparkPad) - (v-lo)/(hi-lo)*(sparkH-2*sparkPad)
	}

	var b strings.Builder
	fmt.Fprintf(&b, `<svg class="spark" viewBox="0 0 %g %g" xmlns="http://www.w3.org/2000/svg" style="height:auto; margin-top:.6rem;">`, sparkW, sparkH)
	// Сетка: максимум, середина, минимум окна.
	for _, v := range []float64{hi, (hi + lo) / 2, lo} {
		fmt.Fprintf(&b, `<line x1="%g" y1="%.1f" x2="%g" y2="%.1f" stroke="#1c2536" stroke-dasharray="4 4"/>`,
			sparkSide, y(v), sparkW-sparkSide, y(v))
		fmt.Fprintf(&b, `<text x="%g" y="%.1f" font-size="10" fill="#5f708c" text-anchor="end">%s</text>`,
			sparkW-sparkSide-2, y(v)-2.5, sparkLabel(v))
	}
	var line []string
	for i, d := range hist {
		line = append(line, fmt.Sprintf("%.1f,%.1f", x(i), y(d.Average)))
	}
	area := append(append([]string{}, line...),
		fmt.Sprintf("%.1f,%.1f", x(len(hist)-1), y(lo)),
		fmt.Sprintf("%.1f,%.1f", x(0), y(lo)))
	fmt.Fprintf(&b, `<polygon points="%s" fill="rgba(77,163,255,.07)"/>`, strings.Join(area, " "))
	fmt.Fprintf(&b, `<polyline points="%s" fill="none" stroke="#4da3ff" stroke-width="1.5"/>`, strings.Join(line, " "))
	b.WriteString(`</svg>`)
	return template.HTML(b.String())
}

// ── график профита ───────────────────────────────────────────────────

const (
	fpW   = 1000.0
	fpH   = 470.0
	fpPad = 14.0
	fpTop = 12.0
	fpBot = 430.0
)

// farmDayPoint — один день общей оси трёх товаров. Из него считается и
// серверный график, и клиентский пересчёт «а что если» в браузере.
type farmDayPoint struct {
	Day            time.Time
	Plex, Ext, Inj float64
}

// alignFarmSeries выравнивает три истории на общую дневную ось: с самого
// позднего из трёх начал (раньше него чьей-то цены просто нет) до самого
// позднего известного дня, обрезая по окну days.
func alignFarmSeries(series []esi.PriceSeries, days int) []farmDayPoint {
	if len(series) != 3 || len(series[0]) == 0 || len(series[1]) == 0 || len(series[2]) == 0 {
		return nil
	}
	first := series[0][0].Day
	last := series[0][len(series[0])-1].Day
	for _, sr := range series[1:] {
		if f := sr[0].Day; f.After(first) {
			first = f
		}
		if l := sr[len(sr)-1].Day; l.After(last) {
			last = l
		}
	}
	if cut := time.Now().UTC().AddDate(0, 0, -days); first.Before(cut) {
		first = cut
	}
	if !first.Before(last) {
		return nil
	}
	var pts []farmDayPoint
	for d := first; !d.After(last); d = d.AddDate(0, 0, 1) {
		plexP, extP, injP := series[0].At(d), series[1].At(d), series[2].At(d)
		if plexP == 0 || extP == 0 || injP == 0 {
			continue
		}
		pts = append(pts, farmDayPoint{Day: d, Plex: plexP, Ext: extP, Inj: injP})
	}
	return pts
}

// renderFarmProfitChart строит SVG «профит всей фермы в год по дневным
// ценам». Возвращает пустую строку, если истории мало.
func renderFarmProfitChart(in farmInputs, dpts []farmDayPoint) (template.HTML, string) {
	type pt struct {
		day time.Time
		v   float64
	}
	var pts []pt
	for _, q := range dpts {
		pts = append(pts, pt{day: q.Day, v: farmProfitTotal(in, q.Plex, q.Ext, q.Inj)})
	}
	if len(pts) < 2 {
		return "", ""
	}

	vmin, vmax := pts[0].v, pts[0].v
	for _, q := range pts {
		vmin = math.Min(vmin, q.v)
		vmax = math.Max(vmax, q.v)
	}
	if vmax <= vmin {
		vmax = vmin + 1
	}
	span := vmax - vmin
	vmin -= span * 0.06
	vmax += span * 0.06

	x := func(i int) float64 {
		return fpPad + float64(i)*(fpW-2*fpPad)/float64(len(pts)-1)
	}
	y := func(v float64) float64 {
		return fpBot - (v-vmin)/(vmax-vmin)*(fpBot-fpTop)
	}

	var b strings.Builder
	fmt.Fprintf(&b, `<svg class="fchart" viewBox="0 0 %g %g" preserveAspectRatio="xMidYMid meet" xmlns="http://www.w3.org/2000/svg">`, fpW, fpH)
	for i := 0; i <= 4; i++ {
		v := vmin + (vmax-vmin)*float64(i)/4
		fmt.Fprintf(&b, `<line x1="%g" y1="%.1f" x2="%g" y2="%.1f" stroke="#1c2536" stroke-dasharray="4 4"/>`,
			fpPad, y(v), fpW-fpPad, y(v))
		fmt.Fprintf(&b, `<text x="%g" y="%.1f" font-size="11" fill="#5f708c" text-anchor="end">%s</text>`,
			fpW-fpPad-4, y(v)-3, iskShort(v))
	}
	// Нулевая линия и подкрашенная зона убытка — граница «ферма
	// окупается / нет» видна без чтения осей.
	if vmin < 0 && vmax > 0 {
		fmt.Fprintf(&b, `<rect x="%g" y="%.1f" width="%g" height="%.1f" fill="rgba(224,108,108,.05)"/>`,
			fpPad, y(0), fpW-2*fpPad, fpBot-y(0))
		fmt.Fprintf(&b, `<line x1="%g" y1="%.1f" x2="%g" y2="%.1f" stroke="#e06c6c" stroke-dasharray="6 4"/>`,
			fpPad, y(0), fpW-fpPad, y(0))
		fmt.Fprintf(&b, `<text x="%g" y="%.1f" font-size="11" fill="#e06c6c">зона убытка</text>`,
			fpPad+6, y(0)+14)
	}
	// Метки месяцев.
	prevMonth := -1
	for i, q := range pts {
		if m := int(q.day.Month()); m != prevMonth {
			if prevMonth != -1 {
				fmt.Fprintf(&b, `<line x1="%.1f" y1="%g" x2="%.1f" y2="%g" stroke="#1c2536"/>`,
					x(i), fpTop, x(i), fpBot)
				fmt.Fprintf(&b, `<text x="%.1f" y="%g" font-size="11" fill="#5f708c" text-anchor="middle">%s</text>`,
					x(i), fpBot+16, ruMonths[m-1])
			}
			prevMonth = m
		}
	}
	// Заливка до нуля (или до низа, если весь график в плюсе).
	base := y(math.Max(0, vmin))
	var area, line []string
	for i, q := range pts {
		xy := fmt.Sprintf("%.1f,%.1f", x(i), y(q.v))
		line = append(line, xy)
		area = append(area, xy)
	}
	area = append(area,
		fmt.Sprintf("%.1f,%.1f", x(len(pts)-1), base),
		fmt.Sprintf("%.1f,%.1f", x(0), base))
	fmt.Fprintf(&b, `<polygon points="%s" fill="rgba(95,211,141,.08)"/>`, strings.Join(area, " "))
	fmt.Fprintf(&b, `<polyline points="%s" fill="none" stroke="#5fd38d" stroke-width="1.6"/>`, strings.Join(line, " "))
	endColor := "#5fd38d"
	if pts[len(pts)-1].v < 0 {
		endColor = "#e06c6c"
	}
	fmt.Fprintf(&b, `<circle cx="%.1f" cy="%.1f" r="3.5" fill="%s"/>`,
		x(len(pts)-1), y(pts[len(pts)-1].v), endColor)
	b.WriteString(`</svg>`)

	note := fmt.Sprintf("с %s по %s, %d точек",
		pts[0].day.Format("02.01.2006"), pts[len(pts)-1].day.Format("02.01.2006"), len(pts))
	return template.HTML(b.String()), note
}

// ── закуп ────────────────────────────────────────────────────────────

type buySummary struct {
	Qty      int64
	ISK      float64
	Avg      float64 // средняя цена закупа
	CurPrice float64
	Diff     float64 // экономия против текущей цены (плюс = закупился дешевле)
}

func newBuySummary(buys []store.PlexPurchase, curPrice float64) buySummary {
	b := buySummary{CurPrice: curPrice}
	for _, q := range buys {
		b.Qty += q.Qty
		b.ISK += float64(q.Qty) * q.Price
	}
	if b.Qty != 0 {
		b.Avg = b.ISK / float64(b.Qty)
	}
	if curPrice > 0 && b.Avg > 0 {
		b.Diff = (curPrice - b.Avg) * float64(b.Qty)
	}
	return b
}

// ── формы ────────────────────────────────────────────────────────────

// parseFarmNum разбирает число из формы: пробелы (в т.ч. неразрывные) —
// разделители тысяч, запятая — десятичная.
func parseFarmNum(v string) (float64, error) {
	v = strings.NewReplacer(" ", "", " ", "", ",", ".").Replace(strings.TrimSpace(v))
	if v == "" {
		return 0, nil
	}
	return strconv.ParseFloat(v, 64)
}

func farmRedirect(w http.ResponseWriter, r *http.Request, dest, errMsg string) {
	if errMsg != "" {
		dest += "?err=" + url.QueryEscape(errMsg)
	}
	http.Redirect(w, r, dest, http.StatusFound)
}

func (s *Server) handleSPFarmParams(w http.ResponseWriter, r *http.Request) {
	p := s.farmParams()
	var err error
	num := func(field string, dst *float64) {
		if err != nil {
			return
		}
		var v float64
		if v, err = parseFarmNum(r.FormValue(field)); err != nil {
			err = fmt.Errorf("поле %q: %v", field, err)
			return
		}
		*dst = v
	}
	num("booster", &p.BoosterPLEX)
	num("tax", &p.TaxPct)
	if err != nil {
		farmRedirect(w, r, "/tools/spfarm/model", err.Error())
		return
	}
	p.UseAIR = r.FormValue("use_air") == "1"

	raw, _ := json.Marshal(p)
	if err := s.Store.SetSetting("spfarm_params", string(raw)); err != nil {
		httpError(w, "saving farm params", err)
		return
	}
	farmRedirect(w, r, "/tools/spfarm/model", "")
}

// handleSPFarmSide сохраняет сторону стакана, выбранную тоглом на
// карточке цены (fetch со страницы, без перезагрузки). «Своя цена» —
// чистый what-if и на сервер не попадает.
func (s *Server) handleSPFarmSide(w http.ResponseWriter, r *http.Request) {
	side := r.FormValue("side")
	if side != "sell" && side != "buy" {
		http.Error(w, "bad side", http.StatusBadRequest)
		return
	}
	p := s.farmParams()
	switch r.FormValue("good") {
	case "plex":
		p.PlexSide = side
	case "ext":
		p.ExtSide = side
	case "inj":
		p.InjSide = side
	default:
		http.Error(w, "bad good", http.StatusBadRequest)
		return
	}
	raw, _ := json.Marshal(p)
	if err := s.Store.SetSetting("spfarm_params", string(raw)); err != nil {
		httpError(w, "saving farm params", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleSPFarmBuy(w http.ResponseWriter, r *http.Request) {
	day := strings.TrimSpace(r.FormValue("day"))
	if day == "" {
		day = time.Now().UTC().Format("2006-01-02")
	}
	if _, err := time.Parse("2006-01-02", day); err != nil {
		farmRedirect(w, r, "/tools/plex", "дата: нужен формат ГГГГ-ММ-ДД")
		return
	}
	qty, err := parseFarmNum(r.FormValue("qty"))
	if err != nil || qty == 0 {
		farmRedirect(w, r, "/tools/plex", "количество PLEX: целое число, отрицательное — расход")
		return
	}
	price, err := parseFarmNum(r.FormValue("price"))
	if err != nil || price < 0 {
		farmRedirect(w, r, "/tools/plex", "цена: ISK за штуку")
		return
	}
	if err := s.Store.AddPlexPurchase(store.PlexPurchase{
		Day: day, Qty: int64(qty), Price: price,
		Note: strings.TrimSpace(r.FormValue("note")),
	}); err != nil {
		httpError(w, "saving purchase", err)
		return
	}
	farmRedirect(w, r, "/tools/plex", "")
}

func (s *Server) handleSPFarmBuyDelete(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.FormValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	if err := s.Store.DeletePlexPurchase(id); err != nil {
		httpError(w, "deleting purchase", err)
		return
	}
	farmRedirect(w, r, "/tools/plex", "")
}
