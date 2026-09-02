package web

import (
	"fmt"
	"net/http"
	"strconv"
	"time"

	"eve-empire/internal/ledger"
	"eve-empire/internal/store"
)

// Складской и финансовый учёт (ACCOUNTING.md, этап 1).
//
// The page shows what the ledger knows, not what the market is worth: the
// stock is valued at what it COST, and the margin is realised profit only.
// Mixing in the revaluation of goods still on the shelf is the classic way
// to mistake a rising market for successful trading.

type accRow struct {
	TypeID   int64
	Name     string
	Sales    int
	Quantity int64
	Revenue  float64
	COGS     float64
	Tax      float64
	Profit   float64
	Margin   float64
}

type accStock struct {
	TypeID    int64
	Name      string
	Where     string
	Quantity  int64
	Cost      float64
	Market    float64 // сколько это стоит сегодня по оценке рынка
	Delta     float64 // рынок минус себестоимость: что даст продажа
	Estimated bool
	Days      int
}

// accProd is one job seen from the money side: what it actually cost
// against what the product is worth now.
type accProd struct {
	Name     string
	Quantity int64
	Cost     float64
	Unit     float64
	Market   float64
	Delta    float64
	InWIP    bool
	When     string
	Note     string
}

// accStage is one processing step and what it added.
type accStage struct {
	Name   string
	Docs   int
	InMkt  float64
	OutMkt float64
	Added  float64
}

var stageNames = map[string]string{
	"purchase": "закупка", "sale": "продажа", "manufacture": "производство",
	"reaction": "реакции", "reprocess": "переработка", "transfer": "перемещение",
	"receipt": "ручной приход", "writeoff": "списание", "delivery": "выдача работ",
	"fee": "комиссии", "opening": "начальные остатки",
}

func (s *Server) handleAccounting(w http.ResponseWriter, r *http.Request) {
	s.accountingPage(w, r, "")
}

// handleAccountingBuild runs the stage-1 builders and re-renders.
//
// СЛЕДСТВИЕ 2 из ARCHITECTURE.md: страница, отвечающая на POST, не
// помечается Stale — иначе ревалидация дёрнет GET и затрёт результат.
func (s *Server) handleAccountingBuild(w http.ResponseWriter, r *http.Request) {
	source := r.FormValue("price")
	if source != "adjusted" {
		source = "average"
	}
	res, err := ledger.New(s.Store, s.ESI).WithSDE(s.SDE).BuildAll(source)
	msg := res.Note
	if err != nil {
		msg = "ошибка сборки: " + err.Error()
	}
	s.accountingPage(w, r, msg)
}

// accRecon is one proposed reconciliation line, ready for the template.
type accRecon struct {
	Kind     string
	TypeID   int64
	Name     string
	Qty      int64
	From     string
	To       string
	FromWho  string
	ToWho    string
	Owner    int64
	Location int64
	Note     string
}

// handleAccountingRecon acts on the reconciliation queue.
//
// Only movements are applied in bulk: a movement carries cost across and
// changes no totals, so being wrong about one is cheap and reversible.
// A surplus or a shortage moves money, and stays a human decision.
func (s *Server) handleAccountingRecon(w http.ResponseWriter, r *http.Request) {
	b := ledger.New(s.Store, s.ESI).WithSDE(s.SDE)
	var msg string

	switch r.FormValue("do") {
	case "transfers":
		sum, err := s.Store.Reconcile()
		if err != nil {
			httpError(w, "reconcile", err)
			return
		}
		n, err := b.ApplyTransfers(ledger.Classify(sum))
		if err != nil {
			msg = "перемещения: " + err.Error()
		} else {
			msg = fmt.Sprintf("проведено перемещений: %d", n)
		}
	case "receipt":
		owner, loc, typeID, qty := reconArgs(r)
		unit := 0.0
		estimated := false
		if r.FormValue("price") == "market" {
			if prices, err := s.ESI.MarketPrices(); err == nil {
				unit = prices[typeID].Average
			}
			estimated = true
		}
		src := r.FormValue("source")
		if src == "" {
			src = "не указан"
		}
		if err := b.PostReceipt(owner, loc, typeID, qty, unit, src, estimated); err != nil {
			msg = "приход: " + err.Error()
		} else {
			msg = fmt.Sprintf("оприходовано %d ед. (%s)", qty, src)
		}
	case "writeoff":
		owner, loc, typeID, qty := reconArgs(r)
		reason := r.FormValue("source")
		if reason == "" {
			reason = "не указана"
		}
		if err := b.PostWriteOff(owner, loc, typeID, qty, reason); err != nil {
			msg = "списание: " + err.Error()
		} else {
			msg = fmt.Sprintf("списано %d ед. (%s)", qty, reason)
		}
	}
	s.accountingPage(w, r, msg)
}

// handleAccountingClose seals the books before a date.
func (s *Server) handleAccountingClose(w http.ResponseWriter, r *http.Request) {
	var msg string
	if r.FormValue("do") == "open" {
		if err := s.Store.SetClosedBefore(time.Time{}); err != nil {
			msg = "открыть период: " + err.Error()
		} else {
			msg = "период снова открыт — задним числом снова можно"
		}
	} else {
		d, err := time.Parse("2006-01-02", r.FormValue("date"))
		if err != nil {
			msg = "не понял дату, нужен формат ГГГГ-ММ-ДД"
		} else if err := s.Store.SetClosedBefore(d); err != nil {
			msg = "закрыть период: " + err.Error()
		} else {
			msg = "период закрыт до " + d.Format("02.01.2006") +
				" — правки задним числом больше не проводятся"
		}
	}
	s.accountingPage(w, r, msg)
}

func reconArgs(r *http.Request) (owner, loc, typeID, qty int64) {
	n := func(k string) int64 {
		v, _ := strconv.ParseInt(r.FormValue(k), 10, 64)
		return v
	}
	return n("owner"), n("loc"), n("type"), n("qty")
}

func (s *Server) accountingPage(w http.ResponseWriter, r *http.Request, msg string) {
	ec, stale := s.esiFor(r)
	if r.Method == http.MethodPost {
		stale = nil
	}
	data, _, err := s.shell(ec, 0, "accounting")
	if err != nil {
		httpError(w, "loading characters", err)
		return
	}

	att, err := s.Store.Attention()
	if err != nil {
		httpError(w, "reading ledger", err)
		return
	}

	from := time.Now().AddDate(0, 0, -30)
	to := time.Now().Add(time.Hour)
	margins, err := s.Store.Margins(from, to)
	if err != nil {
		httpError(w, "margins", err)
		return
	}
	stock, err := s.Store.Stock()
	if err != nil {
		httpError(w, "stock", err)
		return
	}

	var ids []int64
	for _, m := range margins {
		ids = append(ids, m.TypeID)
	}
	for i, st := range stock {
		if i >= 40 {
			break
		}
		ids = append(ids, st.TypeID)
	}
	names := ec.Names(ids)

	rows := make([]accRow, 0, len(margins))
	var totRevenue, totCOGS, totTax, totProfit float64
	for _, m := range margins {
		totRevenue += m.Revenue
		totCOGS += m.COGS
		totTax += m.Tax
		totProfit += m.Profit()
		rows = append(rows, accRow{
			TypeID: m.TypeID, Name: typeName(names, m.TypeID), Sales: m.Sales,
			Quantity: m.Quantity, Revenue: m.Revenue, COGS: m.COGS, Tax: m.Tax,
			Profit: m.Profit(), Margin: m.MarginPct(),
		})
	}

	// Рынок рядом с себестоимостью: без этой пары нельзя ответить, что из
	// лежащего стоит продать, а что продавать себе дороже.
	prices, _ := s.ESI.MarketPrices()
	now := time.Now()
	shelf := make([]accStock, 0, 40)
	for i, st := range stock {
		if i >= 40 {
			break
		}
		where := st.HolderName
		if where == "" {
			where = "ангар"
		}
		market := prices[st.TypeID].Average * float64(st.Quantity)
		shelf = append(shelf, accStock{
			TypeID: st.TypeID, Name: typeName(names, st.TypeID), Where: where,
			Quantity: st.Quantity, Cost: st.Cost, Market: market,
			Delta: market - st.Cost, Estimated: st.Estimated,
			Days: int(now.Sub(st.OldestAt).Hours() / 24),
		})
	}

	// ── сверка ──
	sum, err := s.Store.Reconcile()
	if err != nil {
		httpError(w, "reconcile", err)
		return
	}
	props := ledger.Classify(sum)
	var locIDs []int64
	var typeIDs []int64
	for _, p := range props {
		locIDs = append(locIDs, p.FromLocation, p.ToLocation)
		typeIDs = append(typeIDs, p.TypeID, p.FromOwner, p.ToOwner)
	}
	locNames := map[int64]string{}
	if len(locIDs) > 0 {
		if ch, err := s.Store.Characters(); err == nil && len(ch) > 0 {
			locNames = ec.LocationNames(ch[0].ID, locIDs)
		}
	}
	propNames := ec.Names(typeIDs)
	transfers := 0
	recon := make([]accRecon, 0, len(props))
	for _, p := range props {
		if p.Kind == "transfer" {
			transfers++
		}
		owner, loc := p.ToOwner, p.ToLocation
		if p.Kind == "writeoff" {
			owner, loc = p.FromOwner, p.FromLocation
		}
		note := ""
		if p.OnMarket > 0 {
			note = fmt.Sprintf("на витрине %d", p.OnMarket)
		} else if p.InSafety > 0 {
			note = fmt.Sprintf("в asset safety %d", p.InSafety)
		}
		recon = append(recon, accRecon{
			Kind: p.Kind, TypeID: p.TypeID, Name: typeName(propNames, p.TypeID),
			Qty: p.Qty, From: locName(locNames, p.FromLocation),
			To:      locName(locNames, p.ToLocation),
			FromWho: locName(propNames, p.FromOwner),
			ToWho:   locName(propNames, p.ToOwner),
			Owner:   owner, Location: loc, Note: note,
		})
	}

	// ── производство: факт против рынка ──
	prod, err := s.Store.Production(from, to)
	if err != nil {
		httpError(w, "production", err)
		return
	}
	var prodIDs []int64
	for _, p := range prod {
		prodIDs = append(prodIDs, p.TypeID)
	}
	prodNames := ec.Names(prodIDs)
	made := make([]accProd, 0, len(prod))
	var madeCost, madeMarket float64
	for _, p := range prod {
		market := prices[p.TypeID].Average * float64(p.Quantity)
		unit := 0.0
		if p.Quantity > 0 {
			unit = p.Cost / float64(p.Quantity)
		}
		madeCost += p.Cost
		madeMarket += market
		made = append(made, accProd{
			Name: typeName(prodNames, p.TypeID), Quantity: p.Quantity,
			Cost: p.Cost, Unit: unit, Market: market, Delta: market - p.Cost,
			InWIP: p.InWIP, When: p.At.Format("02.01 15:04"), Note: p.Note,
		})
	}
	data["Made"] = made
	data["MadeCost"] = madeCost
	data["MadeMarket"] = madeMarket
	data["MadeDelta"] = madeMarket - madeCost

	// ── капитал и контрольная сумма ──
	cap30, err := s.Store.Capital(from, to)
	if err != nil {
		httpError(w, "capital", err)
		return
	}
	var kinds []store.CashKind
	for i, k := range cap30.Kinds {
		if i >= 14 {
			break
		}
		kinds = append(kinds, k)
	}
	data["Cap"] = cap30
	data["CapKinds"] = kinds
	data["CapKindsMore"] = len(cap30.Kinds) - len(kinds)

	stages, err := s.Store.Stages(from, to)
	if err != nil {
		httpError(w, "stages", err)
		return
	}
	byStage := make([]accStage, 0, len(stages))
	for _, st := range stages {
		name := stageNames[st.Kind]
		if name == "" {
			name = st.Kind
		}
		byStage = append(byStage, accStage{Name: name, Docs: st.Docs,
			InMkt: st.InMkt, OutMkt: st.OutMkt, Added: st.Added})
	}
	data["Stages"] = byStage

	feeN, feeSum, err := s.Store.UnmatchedFees()
	if err != nil {
		httpError(w, "fees", err)
		return
	}
	data["FeesUnmatchedN"] = feeN
	data["FeesUnmatched"] = feeSum

	closed := s.Store.ClosedBefore()
	if !closed.IsZero() {
		data["ClosedBefore"] = closed.Format("02.01.2006")
	}
	data["CloseDefault"] = time.Now().AddDate(0, 0, -1).Format("2006-01-02")

	data["Recon"] = recon
	data["ReconChecked"] = sum.Checked
	data["ReconDiffs"] = len(sum.Lines)
	data["ReconTransfers"] = transfers
	data["OnMarketQty"] = sum.OnMarketQty
	data["TransitQty"] = sum.TransitQty
	data["SafetyQty"] = sum.SafetyQty
	data["Message"] = msg
	data["Att"] = att
	data["Rows"] = rows
	data["Stock"] = shelf
	data["StockMore"] = len(stock) - len(shelf)
	data["TotRevenue"] = totRevenue
	data["TotCOGS"] = totCOGS
	data["TotTax"] = totTax
	data["TotProfit"] = totProfit
	data["Opened"] = !s.Store.LedgerEmpty()
	data["EstimateShare"] = share(att.EstimateCost, att.StockCost)
	s.render(w, "accounting", data, stale)
}

func typeName(names map[int64]string, id int64) string {
	if n := names[id]; n != "" {
		return n
	}
	return fmt.Sprintf("Тип %d", id)
}

func locName(names map[int64]string, id int64) string {
	if id == 0 {
		return "—"
	}
	if n := names[id]; n != "" {
		return n
	}
	return fmt.Sprintf("Локация %d", id)
}

func share(part, whole float64) float64 {
	if whole == 0 {
		return 0
	}
	return 100 * part / whole
}
