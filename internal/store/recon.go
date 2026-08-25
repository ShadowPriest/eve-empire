package store

import "time"

// Сверка реестра с действительностью (ACCOUNTING.md §7).
//
// "Всё имущество" is NOT the same thing as /assets/. Goods sitting on a
// sell order left the hangar when the order was placed, goods under a
// courier wrap are held by the hauler rather than owned by them, and
// materials inside a running job are nowhere at all. The balance only
// closes when each of those is counted on the right side:
//
//	ассеты (без груза под обёрткой) + витрина  ==  остаток реестра − НЗП
//
// Stage 2 covers assets and the market. Contracts, colonies and WIP are
// still missing and are reported as caveats rather than silently ignored.

const (
	typePlasticWrap = 3468 // груз принятой курьерки: лежит у ПЕРЕВОЗЧИКА
	typeAssetSafety = 60   // asset safety: имущество своё, просто забытое
)

// ReconLine is one (owner, location, type) that does not add up.
type ReconLine struct {
	OwnerID    int64
	LocationID int64
	TypeID     int64
	InAssets   int64 // лежит в ангарах, контейнерах, трюмах
	OnMarket   int64 // выставлено своим ордером продажи
	InTransit  int64 // под чужой Plastic Wrap — числится за перевозчиком
	InSafety   int64 // под Asset Safety Wrap, часть InAssets
	Ledger     int64 // сколько думает реестр
	Diff       int64 // InAssets + OnMarket − Ledger
}

// Surplus reports goods reality has and the ledger does not.
func (r ReconLine) Surplus() bool { return r.Diff > 0 }

// ReconSummary is what one reconciliation pass found.
type ReconSummary struct {
	Lines       []ReconLine
	Checked     int // всего сопоставленных (владелец, локация, тип)
	OnMarketQty int64
	TransitQty  int64
	SafetyQty   int64
	NoAssets    bool // снимков ещё нет — сверять не с чем
}

// Reconcile compares the ledger against the current asset snapshot plus
// what is on the market. It returns only the lines that disagree.
func (s *Store) Reconcile() (ReconSummary, error) {
	var sum ReconSummary

	type key struct{ owner, location, typeID int64 }
	actual := map[key]*ReconLine{}
	at := func(k key) *ReconLine {
		if l, ok := actual[k]; ok {
			return l
		}
		l := &ReconLine{OwnerID: k.owner, LocationID: k.location, TypeID: k.typeID}
		actual[k] = l
		return l
	}

	// ── ассеты, с разбором цепочки родителей ──
	type item struct {
		owner, typeID, qty, root, parent int64
	}
	rows, err := s.db.Query(`SELECT owner_id, item_id, type_id, quantity, root_id,
		parent_item_id FROM asset_state`)
	if err != nil {
		return sum, err
	}
	items := map[int64]item{} // item_id → строка (item_id уникален по всей игре)
	var order []int64
	for rows.Next() {
		var id int64
		var it item
		if err := rows.Scan(&it.owner, &id, &it.typeID, &it.qty, &it.root, &it.parent); err != nil {
			rows.Close()
			return sum, err
		}
		items[id] = it
		order = append(order, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return sum, err
	}
	if len(items) == 0 {
		sum.NoAssets = true
		return sum, nil
	}

	// GRABLE: "лежит в обёртке" определяется только цепочкой родителей —
	// содержимое контейнера несёт location_flag=AutoFit, а не Cargo, и по
	// флагу вложенность не восстановить (ПРОВЕРЕНО, §4.6).
	under := func(id int64, wrapType int64) bool {
		cur := items[id].parent
		for depth := 0; depth < 32 && cur != 0; depth++ {
			p, ok := items[cur]
			if !ok {
				return false
			}
			if p.typeID == wrapType {
				return true
			}
			cur = p.parent
		}
		return false
	}

	for _, id := range order {
		it := items[id]
		k := key{it.owner, it.root, it.typeID}
		l := at(k)
		switch {
		case under(id, typePlasticWrap):
			// Груз чужого контракта: физически здесь, принадлежит другому.
			l.InTransit += it.qty
			sum.TransitQty += it.qty
		default:
			l.InAssets += it.qty
			if under(id, typeAssetSafety) {
				l.InSafety += it.qty
				sum.SafetyQty += it.qty
			}
		}
	}

	// ── витрина: свои ордера продажи ──
	orders, err := s.db.Query(`SELECT owner_id, location_id, type_id, volume_remain
		FROM hist_order WHERE is_buy = 0 AND state = 'open' AND volume_remain > 0`)
	if err != nil {
		return sum, err
	}
	for orders.Next() {
		var owner, loc, typeID, qty int64
		if err := orders.Scan(&owner, &loc, &typeID, &qty); err != nil {
			orders.Close()
			return sum, err
		}
		at(key{owner, loc, typeID}).OnMarket += qty
		sum.OnMarketQty += qty
	}
	orders.Close()
	if err := orders.Err(); err != nil {
		return sum, err
	}

	// ── остаток реестра ──
	led, err := s.db.Query(`SELECT l.owner_id, p.location_id, l.type_id, SUM(l.qty_left)
		FROM acc_lot l JOIN acc_place p ON p.id = l.place_id
		WHERE l.qty_left > 0
		GROUP BY l.owner_id, p.location_id, l.type_id`)
	if err != nil {
		return sum, err
	}
	for led.Next() {
		var owner, loc, typeID, qty int64
		if err := led.Scan(&owner, &loc, &typeID, &qty); err != nil {
			led.Close()
			return sum, err
		}
		at(key{owner, loc, typeID}).Ledger += qty
	}
	led.Close()
	if err := led.Err(); err != nil {
		return sum, err
	}

	sum.Checked = len(actual)
	for _, l := range actual {
		l.Diff = l.InAssets + l.OnMarket - l.Ledger
		if l.Diff != 0 {
			sum.Lines = append(sum.Lines, *l)
		}
	}
	return sum, nil
}

// ── документы сверки ─────────────────────────────────────────────────

// LotChoice is one live lot offered to a human deciding what to write off
// or move.
type LotChoice struct {
	LotID     int64
	PlaceID   int64
	PlaceName string
	Quantity  int64
	Cost      float64
	Estimated bool
	At        time.Time
}

// LiveLots lists what the ledger still holds of one type at one location,
// oldest first — the order a write-off consumes them in by default.
func (s *Store) LiveLots(ownerID, locationID, typeID int64) ([]LotChoice, error) {
	rows, err := s.db.Query(`SELECT l.id, l.place_id, p.name, l.qty_left, l.cost_left,
		l.cost_kind, l.at
		FROM acc_lot l JOIN acc_place p ON p.id = l.place_id
		WHERE l.qty_left > 0 AND l.owner_id = ? AND p.location_id = ? AND l.type_id = ?
		ORDER BY l.at, l.id`, ownerID, locationID, typeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []LotChoice
	for rows.Next() {
		var c LotChoice
		var kind string
		var at int64
		if err := rows.Scan(&c.LotID, &c.PlaceID, &c.PlaceName, &c.Quantity,
			&c.Cost, &kind, &at); err != nil {
			return nil, err
		}
		c.Estimated, c.At = kind == "estimate", time.Unix(at, 0)
		if c.PlaceName == "" {
			c.PlaceName = "ангар"
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// DeadStockLine is one pile that is not moving.
type DeadStockLine struct {
	OwnerID    int64
	LocationID int64
	PlaceName  string
	TypeID     int64
	Quantity   int64
	Cost       float64
	Estimated  bool
	Days       int
}

// DeadStock lists piles that have not been touched for at least the given
// number of days, dearest first. Answering "где валяется барахло" needs
// both halves: how much ISK is frozen, and how long it has been frozen.
func (s *Store) DeadStock(minDays int) ([]DeadStockLine, error) {
	cutoff := time.Now().AddDate(0, 0, -minDays).Unix()
	rows, err := s.db.Query(`
SELECT l.owner_id, p.location_id, p.name, l.type_id,
       SUM(l.qty_left), SUM(l.cost_left),
       MAX(CASE WHEN l.cost_kind = 'estimate' THEN 1 ELSE 0 END), MIN(l.at)
FROM acc_lot l JOIN acc_place p ON p.id = l.place_id
WHERE l.qty_left > 0 AND l.at <= ?
GROUP BY l.owner_id, p.location_id, p.name, l.type_id
ORDER BY SUM(l.cost_left) DESC`, cutoff)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	now := time.Now()
	var out []DeadStockLine
	for rows.Next() {
		var d DeadStockLine
		var est int
		var oldest int64
		if err := rows.Scan(&d.OwnerID, &d.LocationID, &d.PlaceName, &d.TypeID,
			&d.Quantity, &d.Cost, &est, &oldest); err != nil {
			return nil, err
		}
		d.Estimated = est == 1
		d.Days = int(now.Sub(time.Unix(oldest, 0)).Hours() / 24)
		if d.PlaceName == "" {
			d.PlaceName = "ангар"
		}
		out = append(out, d)
	}
	return out, rows.Err()
}
