// Package ledger turns collected ESI history into accounting documents.
//
// Stage 1 of ACCOUNTING.md: the opening inventory, and purchases and sales
// from wallet transactions. Everything here is idempotent — a document is
// keyed by its ESI id, so running a builder twice changes nothing and the
// whole ledger can be rebuilt from hist_* at any time.
package ledger

import (
	"fmt"
	"log"
	"time"

	"eve-empire/internal/esi"
	"eve-empire/internal/store"
)

type Builder struct {
	Store *store.Store
	ESI   *esi.Client
}

func New(st *store.Store, ec *esi.Client) *Builder { return &Builder{Store: st, ESI: ec} }

// Result is what a build pass did, for the page that triggered it.
type Result struct {
	Documents int
	Lots      int
	Skipped   int
	Shortfall int64
	Note      string
}

func priceOf(p esi.MarketPrice, source string) float64 {
	if source == "adjusted" {
		return p.Adjusted
	}
	return p.Average
}

// ── торговля ─────────────────────────────────────────────────────────

// Trades posts purchases and sales from collected wallet transactions,
// oldest first — the order matters, because FIFO must not consume a lot
// that did not exist yet.
//
// The sales tax of each sale is attached exactly: the journal carries a
// market_transaction line whose context_id IS the transaction id, and the
// matching transaction_tax line sits within a few journal ids of it
// (ПРОВЕРЕНО — all 102 sales of the sample matched within ±3). Pro-rata
// allocation would have been wrong: the tax rate differs per character
// (4.2 %, 3.375 % and 5.025 % were all present).
func (b *Builder) Trades() (Result, error) {
	var res Result
	txs, err := b.Store.Transactions()
	if err != nil {
		return res, err
	}
	if len(txs) == 0 {
		res.Note = "транзакций пока не собрано"
		return res, nil
	}
	done, err := b.Store.PostedSrcIDs("esi:transaction")
	if err != nil {
		return res, err
	}
	prices, err := b.ESI.MarketPrices()
	if err != nil {
		// Not fatal: prices only value goods the ledger never saw arrive.
		log.Printf("реестр: цены недоступны, нехватка будет оценена в 0: %v", err)
		prices = map[int64]esi.MarketPrice{}
	}
	taxOf, err := b.salesTax()
	if err != nil {
		return res, err
	}
	// Books opened today: the collected window predates the inventory, and
	// those goods are already priced INTO it (see Opening). Posting them
	// again would double the stock and make every past sale eat lots that,
	// in ledger time, did not exist yet.
	openedAt, err := b.Store.OpeningTimes()
	if err != nil {
		return res, err
	}
	// An owner with transactions but no assets of its own still shares the
	// moment the books were opened; without this fallback its whole history
	// would post as if the inventory had never happened.
	var opened time.Time
	for _, t := range openedAt {
		if opened.IsZero() || t.Before(opened) {
			opened = t
		}
	}
	before := 0

	for _, t := range txs {
		key := fmt.Sprint(t.TransactionID)
		if done[key] {
			res.Skipped++
			continue
		}
		o, ok := openedAt[t.OwnerID]
		if !ok {
			o = opened
		}
		if !o.IsZero() && t.Date.Before(o) {
			before++
			continue
		}
		gross := t.UnitPrice * float64(t.Quantity)
		place := store.PlaceKey{OwnerID: t.OwnerID, LocationID: t.LocationID, Flag: "Hangar"}

		var line store.Line
		var cash []store.CashLine
		kind := "sale"
		if t.IsBuy {
			kind = "purchase"
			line = store.Line{Place: place, TypeID: t.TypeID, Qty: t.Quantity,
				CostTotal: gross, MktTotal: gross}
			cash = []store.CashLine{{OwnerID: t.OwnerID, Division: t.Division,
				Kind: "cost", Amount: -gross}}
		} else {
			// Goods sold off a market order left the hangar when the order
			// was placed; stage 1 does not model that escrow, so the issue
			// is allowed to look wider than the exact place.
			line = store.Line{Place: place, TypeID: t.TypeID, Qty: -t.Quantity,
				ShortfallUnitCost: prices[t.TypeID].Average}
			cash = []store.CashLine{{OwnerID: t.OwnerID, Division: t.Division,
				Kind: "revenue", Amount: gross}}
			if tax := taxOf[t.TransactionID]; tax != 0 {
				cash = append(cash, store.CashLine{OwnerID: t.OwnerID,
					Division: t.Division, Kind: "sales_tax", Amount: tax})
			}
		}

		r, err := b.Store.PostDoc(store.Doc{
			Kind: kind, OwnerID: t.OwnerID, At: t.Date,
			Src: "esi:transaction", SrcID: key,
		}, []store.Line{line}, cash)
		if err != nil {
			return res, fmt.Errorf("транзакция %d: %w", t.TransactionID, err)
		}
		if !r.Posted {
			res.Skipped++
			continue
		}
		res.Documents++
		res.Shortfall += r.Shortfall
	}
	if before > 0 {
		res.Note = fmt.Sprintf("%d сделок старше инвентаризации — они уже учтены в остатках", before)
	}
	return res, nil
}

// salesTax maps a transaction id to its (negative) tax amount.
func (b *Builder) salesTax() (map[int64]float64, error) {
	lines, err := b.Store.JournalOfTypes("market_transaction", "transaction_tax")
	if err != nil {
		return nil, err
	}
	type key struct {
		owner int64
		sec   int64
	}
	taxes := map[key][]store.JournalRow{}
	var sales []store.JournalRow
	for _, l := range lines {
		switch l.RefType {
		case "transaction_tax":
			k := key{l.OwnerID, l.Date.Unix()}
			taxes[k] = append(taxes[k], l)
		case "market_transaction":
			if l.Amount > 0 { // только продажи облагаются налогом
				sales = append(sales, l)
			}
		}
	}

	out := map[int64]float64{}
	used := map[int64]bool{}
	for _, s := range sales {
		if s.ContextIDType != "market_transaction_id" || s.ContextID == 0 {
			continue
		}
		k := key{s.OwnerID, s.Date.Unix()}
		best, bestGap := int64(-1), int64(1<<62)
		var amount float64
		for _, t := range taxes[k] {
			if used[t.ID] {
				continue
			}
			gap := t.ID - s.ID
			if gap < 0 {
				gap = -gap
			}
			if gap < bestGap {
				best, bestGap, amount = t.ID, gap, t.Amount
			}
		}
		if best >= 0 {
			used[best] = true
			out[s.ContextID] = amount
		}
	}
	return out, nil
}

// ── брокерские сборы ─────────────────────────────────────────────────

// BrokerFees posts each broker fee as its own document.
//
// It is NOT folded into the cost of the goods, and that is a deliberate
// stage-1 simplification: the fee is charged when an order is PLACED, the
// journal line carries no context at all, and a transaction never names
// the order it filled. So there is no path from a fee to the units it
// belongs to. What can be recovered is which ORDER it was — the fee and
// the order share a timestamp — and that goes into the note.
//
// Consequence: the buy-side fee stays a period expense instead of being
// capitalised as ACCOUNTING.md §8 prescribes. Total profit is unaffected;
// only the split between "cost of stock" and "expense" moves.
func (b *Builder) BrokerFees() (Result, error) {
	var res Result
	fees, err := b.Store.JournalOfTypes("brokers_fee")
	if err != nil {
		return res, err
	}
	if len(fees) == 0 {
		return res, nil
	}
	orders, err := b.Store.Orders()
	if err != nil {
		return res, err
	}
	done, err := b.Store.PostedSrcIDs("esi:brokers_fee")
	if err != nil {
		return res, err
	}

	matched := 0
	for _, f := range fees {
		key := fmt.Sprint(f.ID)
		if done[key] {
			res.Skipped++
			continue
		}
		note := "сбор не сопоставлен с ордером"
		var bestGap = int64(1 << 62)
		for _, o := range orders {
			if o.OwnerID != f.OwnerID || o.Issued.IsZero() {
				continue
			}
			gap := o.Issued.Unix() - f.Date.Unix()
			if gap < 0 {
				gap = -gap
			}
			if gap < bestGap {
				bestGap = gap
				side := "продажа"
				if o.IsBuy {
					side = "покупка"
				}
				note = fmt.Sprintf("ордер %d (%s), тип %d", o.OrderID, side, o.TypeID)
			}
		}
		if bestGap > 5 { // дальше пяти секунд совпадение уже случайно
			note = "сбор не сопоставлен с ордером"
		} else {
			matched++
		}
		r, err := b.Store.PostDoc(store.Doc{
			Kind: "fee", OwnerID: f.OwnerID, At: f.Date,
			Src: "esi:brokers_fee", SrcID: key, Note: note,
		}, nil, []store.CashLine{{OwnerID: f.OwnerID, Division: f.Division,
			Kind: "broker_fee", Amount: f.Amount}})
		if err != nil {
			return res, err
		}
		if r.Posted {
			res.Documents++
		}
	}
	switch {
	case res.Documents == 0 && res.Skipped > 0:
		res.Note = fmt.Sprintf("брокерские сборы уже проведены (%d)", res.Skipped)
	default:
		res.Note = fmt.Sprintf("брокерских сборов %d, сопоставлено с ордером %d",
			res.Documents, matched)
	}
	return res, nil
}

// BuildAll runs the stage-1 builders in the order they depend on each
// other: stock must exist before a sale can consume it.
func (b *Builder) BuildAll(priceSource string) (Result, error) {
	var total Result
	open, err := b.Opening(priceSource)
	if err != nil {
		return total, err
	}
	trades, err := b.Trades()
	if err != nil {
		return total, err
	}
	fees, err := b.BrokerFees()
	if err != nil {
		return total, err
	}
	total.Documents = open.Documents + trades.Documents + fees.Documents
	total.Lots = open.Lots
	total.Skipped = open.Skipped + trades.Skipped + fees.Skipped
	total.Shortfall = trades.Shortfall
	total.Note = fmt.Sprintf("остатки: %s; сделки: документов %d, нехватка %d ед.; %s",
		firstNonEmpty(open.Note, fmt.Sprintf("партий %d", open.Lots)),
		trades.Documents, trades.Shortfall, fees.Note)
	return total, nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
