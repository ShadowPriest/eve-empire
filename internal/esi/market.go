package esi

// Regional order-book statistics — the price side of the ore table.
//
// ESI hands out the raw order book and nothing else: no "the price of
// Veldspar" endpoint exists. The lowest sell and highest buy are the
// direct answer but a single 1-unit troll order moves them, so we also
// keep two volume-weighted trims: the 98th and 90th percentile drop the
// cheapest 2 % / 10 % of sell volume (and the priciest 2 % / 10 % of buy
// volume) before taking the extreme. That is the same reduction
// ore.cerlestes.de publishes, and on a thin market the trimmed pair is
// the only one worth reading.

import (
	"fmt"
	"sort"
	"sync"
	"time"
)

// RegionPLEX is the global PLEX market. PLEX is not traded regionally
// at all: its orders and history live in this pseudo-region, and asking
// The Forge for them returns an empty book.
const RegionPLEX = 19000001

// Price statistics the table can be rendered with.
const (
	StatSellMin = "smin"
	StatSellP98 = "sp98"
	StatSellP90 = "sp90"
	StatBuyMax  = "bmax"
	StatBuyP98  = "bp98"
	StatBuyP90  = "bp90"
)

// OrderStats reduces one type's order book in one region to the numbers
// the tables display.
type OrderStats struct {
	SellMin, SellP98, SellP90 float64
	BuyMax, BuyP98, BuyP90    float64
	SellOrders, BuyOrders     int
	SellVolume, BuyVolume     int64
	Fetched                   time.Time
}

// Pick returns the statistic by its key (see the Stat* constants).
func (s OrderStats) Pick(stat string) float64 {
	switch stat {
	case StatSellMin:
		return s.SellMin
	case StatSellP90:
		return s.SellP90
	case StatBuyMax:
		return s.BuyMax
	case StatBuyP98:
		return s.BuyP98
	case StatBuyP90:
		return s.BuyP90
	default:
		return s.SellP98
	}
}

// IsBuy reports whether the statistic comes from the buy side.
func IsBuy(stat string) bool {
	return stat == StatBuyMax || stat == StatBuyP98 || stat == StatBuyP90
}

type bookOrder struct {
	Price        float64 `json:"price"`
	VolumeRemain int64   `json:"volume_remain"`
	IsBuyOrder   bool    `json:"is_buy_order"`
}

// RegionOrderStats reads the whole order book of one type in one region
// and reduces it. Public route, ESI caches it for 5 minutes and so do we.
func (c *Client) RegionOrderStats(regionID, typeID int64) (OrderStats, error) {
	var all []bookOrder
	page := 1
	for {
		var chunk []bookOrder
		pages, err := c.get(0, fmt.Sprintf(
			"/markets/%d/orders/?order_type=all&type_id=%d&page=%d", regionID, typeID, page), &chunk)
		if err != nil {
			return OrderStats{}, err
		}
		all = append(all, chunk...)
		if page >= pages || len(chunk) == 0 {
			break
		}
		page++
	}

	var sells, buys []bookOrder
	st := OrderStats{Fetched: time.Now()}
	for _, o := range all {
		if o.VolumeRemain <= 0 {
			continue
		}
		if o.IsBuyOrder {
			buys = append(buys, o)
			st.BuyOrders++
			st.BuyVolume += o.VolumeRemain
		} else {
			sells = append(sells, o)
			st.SellOrders++
			st.SellVolume += o.VolumeRemain
		}
	}
	sort.Slice(sells, func(i, j int) bool { return sells[i].Price < sells[j].Price })
	sort.Slice(buys, func(i, j int) bool { return buys[i].Price > buys[j].Price })

	st.SellMin = trim(sells, 0)
	st.SellP98 = trim(sells, 0.02)
	st.SellP90 = trim(sells, 0.10)
	st.BuyMax = trim(buys, 0)
	st.BuyP98 = trim(buys, 0.02)
	st.BuyP90 = trim(buys, 0.10)
	return st, nil
}

// trim walks the book (already sorted from the most attractive price
// outwards), skips the given share of the total volume and returns the
// price it lands on. share == 0 is the plain best price.
func trim(orders []bookOrder, share float64) float64 {
	if len(orders) == 0 {
		return 0
	}
	if share <= 0 {
		return orders[0].Price
	}
	var total int64
	for _, o := range orders {
		total += o.VolumeRemain
	}
	cut := float64(total) * share
	var seen float64
	for _, o := range orders {
		seen += float64(o.VolumeRemain)
		if seen > cut {
			return o.Price
		}
	}
	return orders[len(orders)-1].Price
}

// RegionOrder is one live order of the full region book — the orders
// tool shows them row by row, so unlike bookOrder this keeps identity
// (order_id matches against the pilots' own orders) and location.
type RegionOrder struct {
	OrderID      int64     `json:"order_id"`
	Price        float64   `json:"price"`
	VolumeRemain int64     `json:"volume_remain"`
	VolumeTotal  int64     `json:"volume_total"`
	MinVolume    int64     `json:"min_volume"`
	IsBuyOrder   bool      `json:"is_buy_order"`
	LocationID   int64     `json:"location_id"`
	SystemID     int64     `json:"system_id"`
	Range        string    `json:"range"`
	Issued       time.Time `json:"issued"`
	Duration     int       `json:"duration"` // days
}

// RegionOrders reads the complete order book of one type in one region.
// Public route, cached like everything else (5 minutes per ESI).
func (c *Client) RegionOrders(regionID, typeID int64) ([]RegionOrder, error) {
	var all []RegionOrder
	page := 1
	for {
		var chunk []RegionOrder
		pages, err := c.get(0, fmt.Sprintf(
			"/markets/%d/orders/?order_type=all&type_id=%d&page=%d", regionID, typeID, page), &chunk)
		if err != nil {
			return nil, err
		}
		all = append(all, chunk...)
		if page >= pages || len(chunk) == 0 {
			break
		}
		page++
	}
	return all, nil
}

// RegionHistoryFull returns the raw daily history rows of one type —
// average/min/max, volume and order count. RegionHistory keeps only the
// averages; the market browser's history tab wants the whole candle.
func (c *Client) RegionHistoryFull(regionID, typeID int64) ([]HistoryDay, error) {
	var raw []HistoryDay
	if _, err := c.get(0,
		fmt.Sprintf("/markets/%d/history/?type_id=%d", regionID, typeID), &raw); err != nil {
		return nil, err
	}
	sort.Slice(raw, func(i, j int) bool { return raw[i].Date < raw[j].Date })
	return raw, nil
}

// RegionOrderBook fetches many types at once. Errors are swallowed per
// type: on the stale view nothing is cached yet and the caller renders
// dashes until the page revalidates.
func (c *Client) RegionOrderBook(regionID int64, typeIDs []int64) map[int64]OrderStats {
	out := make(map[int64]OrderStats, len(typeIDs))
	var mu sync.Mutex
	sem := make(chan struct{}, 16)
	var wg sync.WaitGroup
	for _, id := range dedupe(typeIDs) {
		wg.Add(1)
		go func(id int64) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			st, err := c.RegionOrderStats(regionID, id)
			if err != nil {
				return
			}
			mu.Lock()
			out[id] = st
			mu.Unlock()
		}(id)
	}
	wg.Wait()
	return out
}
