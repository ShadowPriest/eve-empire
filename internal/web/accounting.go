package web

import (
	"fmt"
	"net/http"
	"time"

	"eve-empire/internal/ledger"
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
	Estimated bool
	Days      int
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
	res, err := ledger.New(s.Store, s.ESI).BuildAll(source)
	msg := res.Note
	if err != nil {
		msg = "ошибка сборки: " + err.Error()
	}
	s.accountingPage(w, r, msg)
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
		shelf = append(shelf, accStock{
			TypeID: st.TypeID, Name: typeName(names, st.TypeID), Where: where,
			Quantity: st.Quantity, Cost: st.Cost, Estimated: st.Estimated,
			Days: int(now.Sub(st.OldestAt).Hours() / 24),
		})
	}

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

func share(part, whole float64) float64 {
	if whole == 0 {
		return 0
	}
	return 100 * part / whole
}
