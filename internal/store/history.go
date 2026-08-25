package store

import (
	"database/sql"
	"time"
)

// This file is the accounting collector's storage: everything ESI serves
// through a rolling window and then forgets. See ACCOUNTING.md §11,
// "Этап 0" — without a local copy the purchase price of a lot is lost for
// good and cost basis can never be reconstructed.
//
// Deliberately dumb: raw ESI rows, deduplicated by the ESI id, no
// accounting logic at all. The ledger (acc_*) is built on top of this
// later and can always be rebuilt from it.
//
// owner_id is a character_id or a corporation_id; division is the corp
// wallet division (0 for personal). Every timestamp is unix seconds —
// the SQLite driver hands TIMESTAMP columns back as strings.

func (s *Store) migrateHistory() error {
	_, err := s.db.Exec(`
CREATE TABLE IF NOT EXISTS hist_transaction (
    owner_id       INTEGER NOT NULL,
    division       INTEGER NOT NULL DEFAULT 0,
    transaction_id INTEGER NOT NULL,
    journal_ref_id INTEGER NOT NULL DEFAULT 0, -- ties the fill to its tax line
    client_id      INTEGER NOT NULL DEFAULT 0,
    is_buy         INTEGER NOT NULL,
    is_personal    INTEGER NOT NULL DEFAULT 1,
    date           INTEGER NOT NULL,
    type_id        INTEGER NOT NULL,
    quantity       INTEGER NOT NULL,
    unit_price     REAL    NOT NULL,
    location_id    INTEGER NOT NULL,
    PRIMARY KEY (owner_id, division, transaction_id)
);
CREATE INDEX IF NOT EXISTS hist_transaction_date ON hist_transaction(date);

CREATE TABLE IF NOT EXISTS hist_journal (
    owner_id        INTEGER NOT NULL,
    division        INTEGER NOT NULL DEFAULT 0,
    id              INTEGER NOT NULL,
    date            INTEGER NOT NULL,
    ref_type        TEXT    NOT NULL,
    amount          REAL    NOT NULL,
    balance         REAL    NOT NULL,
    -- GRABLE (verified): brokers_fee and transaction_tax carry NO context
    -- at all. Only market_transaction (context = transaction id) and
    -- contract_brokers_fee (context = contract id) do. Fees therefore have
    -- to be matched indirectly — tax pro rata by sale value, broker fee by
    -- the timestamp of the order it was charged for.
    context_id      INTEGER NOT NULL DEFAULT 0,
    context_id_type TEXT    NOT NULL DEFAULT '',
    first_party_id  INTEGER NOT NULL DEFAULT 0,
    second_party_id INTEGER NOT NULL DEFAULT 0,
    tax             REAL    NOT NULL DEFAULT 0,
    tax_receiver_id INTEGER NOT NULL DEFAULT 0,
    description     TEXT    NOT NULL DEFAULT '',
    reason          TEXT    NOT NULL DEFAULT '',
    PRIMARY KEY (owner_id, division, id)
);
CREATE INDEX IF NOT EXISTS hist_journal_date ON hist_journal(date);
CREATE INDEX IF NOT EXISTS hist_journal_ctx  ON hist_journal(context_id, ref_type);

-- Live and closed orders in one table: /orders/ has no state, and
-- /orders/history/ is the only place a finished order still exists.
CREATE TABLE IF NOT EXISTS hist_order (
    owner_id      INTEGER NOT NULL,
    order_id      INTEGER NOT NULL,
    type_id       INTEGER NOT NULL,
    is_buy        INTEGER NOT NULL,
    price         REAL    NOT NULL,
    volume_total  INTEGER NOT NULL,
    volume_remain INTEGER NOT NULL,
    location_id   INTEGER NOT NULL,
    region_id     INTEGER NOT NULL DEFAULT 0,
    issued        INTEGER NOT NULL DEFAULT 0,
    duration      INTEGER NOT NULL DEFAULT 0,
    escrow        REAL    NOT NULL DEFAULT 0,
    state         TEXT    NOT NULL DEFAULT 'open',
    seen_at       INTEGER NOT NULL,
    PRIMARY KEY (owner_id, order_id)
);

CREATE TABLE IF NOT EXISTS hist_job (
    owner_id          INTEGER NOT NULL,
    job_id            INTEGER NOT NULL,
    installer_id      INTEGER NOT NULL DEFAULT 0,
    activity_id       INTEGER NOT NULL,
    blueprint_id      INTEGER NOT NULL DEFAULT 0,
    blueprint_type_id INTEGER NOT NULL DEFAULT 0,
    product_type_id   INTEGER NOT NULL DEFAULT 0,
    runs              INTEGER NOT NULL DEFAULT 0,
    successful_runs   INTEGER NOT NULL DEFAULT 0,
    cost              REAL    NOT NULL DEFAULT 0, -- install fee, capitalised
    status            TEXT    NOT NULL DEFAULT '',
    facility_id       INTEGER NOT NULL DEFAULT 0,
    start_date        INTEGER NOT NULL DEFAULT 0,
    end_date          INTEGER NOT NULL DEFAULT 0,
    completed_date    INTEGER NOT NULL DEFAULT 0,
    seen_at           INTEGER NOT NULL,
    PRIMARY KEY (owner_id, job_id)
);

CREATE TABLE IF NOT EXISTS hist_contract (
    owner_id          INTEGER NOT NULL,
    contract_id       INTEGER NOT NULL,
    type              TEXT    NOT NULL, -- item_exchange|courier|auction|loan
    status            TEXT    NOT NULL,
    title             TEXT    NOT NULL DEFAULT '',
    for_corporation   INTEGER NOT NULL DEFAULT 0,
    issuer_id         INTEGER NOT NULL DEFAULT 0,
    issuer_corp_id    INTEGER NOT NULL DEFAULT 0,
    assignee_id       INTEGER NOT NULL DEFAULT 0,
    acceptor_id       INTEGER NOT NULL DEFAULT 0,
    start_location_id INTEGER NOT NULL DEFAULT 0,
    end_location_id   INTEGER NOT NULL DEFAULT 0,
    date_issued       INTEGER NOT NULL DEFAULT 0,
    date_accepted     INTEGER NOT NULL DEFAULT 0,
    date_completed    INTEGER NOT NULL DEFAULT 0,
    price             REAL    NOT NULL DEFAULT 0,
    reward            REAL    NOT NULL DEFAULT 0,
    collateral        REAL    NOT NULL DEFAULT 0,
    volume            REAL    NOT NULL DEFAULT 0, -- the ONLY hint at what a
                                                  -- courier carried (see 6)
    items_loaded      INTEGER NOT NULL DEFAULT 0,
    seen_at           INTEGER NOT NULL,
    PRIMARY KEY (owner_id, contract_id)
);

-- GRABLE (ПРОВЕРЕНО): a courier contract never lists its cargo, in any
-- status. Only item_exchange fills this table.
CREATE TABLE IF NOT EXISTS hist_contract_item (
    contract_id  INTEGER NOT NULL,
    record_id    INTEGER NOT NULL,
    type_id      INTEGER NOT NULL,
    quantity     INTEGER NOT NULL,
    raw_quantity INTEGER NOT NULL DEFAULT 0,
    is_included  INTEGER NOT NULL DEFAULT 1,
    is_singleton INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (contract_id, record_id)
);

-- Current asset picture, replaced on every scan. Keeping a full hourly
-- snapshot forever would grow without bound and answer nothing extra:
-- what accounting needs is the CHANGE, which goes to asset_change.
CREATE TABLE IF NOT EXISTS asset_state (
    owner_id       INTEGER NOT NULL,
    item_id        INTEGER NOT NULL,
    type_id        INTEGER NOT NULL,
    quantity       INTEGER NOT NULL,
    location_id    INTEGER NOT NULL,
    location_flag  TEXT    NOT NULL DEFAULT '',
    location_type  TEXT    NOT NULL DEFAULT '',
    is_singleton   INTEGER NOT NULL DEFAULT 0,
    parent_item_id INTEGER NOT NULL DEFAULT 0, -- 0 = sits at a location
    root_id        INTEGER NOT NULL DEFAULT 0, -- station/structure
    name           TEXT    NOT NULL DEFAULT '',
    seen_at        INTEGER NOT NULL,
    PRIMARY KEY (owner_id, item_id)
);
CREATE INDEX IF NOT EXISTS asset_state_where ON asset_state(owner_id, root_id, type_id);

-- Append-only diff between consecutive scans. Stage 2 turns these rows
-- into documents; until then they are just evidence.
CREATE TABLE IF NOT EXISTS asset_change (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    at          INTEGER NOT NULL,
    owner_id    INTEGER NOT NULL,
    item_id     INTEGER NOT NULL,
    type_id     INTEGER NOT NULL,
    kind        TEXT    NOT NULL, -- add|remove|move|qty
    qty_before  INTEGER NOT NULL DEFAULT 0,
    qty_after   INTEGER NOT NULL DEFAULT 0,
    from_root   INTEGER NOT NULL DEFAULT 0,
    from_parent INTEGER NOT NULL DEFAULT 0,
    from_flag   TEXT    NOT NULL DEFAULT '',
    to_root     INTEGER NOT NULL DEFAULT 0,
    to_parent   INTEGER NOT NULL DEFAULT 0,
    to_flag     TEXT    NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS asset_change_at ON asset_change(at);

-- One row per collector task: when it last succeeded and what it said.
CREATE TABLE IF NOT EXISTS collector_run (
    task     TEXT PRIMARY KEY,
    last_ok  INTEGER NOT NULL DEFAULT 0,
    last_try INTEGER NOT NULL DEFAULT 0,
    note     TEXT    NOT NULL DEFAULT ''
)`)
	if err != nil {
		return err
	}
	return s.migrateLedger()
}

// ── типы строк ───────────────────────────────────────────────────────
// Deliberately store-local: internal/esi imports internal/store, so the
// dependency must not run the other way. The collector maps between them.

type TxRow struct {
	OwnerID       int64
	Division      int
	TransactionID int64
	JournalRefID  int64
	ClientID      int64
	IsBuy         bool
	IsPersonal    bool
	Date          time.Time
	TypeID        int64
	Quantity      int64
	UnitPrice     float64
	LocationID    int64
}

type JournalRow struct {
	OwnerID       int64
	Division      int
	ID            int64
	Date          time.Time
	RefType       string
	Amount        float64
	Balance       float64
	ContextID     int64
	ContextIDType string
	FirstPartyID  int64
	SecondPartyID int64
	Tax           float64
	TaxReceiverID int64
	Description   string
	Reason        string
}

type OrderRow struct {
	OwnerID      int64
	OrderID      int64
	TypeID       int64
	IsBuy        bool
	Price        float64
	VolumeTotal  int64
	VolumeRemain int64
	LocationID   int64
	RegionID     int64
	Issued       time.Time
	Duration     int
	Escrow       float64
	State        string
}

type JobRow struct {
	OwnerID         int64
	JobID           int64
	InstallerID     int64
	ActivityID      int
	BlueprintID     int64
	BlueprintTypeID int64
	ProductTypeID   int64
	Runs            int
	SuccessfulRuns  int
	Cost            float64
	Status          string
	FacilityID      int64
	StartDate       time.Time
	EndDate         time.Time
	CompletedDate   time.Time
}

type ContractRow struct {
	OwnerID         int64
	ContractID      int64
	Type            string
	Status          string
	Title           string
	ForCorporation  bool
	IssuerID        int64
	IssuerCorpID    int64
	AssigneeID      int64
	AcceptorID      int64
	StartLocationID int64
	EndLocationID   int64
	DateIssued      time.Time
	DateAccepted    time.Time
	DateCompleted   time.Time
	Price           float64
	Reward          float64
	Collateral      float64
	Volume          float64
}

type ContractItemRow struct {
	ContractID  int64
	RecordID    int64
	TypeID      int64
	Quantity    int64
	RawQuantity int64
	IsIncluded  bool
	IsSingleton bool
}

// AssetRow is one flattened asset line with its parent chain resolved.
type AssetRow struct {
	OwnerID      int64
	ItemID       int64
	TypeID       int64
	Quantity     int64
	LocationID   int64
	LocationFlag string
	LocationType string
	IsSingleton  bool
	ParentItemID int64
	RootID       int64
	Name         string
}

// AssetDiff counts what one scan changed.
type AssetDiff struct {
	Added, Removed, Moved, Requantified int
}

func unix(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}

func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
}

// ── запись ───────────────────────────────────────────────────────────

// SaveTransactions stores fills. They never change, so a row already
// present is left alone — this is the dedup that makes re-collecting the
// same ESI window free.
func (s *Store) SaveTransactions(rows []TxRow) (int, error) {
	return s.insertMany(len(rows), `INSERT OR IGNORE INTO hist_transaction
		(owner_id, division, transaction_id, journal_ref_id, client_id, is_buy,
		 is_personal, date, type_id, quantity, unit_price, location_id)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
		func(i int) []any {
			r := rows[i]
			return []any{r.OwnerID, r.Division, r.TransactionID, r.JournalRefID,
				r.ClientID, btoi(r.IsBuy), btoi(r.IsPersonal), unix(r.Date),
				r.TypeID, r.Quantity, r.UnitPrice, r.LocationID}
		})
}

// SaveJournal stores wallet journal lines (immutable, same dedup).
func (s *Store) SaveJournal(rows []JournalRow) (int, error) {
	return s.insertMany(len(rows), `INSERT OR IGNORE INTO hist_journal
		(owner_id, division, id, date, ref_type, amount, balance, context_id,
		 context_id_type, first_party_id, second_party_id, tax, tax_receiver_id,
		 description, reason)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		func(i int) []any {
			r := rows[i]
			return []any{r.OwnerID, r.Division, r.ID, unix(r.Date), r.RefType,
				r.Amount, r.Balance, r.ContextID, r.ContextIDType, r.FirstPartyID,
				r.SecondPartyID, r.Tax, r.TaxReceiverID, r.Description, r.Reason}
		})
}

// SaveOrders upserts orders: a live order changes as it fills, and the
// same order later reappears from /orders/history/ with a final state.
func (s *Store) SaveOrders(rows []OrderRow, seenAt time.Time) (int, error) {
	return s.insertMany(len(rows), `INSERT INTO hist_order
		(owner_id, order_id, type_id, is_buy, price, volume_total, volume_remain,
		 location_id, region_id, issued, duration, escrow, state, seen_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(owner_id, order_id) DO UPDATE SET
			price = excluded.price, volume_remain = excluded.volume_remain,
			escrow = excluded.escrow, state = excluded.state,
			seen_at = excluded.seen_at`,
		func(i int) []any {
			r := rows[i]
			return []any{r.OwnerID, r.OrderID, r.TypeID, btoi(r.IsBuy), r.Price,
				r.VolumeTotal, r.VolumeRemain, r.LocationID, r.RegionID,
				unix(r.Issued), r.Duration, r.Escrow, r.State, seenAt.Unix()}
		})
}

// SaveJobs upserts industry jobs; status and completion move over time.
func (s *Store) SaveJobs(rows []JobRow, seenAt time.Time) (int, error) {
	return s.insertMany(len(rows), `INSERT INTO hist_job
		(owner_id, job_id, installer_id, activity_id, blueprint_id, blueprint_type_id,
		 product_type_id, runs, successful_runs, cost, status, facility_id,
		 start_date, end_date, completed_date, seen_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(owner_id, job_id) DO UPDATE SET
			status = excluded.status, successful_runs = excluded.successful_runs,
			completed_date = excluded.completed_date, seen_at = excluded.seen_at`,
		func(i int) []any {
			r := rows[i]
			return []any{r.OwnerID, r.JobID, r.InstallerID, r.ActivityID, r.BlueprintID,
				r.BlueprintTypeID, r.ProductTypeID, r.Runs, r.SuccessfulRuns, r.Cost,
				r.Status, r.FacilityID, unix(r.StartDate), unix(r.EndDate),
				unix(r.CompletedDate), seenAt.Unix()}
		})
}

// SaveContracts upserts contracts; status walks outstanding → in_progress
// → finished, and the dates fill in as it goes.
func (s *Store) SaveContracts(rows []ContractRow, seenAt time.Time) (int, error) {
	return s.insertMany(len(rows), `INSERT INTO hist_contract
		(owner_id, contract_id, type, status, title, for_corporation, issuer_id,
		 issuer_corp_id, assignee_id, acceptor_id, start_location_id, end_location_id,
		 date_issued, date_accepted, date_completed, price, reward, collateral,
		 volume, seen_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(owner_id, contract_id) DO UPDATE SET
			status = excluded.status, acceptor_id = excluded.acceptor_id,
			date_accepted = excluded.date_accepted,
			date_completed = excluded.date_completed, seen_at = excluded.seen_at`,
		func(i int) []any {
			r := rows[i]
			return []any{r.OwnerID, r.ContractID, r.Type, r.Status, r.Title,
				btoi(r.ForCorporation), r.IssuerID, r.IssuerCorpID, r.AssigneeID,
				r.AcceptorID, r.StartLocationID, r.EndLocationID, unix(r.DateIssued),
				unix(r.DateAccepted), unix(r.DateCompleted), r.Price, r.Reward,
				r.Collateral, r.Volume, seenAt.Unix()}
		})
}

// SaveContractItems stores a contract's cargo and marks the contract as
// loaded, so the collector asks ESI for it only once.
func (s *Store) SaveContractItems(contractID int64, rows []ContractItemRow) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, r := range rows {
		if _, err := tx.Exec(`INSERT OR IGNORE INTO hist_contract_item
			(contract_id, record_id, type_id, quantity, raw_quantity, is_included, is_singleton)
			VALUES (?,?,?,?,?,?,?)`,
			r.ContractID, r.RecordID, r.TypeID, r.Quantity, r.RawQuantity,
			btoi(r.IsIncluded), btoi(r.IsSingleton)); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`UPDATE hist_contract SET items_loaded = 1 WHERE contract_id = ?`,
		contractID); err != nil {
		return err
	}
	return tx.Commit()
}

// ContractsNeedingItems lists contracts whose cargo has not been fetched
// yet. Couriers are excluded: ESI never lists their cargo (ПРОВЕРЕНО), so
// asking would burn a request per contract forever.
func (s *Store) ContractsNeedingItems(ownerID int64, limit int) ([]int64, error) {
	rows, err := s.db.Query(`SELECT contract_id FROM hist_contract
		WHERE owner_id = ? AND items_loaded = 0 AND type <> 'courier'
		ORDER BY date_issued DESC LIMIT ?`, ownerID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// ApplyAssetSnapshot diffs a fresh scan against the stored state, appends
// the changes and replaces the state. It returns what changed so the
// collector can log something meaningful instead of "ok".
//
// Cross-owner moves show up as a remove for one owner and an add for the
// other; pairing them by item_id is stage 2's job and is exact, because a
// container keeps its item_id through hangars, corp hangars and couriers
// (ПРОВЕРЕНО, ACCOUNTING.md §4.4).
func (s *Store) ApplyAssetSnapshot(ownerID int64, rows []AssetRow, at time.Time) (AssetDiff, error) {
	var d AssetDiff
	tx, err := s.db.Begin()
	if err != nil {
		return d, err
	}
	defer tx.Rollback()

	type prev struct {
		typeID, qty, root, parent int64
		flag                      string
	}
	old := map[int64]prev{}
	cur, err := tx.Query(`SELECT item_id, type_id, quantity, root_id, parent_item_id,
		location_flag FROM asset_state WHERE owner_id = ?`, ownerID)
	if err != nil {
		return d, err
	}
	for cur.Next() {
		var id int64
		var p prev
		if err := cur.Scan(&id, &p.typeID, &p.qty, &p.root, &p.parent, &p.flag); err != nil {
			cur.Close()
			return d, err
		}
		old[id] = p
	}
	cur.Close()
	if err := cur.Err(); err != nil {
		return d, err
	}

	logChange := func(kind string, r AssetRow, p prev, hadOld bool) error {
		_, err := tx.Exec(`INSERT INTO asset_change
			(at, owner_id, item_id, type_id, kind, qty_before, qty_after,
			 from_root, from_parent, from_flag, to_root, to_parent, to_flag)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			at.Unix(), ownerID, r.ItemID, r.TypeID, kind,
			p.qty, r.Quantity, p.root, p.parent, p.flag,
			r.RootID, r.ParentItemID, r.LocationFlag)
		return err
	}

	// The very first scan of an owner is a baseline, not a diff: logging
	// every one of its several thousand rows as "appeared" would bury the
	// real changes that follow.
	baseline := len(old) == 0

	seen := make(map[int64]bool, len(rows))
	for _, r := range rows {
		seen[r.ItemID] = true
		p, had := old[r.ItemID]
		switch {
		case !had:
			if !baseline {
				if err := logChange("add", r, prev{}, false); err != nil {
					return d, err
				}
			}
			d.Added++
		default:
			if p.root != r.RootID || p.parent != r.ParentItemID || p.flag != r.LocationFlag {
				if err := logChange("move", r, p, true); err != nil {
					return d, err
				}
				d.Moved++
			}
			if p.qty != r.Quantity {
				if err := logChange("qty", r, p, true); err != nil {
					return d, err
				}
				d.Requantified++
			}
		}
		if _, err := tx.Exec(`INSERT INTO asset_state
			(owner_id, item_id, type_id, quantity, location_id, location_flag,
			 location_type, is_singleton, parent_item_id, root_id, name, seen_at)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?)
			ON CONFLICT(owner_id, item_id) DO UPDATE SET
				quantity = excluded.quantity, location_id = excluded.location_id,
				location_flag = excluded.location_flag,
				location_type = excluded.location_type,
				parent_item_id = excluded.parent_item_id, root_id = excluded.root_id,
				name = CASE WHEN excluded.name <> '' THEN excluded.name ELSE asset_state.name END,
				seen_at = excluded.seen_at`,
			ownerID, r.ItemID, r.TypeID, r.Quantity, r.LocationID, r.LocationFlag,
			r.LocationType, btoi(r.IsSingleton), r.ParentItemID, r.RootID, r.Name,
			at.Unix()); err != nil {
			return d, err
		}
	}

	for id, p := range old {
		if seen[id] {
			continue
		}
		if _, err := tx.Exec(`INSERT INTO asset_change
			(at, owner_id, item_id, type_id, kind, qty_before, qty_after,
			 from_root, from_parent, from_flag)
			VALUES (?,?,?,?,'remove',?,0,?,?,?)`,
			at.Unix(), ownerID, id, p.typeID, p.qty, p.root, p.parent, p.flag); err != nil {
			return d, err
		}
		if _, err := tx.Exec(`DELETE FROM asset_state WHERE owner_id = ? AND item_id = ?`,
			ownerID, id); err != nil {
			return d, err
		}
		d.Removed++
	}
	return d, tx.Commit()
}

// ── журнал прогонов коллектора ───────────────────────────────────────

func (s *Store) MarkCollectorRun(task string, at time.Time, ok bool, note string) {
	var okAt int64
	if ok {
		okAt = at.Unix()
	}
	// last_ok must survive a failing run: it is the "when did this last
	// actually work" that a stuck collector is spotted by.
	s.db.Exec(`INSERT INTO collector_run (task, last_ok, last_try, note)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(task) DO UPDATE SET
			last_try = excluded.last_try,
			last_ok  = CASE WHEN excluded.last_ok > 0
			                THEN excluded.last_ok ELSE collector_run.last_ok END,
			note     = excluded.note`,
		task, okAt, at.Unix(), note)
}

// CollectorStatus is one line for a future "фоновые задачи" panel.
type CollectorStatus struct {
	Task    string
	LastOK  time.Time
	LastTry time.Time
	Note    string
}

func (s *Store) CollectorStatuses() ([]CollectorStatus, error) {
	rows, err := s.db.Query(`SELECT task, last_ok, last_try, note
		FROM collector_run ORDER BY task`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CollectorStatus
	for rows.Next() {
		var c CollectorStatus
		var ok, try int64
		if err := rows.Scan(&c.Task, &ok, &try, &c.Note); err != nil {
			return nil, err
		}
		if ok > 0 {
			c.LastOK = time.Unix(ok, 0)
		}
		if try > 0 {
			c.LastTry = time.Unix(try, 0)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// insertMany runs one prepared statement over n rows in a single
// transaction and reports how many rows it actually inserted.
func (s *Store) insertMany(n int, query string, args func(int) []any) (int, error) {
	if n == 0 {
		return 0, nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	stmt, err := tx.Prepare(query)
	if err != nil {
		return 0, err
	}
	defer stmt.Close()
	written := 0
	for i := 0; i < n; i++ {
		res, err := stmt.Exec(args(i)...)
		if err != nil {
			return written, err
		}
		if aff, err := res.RowsAffected(); err == nil {
			written += int(aff)
		}
	}
	return written, tx.Commit()
}

var _ = sql.ErrNoRows

// NamedAssets returns the item ids of this owner that already carry a
// name, so the collector asks ESI only for the ones it has never seen.
func (s *Store) NamedAssets(ownerID int64) (map[int64]bool, error) {
	rows, err := s.db.Query(`SELECT item_id FROM asset_state
		WHERE owner_id = ? AND name <> ''`, ownerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]bool{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}
