package store

import "time"

// Reads for the ledger builders and reports.

// AssetGroup is one (owner, place, type) pile of the current asset state —
// the unit the opening inventory turns into a lot.
type AssetGroup struct {
	OwnerID    int64
	LocationID int64
	HolderID   int64
	Flag       string
	HolderName string
	TypeID     int64
	Quantity   int64
}

// AssetGroups collapses asset_state into piles. Nothing is dropped: a
// forgotten Asset Safety Wrap holds real goods, and so does a ship hold.
func (s *Store) AssetGroups() ([]AssetGroup, error) {
	rows, err := s.db.Query(`
SELECT a.owner_id, a.root_id, a.parent_item_id, a.location_flag,
       COALESCE(h.name, ''), a.type_id, SUM(a.quantity)
FROM asset_state a
LEFT JOIN asset_state h ON h.owner_id = a.owner_id AND h.item_id = a.parent_item_id
GROUP BY a.owner_id, a.root_id, a.parent_item_id, a.location_flag, a.type_id
HAVING SUM(a.quantity) > 0`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AssetGroup
	for rows.Next() {
		var g AssetGroup
		if err := rows.Scan(&g.OwnerID, &g.LocationID, &g.HolderID, &g.Flag,
			&g.HolderName, &g.TypeID, &g.Quantity); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// MarketGroups returns goods sitting on open sell orders. They left the
// hangar when the order was placed and appear in NO asset row, so an
// inventory built from /assets/ alone silently loses them — the first
// reconciliation after the first inventory found exactly that.
func (s *Store) MarketGroups() ([]AssetGroup, error) {
	rows, err := s.db.Query(`SELECT owner_id, location_id, type_id, SUM(volume_remain)
		FROM hist_order WHERE is_buy = 0 AND state = 'open' AND volume_remain > 0
		GROUP BY owner_id, location_id, type_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AssetGroup
	for rows.Next() {
		g := AssetGroup{Flag: "Market", HolderName: "на витрине"}
		if err := rows.Scan(&g.OwnerID, &g.LocationID, &g.TypeID, &g.Quantity); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// Transactions returns collected fills, oldest first — the order they must
// be posted in, or FIFO would consume lots that did not exist yet.
func (s *Store) Transactions() ([]TxRow, error) {
	rows, err := s.db.Query(`SELECT owner_id, division, transaction_id, journal_ref_id,
		client_id, is_buy, is_personal, date, type_id, quantity, unit_price, location_id
		FROM hist_transaction ORDER BY date, transaction_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TxRow
	for rows.Next() {
		var t TxRow
		var isBuy, isPersonal int
		var date int64
		if err := rows.Scan(&t.OwnerID, &t.Division, &t.TransactionID, &t.JournalRefID,
			&t.ClientID, &isBuy, &isPersonal, &date, &t.TypeID, &t.Quantity,
			&t.UnitPrice, &t.LocationID); err != nil {
			return nil, err
		}
		t.IsBuy, t.IsPersonal, t.Date = isBuy == 1, isPersonal == 1, time.Unix(date, 0)
		out = append(out, t)
	}
	return out, rows.Err()
}

// JournalOfTypes returns journal lines of the given ref_types, oldest
// first. Used to attach a sale to its tax line and a broker fee to its
// order — neither of which ESI links directly (see ACCOUNTING.md §8).
func (s *Store) JournalOfTypes(refTypes ...string) ([]JournalRow, error) {
	if len(refTypes) == 0 {
		return nil, nil
	}
	q := `SELECT owner_id, division, id, date, ref_type, amount, balance,
		context_id, context_id_type FROM hist_journal WHERE ref_type IN (?`
	args := []any{refTypes[0]}
	for _, rt := range refTypes[1:] {
		q += ",?"
		args = append(args, rt)
	}
	q += ") ORDER BY date, id"

	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []JournalRow
	for rows.Next() {
		var j JournalRow
		var date int64
		if err := rows.Scan(&j.OwnerID, &j.Division, &j.ID, &date, &j.RefType,
			&j.Amount, &j.Balance, &j.ContextID, &j.ContextIDType); err != nil {
			return nil, err
		}
		j.Date = time.Unix(date, 0)
		out = append(out, j)
	}
	return out, rows.Err()
}

// Orders returns stored orders for matching broker fees by time.
func (s *Store) Orders() ([]OrderRow, error) {
	rows, err := s.db.Query(`SELECT owner_id, order_id, type_id, is_buy, price,
		volume_total, volume_remain, location_id, region_id, issued, duration,
		escrow, state FROM hist_order ORDER BY issued`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []OrderRow
	for rows.Next() {
		var o OrderRow
		var isBuy int
		var issued int64
		if err := rows.Scan(&o.OwnerID, &o.OrderID, &o.TypeID, &isBuy, &o.Price,
			&o.VolumeTotal, &o.VolumeRemain, &o.LocationID, &o.RegionID, &issued,
			&o.Duration, &o.Escrow, &o.State); err != nil {
			return nil, err
		}
		o.IsBuy, o.Issued = isBuy == 1, time.Unix(issued, 0)
		out = append(out, o)
	}
	return out, rows.Err()
}

// ── отчёты ───────────────────────────────────────────────────────────

// StockLine is one live pile in the stock report.
type StockLine struct {
	OwnerID    int64
	LocationID int64
	HolderName string
	TypeID     int64
	Quantity   int64
	Cost       float64
	Estimated  bool // any part of it priced by guess rather than by fact
	OldestAt   time.Time
}

// Stock lists what is on hand at cost, newest-cost first by value.
func (s *Store) Stock() ([]StockLine, error) {
	rows, err := s.db.Query(`
SELECT l.owner_id, p.location_id, p.name, l.type_id,
       SUM(l.qty_left), SUM(l.cost_left),
       MAX(CASE WHEN l.cost_kind = 'estimate' THEN 1 ELSE 0 END), MIN(l.at)
FROM acc_lot l JOIN acc_place p ON p.id = l.place_id
WHERE l.qty_left > 0
GROUP BY l.owner_id, p.location_id, p.name, l.type_id
ORDER BY SUM(l.cost_left) DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []StockLine
	for rows.Next() {
		var l StockLine
		var est int
		var oldest int64
		if err := rows.Scan(&l.OwnerID, &l.LocationID, &l.HolderName, &l.TypeID,
			&l.Quantity, &l.Cost, &est, &oldest); err != nil {
			return nil, err
		}
		l.Estimated, l.OldestAt = est == 1, time.Unix(oldest, 0)
		out = append(out, l)
	}
	return out, rows.Err()
}

// MarginLine is realised profit on one type over a period.
type MarginLine struct {
	TypeID   int64
	Sales    int
	Quantity int64
	Revenue  float64
	COGS     float64
	Tax      float64
	Broker   float64
}

// Profit is revenue − COGS − selling costs. Unrealised revaluation of what
// is still on the shelf is deliberately NOT in here (ACCOUNTING.md §2).
func (m MarginLine) Profit() float64 { return m.Revenue - m.COGS - m.Tax - m.Broker }

func (m MarginLine) MarginPct() float64 {
	if m.Revenue == 0 {
		return 0
	}
	return 100 * m.Profit() / m.Revenue
}

// Margins reports realised trading profit by type between two instants.
//
// GRABLE: moves and cash must be collapsed BEFORE they meet. A sale has
// one move per lot it consumed and one cash line per revenue/tax/fee, so
// joining the two tables directly multiplies both sides — the first run of
// this report showed cost of goods at exactly twice revenue.
func (s *Store) Margins(from, to time.Time) ([]MarginLine, error) {
	rows, err := s.db.Query(`
WITH sold AS (
  SELECT d.id AS doc_id, m.type_id,
         SUM(m.qty) AS qty, SUM(m.cost) AS cost
  FROM acc_doc d JOIN acc_move m ON m.doc_id = d.id AND m.qty < 0
  WHERE d.kind = 'sale' AND d.at >= ? AND d.at < ?
  GROUP BY d.id, m.type_id
),
money AS (
  SELECT doc_id,
         SUM(CASE WHEN kind = 'revenue'    THEN amount  ELSE 0 END) AS revenue,
         SUM(CASE WHEN kind = 'sales_tax'  THEN -amount ELSE 0 END) AS tax,
         SUM(CASE WHEN kind = 'broker_fee' THEN -amount ELSE 0 END) AS broker
  FROM acc_cash GROUP BY doc_id
)
SELECT s.type_id, COUNT(DISTINCT s.doc_id), -SUM(s.qty),
       COALESCE(SUM(m.revenue), 0), -SUM(s.cost),
       COALESCE(SUM(m.tax), 0), COALESCE(SUM(m.broker), 0)
FROM sold s LEFT JOIN money m ON m.doc_id = s.doc_id
GROUP BY s.type_id
ORDER BY 4 DESC`, from.Unix(), to.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MarginLine
	for rows.Next() {
		var l MarginLine
		if err := rows.Scan(&l.TypeID, &l.Sales, &l.Quantity, &l.Revenue,
			&l.COGS, &l.Tax, &l.Broker); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// TurnoverLine is one type's movement over a period.
type TurnoverLine struct {
	TypeID     int64
	BoughtQty  int64
	BoughtISK  float64
	SoldQty    int64
	SoldISK    float64
	COGS       float64
	OnHandQty  int64
	OnHandCost float64
}

// Turnover shows what moved and what is stuck: a type with a large pile
// and no sales is frozen capital, and that is the point of the report.
func (s *Store) Turnover(from, to time.Time) ([]TurnoverLine, error) {
	rows, err := s.db.Query(`
WITH moved AS (
  SELECT m.type_id,
         SUM(CASE WHEN d.kind = 'purchase' THEN m.qty ELSE 0 END)        AS bought_qty,
         SUM(CASE WHEN d.kind = 'purchase' THEN m.cost ELSE 0 END)       AS bought_isk,
         SUM(CASE WHEN d.kind = 'sale' THEN -m.qty ELSE 0 END)           AS sold_qty,
         SUM(CASE WHEN d.kind = 'sale' THEN -m.cost ELSE 0 END)          AS cogs
  FROM acc_doc d JOIN acc_move m ON m.doc_id = d.id
  WHERE d.at >= ? AND d.at < ?
  GROUP BY m.type_id
),
onhand AS (
  SELECT type_id, SUM(qty_left) AS qty, SUM(cost_left) AS cost
  FROM acc_lot WHERE qty_left > 0 GROUP BY type_id
),
revenue AS (
  -- One row per (document, type) first: a sale that ate three lots has
  -- three moves, and joining cash straight onto them triples the revenue.
  SELECT t.type_id, SUM(t.isk) AS isk FROM (
    SELECT d.id AS doc_id,
           (SELECT m.type_id FROM acc_move m
             WHERE m.doc_id = d.id AND m.qty < 0 LIMIT 1) AS type_id,
           (SELECT COALESCE(SUM(c.amount), 0) FROM acc_cash c
             WHERE c.doc_id = d.id AND c.kind = 'revenue') AS isk
    FROM acc_doc d
    WHERE d.kind = 'sale' AND d.at >= ? AND d.at < ?
  ) t WHERE t.type_id IS NOT NULL
  GROUP BY t.type_id
)
SELECT COALESCE(mv.type_id, oh.type_id),
       COALESCE(mv.bought_qty,0), COALESCE(mv.bought_isk,0),
       COALESCE(mv.sold_qty,0),   COALESCE(rv.isk,0),
       COALESCE(mv.cogs,0),
       COALESCE(oh.qty,0),        COALESCE(oh.cost,0)
FROM moved mv
FULL OUTER JOIN onhand oh ON oh.type_id = mv.type_id
LEFT JOIN revenue rv ON rv.type_id = COALESCE(mv.type_id, oh.type_id)
ORDER BY COALESCE(oh.cost,0) + COALESCE(mv.bought_isk,0) DESC`,
		from.Unix(), to.Unix(), from.Unix(), to.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TurnoverLine
	for rows.Next() {
		var l TurnoverLine
		if err := rows.Scan(&l.TypeID, &l.BoughtQty, &l.BoughtISK, &l.SoldQty,
			&l.SoldISK, &l.COGS, &l.OnHandQty, &l.OnHandCost); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// Attention counts what a human still has to look at.
type Attention struct {
	EstimateLots  int
	EstimateCost  float64
	ShortfallLots int
	Documents     int
	Lots          int
	StockCost     float64
}

func (s *Store) Attention() (Attention, error) {
	var a Attention
	err := s.db.QueryRow(`
SELECT (SELECT COUNT(*) FROM acc_lot WHERE cost_kind = 'estimate' AND qty_left > 0),
       (SELECT COALESCE(SUM(cost_left),0) FROM acc_lot WHERE cost_kind = 'estimate' AND qty_left > 0),
       (SELECT COUNT(*) FROM acc_lot l JOIN acc_doc d ON d.id = l.doc_id
         WHERE l.cost_kind = 'estimate' AND d.kind IN ('sale','manufacture')),
       (SELECT COUNT(*) FROM acc_doc),
       (SELECT COUNT(*) FROM acc_lot WHERE qty_left > 0),
       (SELECT COALESCE(SUM(cost_left),0) FROM acc_lot WHERE qty_left > 0)`).
		Scan(&a.EstimateLots, &a.EstimateCost, &a.ShortfallLots, &a.Documents,
			&a.Lots, &a.StockCost)
	return a, err
}

// OpeningTimes returns when each owner's books were opened. Anything that
// happened before that instant is already baked into the opening stock and
// must not be posted a second time.
func (s *Store) OpeningTimes() (map[int64]time.Time, error) {
	rows, err := s.db.Query(`SELECT owner_id, at FROM acc_doc WHERE kind = 'opening'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]time.Time{}
	for rows.Next() {
		var owner, at int64
		if err := rows.Scan(&owner, &at); err != nil {
			return nil, err
		}
		out[owner] = time.Unix(at, 0)
	}
	return out, rows.Err()
}

// Jobs returns collected industry jobs, oldest first.
func (s *Store) Jobs() ([]JobRow, error) {
	rows, err := s.db.Query(`SELECT owner_id, job_id, installer_id, activity_id,
		blueprint_id, blueprint_type_id, product_type_id, runs, successful_runs,
		cost, status, facility_id, start_date, end_date, completed_date
		FROM hist_job ORDER BY start_date, job_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []JobRow
	for rows.Next() {
		var j JobRow
		var start, end, done int64
		if err := rows.Scan(&j.OwnerID, &j.JobID, &j.InstallerID, &j.ActivityID,
			&j.BlueprintID, &j.BlueprintTypeID, &j.ProductTypeID, &j.Runs,
			&j.SuccessfulRuns, &j.Cost, &j.Status, &j.FacilityID,
			&start, &end, &done); err != nil {
			return nil, err
		}
		j.StartDate = time.Unix(start, 0)
		j.EndDate = time.Unix(end, 0)
		if done > 0 {
			j.CompletedDate = time.Unix(done, 0)
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// ProductionLine is one finished (or running) job seen from the money side.
type ProductionLine struct {
	DocID    int64
	At       time.Time
	TypeID   int64
	Quantity int64
	Cost     float64 // материалы + сбор за установку, всё что съела работа
	Note     string
	InWIP    bool // продукт ещё не выдан, лежит в НЗП
}

// Production lists what was made and what it actually cost.
//
// The cost here is the FACT — the real purchase prices of the materials
// that were consumed — which is exactly what /tools/build cannot know: it
// prices a plan at today's market. Put side by side, the two answer
// "насколько выгодно я это пустил в производство".
func (s *Store) Production(from, to time.Time) ([]ProductionLine, error) {
	rows, err := s.db.Query(`
SELECT d.id, d.at, m.type_id, m.qty, m.cost, d.note,
       MAX(CASE WHEN p.flag = 'WIP' THEN 1 ELSE 0 END)
FROM acc_doc d
JOIN acc_move m ON m.doc_id = d.id AND m.qty > 0
JOIN acc_place p ON p.id = m.place_id
WHERE d.kind IN ('manufacture','reaction') AND d.at >= ? AND d.at < ?
GROUP BY d.id, m.type_id, m.qty, m.cost, d.note
ORDER BY d.at DESC`, from.Unix(), to.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ProductionLine
	for rows.Next() {
		var l ProductionLine
		var at int64
		var wip int
		if err := rows.Scan(&l.DocID, &at, &l.TypeID, &l.Quantity, &l.Cost,
			&l.Note, &wip); err != nil {
			return nil, err
		}
		l.At, l.InWIP = time.Unix(at, 0), wip == 1
		out = append(out, l)
	}
	return out, rows.Err()
}
