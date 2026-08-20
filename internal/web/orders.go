// The orders tool — the in-game market browser: the market-group tree
// with the goods as leaves, and per type two tabs, «Цены» (the live
// Jita book, the pilots' own orders highlighted) and «История цен»
// (the daily candle chart or its table). Row selection and its running
// totals are pure JS in layout.html — the server only marks ownership.
package web

import (
	"fmt"
	"html/template"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"eve-empire/internal/esi"
	"eve-empire/internal/sde"
)

// orderBookRow is one order of the region book, ready to render.
type orderBookRow struct {
	esi.RegionOrder
	LocationName string
	RangeLabel   string // buy orders only: how far the order reaches
	Expires      string // sell orders: time left, like the game shows
	Mine         string // owning pilot's name, "" for foreign orders
}

// ownOrderRow is one of the pilots' own orders on the picked type —
// shown separately because it may live outside The Forge and then
// never appears in the book below.
type ownOrderRow struct {
	esi.MarketOrder
	CharName string
}

// histPeriod is one entry of the history range picker.
type histPeriod struct {
	Days  int
	Label string
}

var histPeriods = []histPeriod{
	{30, "1 мес"}, {91, "3 мес"}, {182, "6 мес"}, {365, "1 год"},
}

func (s *Server) handleOrdersTool(w http.ResponseWriter, r *http.Request) {
	ec, stale := s.esiFor(r)
	data, _, err := s.shell(ec, 0, "")
	if err != nil {
		httpError(w, "loading characters", err)
		return
	}
	var errs errList

	q := r.URL.Query().Get("q")
	typeID, _ := strconv.ParseInt(r.URL.Query().Get("t"), 10, 64)
	groupID, _ := strconv.ParseInt(r.URL.Query().Get("g"), 10, 64)
	view := r.URL.Query().Get("v") // "" = prices, "hist" = price history
	histDays, _ := strconv.Atoi(r.URL.Query().Get("hd"))
	if histDays == 0 {
		histDays = 91
	}
	histTable := r.URL.Query().Get("hv") == "t"
	data["Query"] = q
	data["NoSDE"] = !s.SDE.Available()

	if q != "" {
		found := s.SDE.SearchMarketTypes(q, 30)
		// A single hit needs no second click.
		if typeID == 0 && len(found) == 1 {
			typeID = found[0].TypeID
		}
		data["Hits"] = found
	}
	data["TypeID"] = typeID

	// The tree opens along the path to the picked group; a type picked
	// through the search opens the tree at its own folder. The goods of
	// the picked group sit in the tree as leaves, like the game does it.
	if typeID != 0 && groupID == 0 {
		groupID = s.SDE.TypeMarketGroup(typeID)
	}
	open := map[int64]bool{}
	for _, id := range s.SDE.MarketGroupPath(groupID) {
		open[id] = true
	}
	var groupTypes []sde.MarketType
	if groupID != 0 {
		groupTypes = s.SDE.MarketGroupTypes(groupID)
		data["GroupName"] = s.SDE.MarketGroupName(groupID)
	}
	data["TreeHTML"] = renderMarketTree(s.SDE.MarketGroups(), open, groupID, groupTypes, typeID)
	data["GroupID"] = groupID

	// Breadcrumbs over the item header: Корабли / Буровые корабли / …
	type crumb struct {
		ID   int64
		Name string
	}
	var crumbs []crumb
	for _, id := range s.SDE.MarketGroupPath(groupID) {
		crumbs = append(crumbs, crumb{id, s.SDE.MarketGroupName(id)})
	}
	data["Crumbs"] = crumbs

	data["View"] = view
	data["HistDays"] = histDays
	data["HistTable"] = histTable
	data["HistPeriods"] = histPeriods

	if typeID != 0 {
		names := s.SDE.TypeNames([]int64{typeID})
		data["TypeName"] = names[typeID]

		// PLEX is not traded regionally — its book lives in the global
		// pseudo-region and The Forge returns an empty answer.
		region := int64(esi.RegionTheForge)
		global := s.SDE.TypeIDByName("PLEX") == typeID
		if global {
			region = esi.RegionPLEX
		}
		data["Global"] = global

		if view == "hist" {
			hist, err := ec.RegionHistoryFull(region, typeID)
			if err != nil {
				errs.add("история", err)
			}
			if len(hist) > histDays {
				hist = hist[len(hist)-histDays:]
			}
			data["Hist"] = hist
			if !histTable && len(hist) > 0 {
				data["HistSVG"] = renderHistoryChart(hist)
			}
		} else {
			s.ordersBook(ec, data, &errs, region, typeID)
		}
	}

	data["Errors"] = errs.list
	s.render(w, "orders", data, stale)
}

// ordersBook fills the «Цены» tab: the full region book split into
// sellers and buyers, the pilots' own orders marked and listed.
func (s *Server) ordersBook(ec *esi.Client, data map[string]any, errs *errList, region, typeID int64) {
	book, err := ec.RegionOrders(region, typeID)
	if err != nil {
		errs.add("ордербук", err)
	}

	// Own orders of every pilot, in parallel: order_id → pilot maps
	// the book rows, the per-type filter fills the "своя" table.
	chars, _ := s.Store.Characters()
	mine := map[int64]string{}
	var own []ownOrderRow
	var firstChar int64
	if len(chars) > 0 {
		firstChar = chars[0].ID
	}
	perChar := make([][]esi.MarketOrder, len(chars))
	var wg sync.WaitGroup
	for i, ch := range chars {
		wg.Add(1)
		go func(i int, charID int64) {
			defer wg.Done()
			// Errors are per-pilot noise (no scope, empty cache on
			// the stale view) — the page renders without that pilot.
			if orders, err := ec.MarketOrders(charID); err == nil {
				perChar[i] = orders
			}
		}(i, ch.ID)
	}
	wg.Wait()
	for i, ch := range chars {
		for _, o := range perChar[i] {
			mine[o.OrderID] = ch.Name
			if o.TypeID == typeID {
				own = append(own, ownOrderRow{MarketOrder: o, CharName: ch.Name})
			}
		}
	}
	sort.Slice(own, func(i, j int) bool {
		if own[i].IsBuyOrder != own[j].IsBuyOrder {
			return !own[i].IsBuyOrder
		}
		return own[i].Price < own[j].Price
	})
	data["Own"] = own

	// Stations resolve through the public batch, structures need the
	// authorized per-structure endpoint — LocationNames splits them.
	var locIDs []int64
	for _, o := range book {
		locIDs = append(locIDs, o.LocationID)
	}
	locNames := ec.LocationNames(firstChar, locIDs)

	now := time.Now()
	var sells, buys []orderBookRow
	var sellVol, buyVol int64
	for _, o := range book {
		if o.VolumeRemain <= 0 {
			continue
		}
		row := orderBookRow{
			RegionOrder:  o,
			LocationName: locNames[o.LocationID],
			Mine:         mine[o.OrderID],
		}
		if o.IsBuyOrder {
			row.RangeLabel = rangeLabel(o.Range)
			buys = append(buys, row)
			buyVol += o.VolumeRemain
		} else {
			row.Expires = humanUntil(o.Issued.AddDate(0, 0, o.Duration), now)
			sells = append(sells, row)
			sellVol += o.VolumeRemain
		}
	}
	sort.Slice(sells, func(i, j int) bool { return sells[i].Price < sells[j].Price })
	sort.Slice(buys, func(i, j int) bool { return buys[i].Price > buys[j].Price })

	data["Sells"] = sells
	data["Buys"] = buys
	data["SellVolume"] = sellVol
	data["BuyVolume"] = buyVol
	var bestSell, bestBuy float64
	if len(sells) > 0 {
		bestSell = sells[0].Price
	}
	if len(buys) > 0 {
		bestBuy = buys[0].Price
	}
	data["BestSell"] = bestSell
	data["BestBuy"] = bestBuy
}

// renderMarketTree builds the market-browser tree as nested lists.
// Rendered in Go, not the template: the recursion needs the open-path
// and selection alongside every node, and templates can't carry that
// many values down a {{template}} call. The picked group additionally
// gets its goods as leaves, the way the game's tree shows them.
func renderMarketTree(roots []*sde.MarketGroupNode, open map[int64]bool,
	sel int64, selTypes []sde.MarketType, selType int64) template.HTML {

	var b strings.Builder
	b.WriteString("<ul>")
	writeMarketNodes(&b, roots, open, sel, selTypes, selType)
	b.WriteString("</ul>")
	return template.HTML(b.String())
}

func writeMarketNodes(b *strings.Builder, list []*sde.MarketGroupNode,
	open map[int64]bool, sel int64, selTypes []sde.MarketType, selType int64) {

	for _, n := range list {
		isSel := n.ID == sel
		hasKids := len(n.Children) > 0 || (isSel && len(selTypes) > 0)
		b.WriteString("<li")
		if open[n.ID] {
			b.WriteString(` class="open"`)
		}
		b.WriteString(">")
		if hasKids {
			b.WriteString(`<i class="mgarr"></i>`)
		} else {
			b.WriteString(`<i class="mgsp"></i>`)
		}
		name := template.HTMLEscapeString(n.Name)
		cls := "mgn"
		if isSel {
			cls += " active"
		}
		if n.HasTypes {
			fmt.Fprintf(b, `<a class="%s" href="/tools/orders?g=%d">%s</a>`, cls, n.ID, name)
		} else {
			// A pure folder only expands — nothing to list on the server.
			fmt.Fprintf(b, `<span class="%s">%s</span>`, cls, name)
		}
		if hasKids {
			b.WriteString("<ul>")
			writeMarketNodes(b, n.Children, open, sel, selTypes, selType)
			if isSel {
				// Subgroups first, then the goods — the game's order.
				for _, t := range selTypes {
					tcls := "mgn mgt"
					if t.TypeID == selType {
						tcls += " active"
					}
					fmt.Fprintf(b, `<li><i class="mgsp"></i><a class="%s" href="/tools/orders?g=%d&t=%d">%s</a></li>`,
						tcls, sel, t.TypeID, template.HTMLEscapeString(t.Name))
				}
			}
			b.WriteString("</ul>")
		}
		b.WriteString("</li>")
	}
}

// rangeLabel translates the reach of a buy order the way the client
// words it.
func rangeLabel(r string) string {
	switch r {
	case "station":
		return "станция"
	case "solarsystem":
		return "система"
	case "region":
		return "регион"
	case "1":
		return "1 переход"
	case "":
		return ""
	}
	return r + " переходов"
}
