package ledger

import (
	"fmt"
	"sort"
	"time"

	"eve-empire/internal/esi"
	"eve-empire/internal/store"
)

// layer is one cost slice of an opening pile, oldest first.
type layer struct {
	qty  int64
	unit float64
	at   time.Time
	fact bool // backed by a real purchase, not by a market guess
}

// Opening turns the current asset snapshot into starting lots — the
// "Провести инвентаризацию империи" button of ACCOUNTING.md §7.2.
//
// Two sources of price, in that order:
//
//  1. REAL purchases from the collected window. What is on the shelf today
//     is, under FIFO, whatever was bought most recently — so walking
//     purchases newest-first up to the quantity on hand prices that part
//     by fact, not by guess. This is the difference between "склад стоит
//     примерно столько" and "эта партия обошлась в столько".
//  2. Whatever quantity is left over predates our records and can only be
//     valued at market, and is marked as an estimate for the rest of its
//     life so reports can say how much of the profit rests on guesses.
//
// One document per owner, keyed by owner id: pressing the button twice
// changes nothing.
func (b *Builder) Opening(priceSource string) (Result, error) {
	var res Result
	groups, err := b.Store.AssetGroups()
	if err != nil {
		return res, err
	}
	if len(groups) == 0 {
		res.Note = "снимков имущества ещё нет — сначала должен отработать сбор"
		return res, nil
	}
	// Товар на витрине физически из ангара уехал и в /assets/ его нет
	// (§7.1). Без него инвентаризация теряет реальное имущество.
	onMarket, err := b.Store.MarketGroups()
	if err != nil {
		return res, err
	}
	groups = append(groups, onMarket...)
	prices, err := b.ESI.MarketPrices()
	if err != nil {
		return res, fmt.Errorf("цены: %w", err)
	}
	txs, err := b.Store.Transactions()
	if err != nil {
		return res, err
	}

	// Everything older than our earliest record is "acquired before the
	// books opened": it must be consumed FIRST by FIFO, so it is dated
	// before every known purchase.
	horizon := time.Now().AddDate(0, -1, 0)
	if len(txs) > 0 {
		horizon = txs[0].Date.AddDate(0, 0, -1)
	}

	type ot struct {
		owner  int64
		typeID int64
	}
	buys := map[ot][]store.TxRow{}
	for _, t := range txs {
		if t.IsBuy {
			k := ot{t.OwnerID, t.TypeID}
			buys[k] = append(buys[k], t)
		}
	}
	// newest first: today's shelf is the most recent purchases
	for k := range buys {
		sort.Slice(buys[k], func(i, j int) bool {
			return buys[k][i].Date.After(buys[k][j].Date)
		})
	}

	byOwner := map[int64][]store.AssetGroup{}
	onHand := map[ot]int64{}
	for _, g := range groups {
		byOwner[g.OwnerID] = append(byOwner[g.OwnerID], g)
		onHand[ot{g.OwnerID, g.TypeID}] += g.Quantity
	}

	at := time.Now()
	factLots, estLots := 0, 0
	var factISK, estISK float64

	for ownerID, gs := range byOwner {
		// Deterministic place order so a rebuild allocates identically.
		sort.Slice(gs, func(i, j int) bool {
			if gs[i].TypeID != gs[j].TypeID {
				return gs[i].TypeID < gs[j].TypeID
			}
			if gs[i].LocationID != gs[j].LocationID {
				return gs[i].LocationID < gs[j].LocationID
			}
			return gs[i].HolderID < gs[j].HolderID
		})

		layers := map[ot][]layer{}
		var lines []store.Line
		for _, g := range gs {
			k := ot{ownerID, g.TypeID}
			ls, built := layers[k]
			if !built {
				ls = buildLayers(onHand[k], buys[k], prices[g.TypeID], priceSource, horizon)
				layers[k] = ls
			}
			need := g.Quantity
			for i := range ls {
				if need <= 0 {
					break
				}
				if ls[i].qty <= 0 {
					continue
				}
				take := ls[i].qty
				if take > need {
					take = need
				}
				ls[i].qty -= take
				need -= take

				cost := ls[i].unit * float64(take)
				kind := "estimate"
				if ls[i].fact {
					kind = "fact"
					factLots++
					factISK += cost
				} else {
					estLots++
					estISK += cost
				}
				lines = append(lines, store.Line{
					Place: store.PlaceKey{
						OwnerID: ownerID, LocationID: g.LocationID,
						HolderID: g.HolderID, Flag: g.Flag, Name: g.HolderName,
					},
					TypeID:    g.TypeID,
					Qty:       take,
					At:        ls[i].at,
					CostTotal: cost,
					MktTotal:  priceOf(prices[g.TypeID], priceSource) * float64(take),
					CostKind:  kind,
				})
			}
			layers[k] = ls
		}

		r, err := b.Store.PostDoc(store.Doc{
			Kind: "opening", OwnerID: ownerID, At: at,
			Src: "opening", SrcID: fmt.Sprint(ownerID),
			Note: "инвентаризация империи, цены: " + priceSource,
		}, lines, nil)
		if err != nil {
			return res, err
		}
		if !r.Posted {
			res.Skipped++
			continue
		}
		res.Documents++
		res.Lots += len(lines)
	}

	if res.Documents == 0 && res.Skipped > 0 {
		res.Note = "начальные остатки уже проведены — повторная инвентаризация ничего не меняет"
		return res, nil
	}
	res.Note = fmt.Sprintf("по факту закупа %d партий на %.2f млрд, по оценке %d на %.2f млрд",
		factLots, factISK/1e9, estLots, estISK/1e9)
	return res, nil
}

// buildLayers prices one (owner, type) pile: recent purchases first, the
// unexplained remainder at market.
//
// GRABLE: the layers a lot is FIFO-consumed in must come out oldest first,
// while purchases are walked newest first — hence the reversal. Getting
// this backwards would make the ledger spend the freshest goods first and
// quietly invert every margin.
func buildLayers(onHand int64, purchases []store.TxRow, price esi.MarketPrice,
	source string, horizon time.Time) []layer {

	if onHand <= 0 {
		return nil
	}
	var covered int64
	var out []layer
	for _, p := range purchases {
		if covered >= onHand {
			break
		}
		take := p.Quantity
		if covered+take > onHand {
			take = onHand - covered
		}
		out = append(out, layer{qty: take, unit: p.UnitPrice, at: p.Date, fact: true})
		covered += take
	}
	// newest-first → oldest-first
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	if rest := onHand - covered; rest > 0 {
		out = append([]layer{{
			qty: rest, unit: priceOf(price, source), at: horizon, fact: false,
		}}, out...)
	}
	return out
}
