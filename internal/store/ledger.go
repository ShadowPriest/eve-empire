package store

import (
	"database/sql"
	"fmt"
	"time"
)

// Ledger storage: the accounting core of ACCOUNTING.md §5.
//
// A document is ONE business event; direction lives on the line, not on
// the document, because most real events are an issue and a receipt at
// once (production eats materials and yields a product, a transfer takes
// from one place and gives to another).
//
// Balances are never stored. Everything is SUM(acc_move.qty), with
// qty_left/cost_left on the lot as a rebuildable cache.

func (s *Store) migrateLedger() error {
	_, err := s.db.Exec(`
CREATE TABLE IF NOT EXISTS acc_doc (
    id       INTEGER PRIMARY KEY AUTOINCREMENT,
    kind     TEXT NOT NULL,
    status   TEXT NOT NULL DEFAULT 'posted', -- draft|posted|void
    owner_id INTEGER NOT NULL,
    at       INTEGER NOT NULL, -- event time, NOT import time
    -- Idempotency: the same ESI object must never post twice.
    src      TEXT NOT NULL,
    src_id   TEXT NOT NULL,
    note     TEXT NOT NULL DEFAULT '',
    UNIQUE (src, src_id)
);
CREATE INDEX IF NOT EXISTS acc_doc_at ON acc_doc(at);

-- A place is (owner, location, holder, flag). The holder is any assembled
-- item with a stable item_id — a container, a ship, a courier wrap — or 0
-- for the bare hangar, which is where most value actually sits.
CREATE TABLE IF NOT EXISTS acc_place (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    owner_id    INTEGER NOT NULL,
    location_id INTEGER NOT NULL,
    holder_id   INTEGER NOT NULL DEFAULT 0,
    flag        TEXT    NOT NULL DEFAULT 'Hangar',
    name        TEXT    NOT NULL DEFAULT '',
    UNIQUE (owner_id, location_id, holder_id, flag)
);

CREATE TABLE IF NOT EXISTS acc_lot (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    type_id       INTEGER NOT NULL,
    owner_id      INTEGER NOT NULL,
    place_id      INTEGER NOT NULL REFERENCES acc_place(id),
    stack_item_id INTEGER NOT NULL DEFAULT 0, -- hint only: a split forks it
    qty_init      INTEGER NOT NULL,
    qty_left      INTEGER NOT NULL,
    -- cost_left is carried, not derived. Recomputing it from a unit price
    -- loses ISK on every partial issue, and the last issue of a lot takes
    -- whatever is left, so a lot can never end at "0 units, 4.7 ISK".
    cost_total    REAL NOT NULL,
    cost_left     REAL NOT NULL,
    mkt_total     REAL NOT NULL DEFAULT 0, -- Jita value when received, for
    mkt_left      REAL NOT NULL DEFAULT 0, -- the per-stage margin report
    cost_kind     TEXT NOT NULL DEFAULT 'fact', -- fact|estimate
    doc_id        INTEGER NOT NULL REFERENCES acc_doc(id),
    at            INTEGER NOT NULL -- receipt time: FIFO order and stock age
);
CREATE INDEX IF NOT EXISTS acc_lot_live
    ON acc_lot(owner_id, type_id, at) WHERE qty_left > 0;

CREATE TABLE IF NOT EXISTS acc_move (
    id       INTEGER PRIMARY KEY AUTOINCREMENT,
    doc_id   INTEGER NOT NULL REFERENCES acc_doc(id),
    lot_id   INTEGER NOT NULL REFERENCES acc_lot(id),
    place_id INTEGER NOT NULL,
    type_id  INTEGER NOT NULL,
    qty      INTEGER NOT NULL, -- >0 receipt, <0 issue
    cost     REAL    NOT NULL, -- ISK moved with these units, sign follows qty
    mkt      REAL    NOT NULL DEFAULT 0,
    at       INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS acc_move_doc ON acc_move(doc_id);
CREATE INDEX IF NOT EXISTS acc_move_type ON acc_move(type_id, at);

CREATE TABLE IF NOT EXISTS acc_cash (
    id       INTEGER PRIMARY KEY AUTOINCREMENT,
    doc_id   INTEGER NOT NULL REFERENCES acc_doc(id),
    owner_id INTEGER NOT NULL,
    division INTEGER NOT NULL DEFAULT 0,
    kind     TEXT    NOT NULL, -- revenue|cost|broker_fee|sales_tax|
                               -- install_fee|internal|other
    amount   REAL    NOT NULL, -- signed, as in the wallet journal
    at       INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS acc_cash_doc ON acc_cash(doc_id)`)
	return err
}

// ── типы ─────────────────────────────────────────────────────────────

type Doc struct {
	Kind    string
	OwnerID int64
	At      time.Time
	Src     string // esi:transaction | esi:job | manual | …
	SrcID   string
	Note    string
}

// PlaceKey identifies where goods sit. HolderID 0 means the bare hangar.
type PlaceKey struct {
	OwnerID    int64
	LocationID int64
	HolderID   int64
	Flag       string
	Name       string // holder label, for humans only
}

// Alloc pins part of an issue to a specific lot — the manual override that
// makes "put THIS purchase into production" possible.
type Alloc struct {
	LotID int64
	Qty   int64
}

// Line is one goods movement. Qty > 0 is a receipt (creates a lot),
// Qty < 0 is an issue (consumes lots).
type Line struct {
	Place  PlaceKey
	TypeID int64
	Qty    int64

	// приход
	// At overrides the document time for this receipt. The opening
	// inventory needs it: its layers carry the dates of the purchases they
	// came from, and FIFO order is exactly those dates.
	At          time.Time
	CostTotal   float64
	MktTotal    float64
	CostKind    string // fact|estimate, default fact
	StackItemID int64

	// CostFrom = "issue" берёт себестоимость этой строки прихода из того,
	// что списано в ЭТОМ ЖЕ документе по тому же типу. Так себестоимость
	// переезжает вместе с товаром при перемещении и переносится на продукт
	// при переработке — вместо того чтобы выдумываться заново.
	CostFrom string

	// расход
	Alloc []Alloc // nil = FIFO
	// Scope widens the search when the exact place has too little: goods
	// sold off a market order left the hangar when the order was placed,
	// and stage 1 does not model that escrow yet.
	Scope string // place (default) | location | owner
	// ShortfallCost values units the ledger has no lot for. They are
	// created as an estimate lot and flagged, never silently zeroed.
	ShortfallUnitCost float64
}

type CashLine struct {
	OwnerID  int64
	Division int
	Kind     string
	Amount   float64
}

// PostResult reports what a posting actually did.
type PostResult struct {
	DocID     int64
	Posted    bool  // false = this src/src_id was already in the ledger
	Shortfall int64 // units issued without a lot to back them
}

// ── проводка ─────────────────────────────────────────────────────────

// PostDoc writes one document with its lines and cash in a single
// transaction. Re-posting the same (src, src_id) is a no-op, which is what
// lets the whole ledger be rebuilt from hist_* by simply running again.
func (s *Store) PostDoc(d Doc, lines []Line, cash []CashLine) (PostResult, error) {
	var res PostResult
	tx, err := s.db.Begin()
	if err != nil {
		return res, err
	}
	defer tx.Rollback()

	if d.Src == "" {
		d.Src = "manual"
	}
	status := "posted"
	r, err := tx.Exec(`INSERT OR IGNORE INTO acc_doc (kind, status, owner_id, at, src, src_id, note)
		VALUES (?,?,?,?,?,?,?)`,
		d.Kind, status, d.OwnerID, d.At.Unix(), d.Src, d.SrcID, d.Note)
	if err != nil {
		return res, err
	}
	if n, _ := r.RowsAffected(); n == 0 {
		// Already posted: hand back the existing id and change nothing.
		err = tx.QueryRow(`SELECT id FROM acc_doc WHERE src = ? AND src_id = ?`,
			d.Src, d.SrcID).Scan(&res.DocID)
		return res, err
	}
	res.DocID, _ = r.LastInsertId()
	res.Posted = true

	// Списания идут ПЕРВЫМИ: приход с CostFrom="issue" забирает их
	// себестоимость, а не выдумывает свою. На этом стоят и перемещение
	// (цена едет с товаром), и переработка (цена переходит на минералы).
	issued := map[int64]float64{}
	// Оценка не должна отмываться в факт переездом: если хоть часть
	// списанного была прикидкой, приход наследует эту пометку.
	guessed := map[int64]bool{}
	for _, ln := range lines {
		if ln.Qty >= 0 {
			continue
		}
		short, cost, est, err := s.issue(tx, res.DocID, d.At, ln)
		if err != nil {
			return res, err
		}
		res.Shortfall += short
		issued[ln.TypeID] += cost
		if est {
			guessed[ln.TypeID] = true
		}
	}
	for _, ln := range lines {
		if ln.Qty <= 0 {
			continue
		}
		if ln.CostFrom == "issue" {
			ln.CostTotal = issued[ln.TypeID]
			if guessed[ln.TypeID] {
				ln.CostKind = "estimate"
			}
		}
		if err := s.receive(tx, res.DocID, d.At, ln); err != nil {
			return res, err
		}
	}
	for _, c := range cash {
		if _, err := tx.Exec(`INSERT INTO acc_cash (doc_id, owner_id, division, kind, amount, at)
			VALUES (?,?,?,?,?,?)`,
			res.DocID, c.OwnerID, c.Division, c.Kind, c.Amount, d.At.Unix()); err != nil {
			return res, err
		}
	}
	return res, tx.Commit()
}

// placeID upserts a place and returns its id.
func (s *Store) placeID(tx *sql.Tx, p PlaceKey) (int64, error) {
	if p.Flag == "" {
		p.Flag = "Hangar"
	}
	var id int64
	err := tx.QueryRow(`SELECT id FROM acc_place
		WHERE owner_id=? AND location_id=? AND holder_id=? AND flag=?`,
		p.OwnerID, p.LocationID, p.HolderID, p.Flag).Scan(&id)
	if err == nil {
		if p.Name != "" {
			tx.Exec(`UPDATE acc_place SET name=? WHERE id=? AND name<>?`, p.Name, id, p.Name)
		}
		return id, nil
	}
	if err != sql.ErrNoRows {
		return 0, err
	}
	r, err := tx.Exec(`INSERT INTO acc_place (owner_id, location_id, holder_id, flag, name)
		VALUES (?,?,?,?,?)`, p.OwnerID, p.LocationID, p.HolderID, p.Flag, p.Name)
	if err != nil {
		return 0, err
	}
	return r.LastInsertId()
}

func (s *Store) receive(tx *sql.Tx, docID int64, at time.Time, ln Line) error {
	placeID, err := s.placeID(tx, ln.Place)
	if err != nil {
		return err
	}
	kind := ln.CostKind
	if kind == "" {
		kind = "fact"
	}
	if !ln.At.IsZero() {
		at = ln.At
	}
	r, err := tx.Exec(`INSERT INTO acc_lot
		(type_id, owner_id, place_id, stack_item_id, qty_init, qty_left,
		 cost_total, cost_left, mkt_total, mkt_left, cost_kind, doc_id, at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		ln.TypeID, ln.Place.OwnerID, placeID, ln.StackItemID, ln.Qty, ln.Qty,
		ln.CostTotal, ln.CostTotal, ln.MktTotal, ln.MktTotal, kind, docID, at.Unix())
	if err != nil {
		return err
	}
	lotID, _ := r.LastInsertId()
	_, err = tx.Exec(`INSERT INTO acc_move (doc_id, lot_id, place_id, type_id, qty, cost, mkt, at)
		VALUES (?,?,?,?,?,?,?,?)`,
		docID, lotID, placeID, ln.TypeID, ln.Qty, ln.CostTotal, ln.MktTotal, at.Unix())
	return err
}

type lotRow struct {
	id        int64
	qtyLeft   int64
	costLeft  float64
	mktLeft   float64
	placeID   int64
	estimated bool
}

// issue consumes lots for one outgoing line. It returns how many units it
// could not back with a real lot, and the total cost it took off the shelf
// — the latter is what a transfer or a reprocess hands to its receipt.
func (s *Store) issue(tx *sql.Tx, docID int64, at time.Time, ln Line) (int64, float64, bool, error) {
	need := -ln.Qty
	var spent float64
	var estimated bool
	placeID, err := s.placeID(tx, ln.Place)
	if err != nil {
		return 0, 0, false, err
	}

	take := func(l lotRow, qty int64) error {
		var cost, mkt float64
		if qty >= l.qtyLeft {
			// Last issue of this lot takes everything that is left, so no
			// rounding tail can survive it.
			qty, cost, mkt = l.qtyLeft, l.costLeft, l.mktLeft
		} else {
			cost = l.costLeft * float64(qty) / float64(l.qtyLeft)
			mkt = l.mktLeft * float64(qty) / float64(l.qtyLeft)
		}
		if _, err := tx.Exec(`UPDATE acc_lot
			SET qty_left = qty_left - ?, cost_left = cost_left - ?, mkt_left = mkt_left - ?
			WHERE id = ?`, qty, cost, mkt, l.id); err != nil {
			return err
		}
		spent += cost
		if l.estimated {
			estimated = true
		}
		_, err := tx.Exec(`INSERT INTO acc_move (doc_id, lot_id, place_id, type_id, qty, cost, mkt, at)
			VALUES (?,?,?,?,?,?,?,?)`,
			docID, l.id, l.placeID, ln.TypeID, -qty, -cost, -mkt, at.Unix())
		return err
	}

	// Explicit allocation: specific identification, the manual override.
	for _, a := range ln.Alloc {
		var l lotRow
		if err := tx.QueryRow(`SELECT id, qty_left, cost_left, mkt_left, place_id,
			cost_kind = 'estimate' FROM acc_lot WHERE id = ?`, a.LotID).
			Scan(&l.id, &l.qtyLeft, &l.costLeft, &l.mktLeft, &l.placeID, &l.estimated); err != nil {
			return 0, 0, false, fmt.Errorf("партия %d: %w", a.LotID, err)
		}
		qty := min64(a.Qty, l.qtyLeft, need)
		if qty <= 0 {
			continue
		}
		if err := take(l, qty); err != nil {
			return 0, 0, false, err
		}
		need -= qty
	}

	// FIFO over widening scopes: the exact place first, then the station,
	// then anything this owner holds.
	scopes := []string{"place", "location", "owner"}
	start := 0
	switch ln.Scope {
	case "location":
		start = 1
	case "owner":
		start = 2
	}
	for i := start; i < len(scopes) && need > 0; i++ {
		lots, err := s.candidateLots(tx, ln, placeID, scopes[i])
		if err != nil {
			return 0, 0, false, err
		}
		for _, l := range lots {
			if need <= 0 {
				break
			}
			qty := min64(need, l.qtyLeft)
			if err := take(l, qty); err != nil {
				return 0, 0, false, err
			}
			need -= qty
		}
		if ln.Scope != "" {
			break // caller pinned the scope: do not widen past it
		}
	}

	if need <= 0 {
		return 0, spent, estimated, nil
	}
	// Nothing left to draw on. Never silently zero: make an estimate lot so
	// the shortage is visible and priced, then consume it.
	cost := ln.ShortfallUnitCost * float64(need)
	r, err := tx.Exec(`INSERT INTO acc_lot
		(type_id, owner_id, place_id, qty_init, qty_left, cost_total, cost_left,
		 mkt_total, mkt_left, cost_kind, doc_id, at)
		VALUES (?,?,?,?,?,?,?,?,?, 'estimate', ?, ?)`,
		ln.TypeID, ln.Place.OwnerID, placeID, need, need, cost, cost, cost, cost,
		docID, at.Unix())
	if err != nil {
		return 0, 0, false, err
	}
	lotID, _ := r.LastInsertId()
	if err := take(lotRow{id: lotID, qtyLeft: need, costLeft: cost, mktLeft: cost, placeID: placeID, estimated: true}, need); err != nil {
		return 0, 0, false, err
	}
	return need, spent, true, nil
}

func (s *Store) candidateLots(tx *sql.Tx, ln Line, placeID int64, scope string) ([]lotRow, error) {
	q := `SELECT l.id, l.qty_left, l.cost_left, l.mkt_left, l.place_id,
		       l.cost_kind = 'estimate'
		FROM acc_lot l JOIN acc_place p ON p.id = l.place_id
		WHERE l.qty_left > 0 AND l.type_id = ? AND l.owner_id = ?`
	args := []any{ln.TypeID, ln.Place.OwnerID}
	switch scope {
	case "place":
		q += ` AND l.place_id = ?`
		args = append(args, placeID)
	case "location":
		q += ` AND p.location_id = ?`
		args = append(args, ln.Place.LocationID)
	}
	// FIFO: oldest receipt first, id as the tie-breaker so the order is
	// stable and a rebuild produces the same allocation.
	q += ` ORDER BY l.at, l.id`

	rows, err := tx.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []lotRow
	for rows.Next() {
		var l lotRow
		if err := rows.Scan(&l.id, &l.qtyLeft, &l.costLeft, &l.mktLeft, &l.placeID,
			&l.estimated); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

func min64(vals ...int64) int64 {
	m := vals[0]
	for _, v := range vals[1:] {
		if v < m {
			m = v
		}
	}
	return m
}

// PostedSrcIDs returns the src_id values already in the ledger for a
// source, so a builder can skip them without a query per row.
func (s *Store) PostedSrcIDs(src string) (map[string]bool, error) {
	rows, err := s.db.Query(`SELECT src_id FROM acc_doc WHERE src = ?`, src)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

// LedgerEmpty reports whether anything has been posted at all.
func (s *Store) LedgerEmpty() bool {
	var n int
	s.db.QueryRow(`SELECT 1 FROM acc_doc LIMIT 1`).Scan(&n)
	return n == 0
}
