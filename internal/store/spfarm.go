package store

import "time"

// SP-ферма: снапшоты цен и журнал закупа PLEX.
//
// ESI отдаёт дневную историю рынка только за ~13 месяцев, а закуп PLEX
// растягивается на год и дольше — без своей записи прошлогодний провал
// цены, по которому и надо было закупаться, исчезнет из графика. Снапшоты
// пишет фоновая задача коллектора и, как страховка для копии с выключенным
// сбором, само открытие страницы (не чаще раза в час).
//
// Журнал закупа — ручной по решению владельца: покупки редкие, механизм
// сначала отлаживается руками, автоматизация из hist_transaction — потом.

func (s *Store) migrateSPFarm() error {
	_, err := s.db.Exec(`
CREATE TABLE IF NOT EXISTS spfarm_price (
    at       INTEGER NOT NULL, -- unix seconds
    type_id  INTEGER NOT NULL,
    sell_min REAL NOT NULL,
    sell_p98 REAL NOT NULL,
    buy_max  REAL NOT NULL,
    buy_p98  REAL NOT NULL,
    PRIMARY KEY (type_id, at)
);
CREATE TABLE IF NOT EXISTS plex_purchase (
    id    INTEGER PRIMARY KEY AUTOINCREMENT,
    day   TEXT    NOT NULL, -- YYYY-MM-DD
    qty   INTEGER NOT NULL, -- PLEX
    price REAL    NOT NULL, -- ISK за штуку
    note  TEXT    NOT NULL DEFAULT ''
);
-- Ростер фермы: какие аккаунты кабинета участвуют и какие персонажи
-- переливают SP. Пул навыков — список названий, по одному на строку:
-- скорость персонажа идёт в прогноз, только когда тренируемый навык из
-- пула (пустой пул = любой навык).
CREATE TABLE IF NOT EXISTS spfarm_account (
    account TEXT PRIMARY KEY
);
CREATE TABLE IF NOT EXISTS spfarm_char (
    character_id INTEGER PRIMARY KEY,
    skill_pool   TEXT NOT NULL DEFAULT ''
);
-- Предложения магазина EVE: цена в PLEX и длительность в месяцах —
-- годовая модель пересчитывает любое предложение в PLEX/год.
CREATE TABLE IF NOT EXISTS spfarm_offer (
    id     INTEGER PRIMARY KEY AUTOINCREMENT,
    name   TEXT    NOT NULL,
    plex   REAL    NOT NULL,
    months INTEGER NOT NULL DEFAULT 12
);
-- Заготовленные планы прокачки для пулов навыков фермы: именованный
-- текст в том же формате, что и сам пул (игровой буфер или по строке).
CREATE TABLE IF NOT EXISTS spfarm_plan (
    id   INTEGER PRIMARY KEY AUTOINCREMENT,
    name TEXT NOT NULL,
    body TEXT NOT NULL
)`)
	return err
}

// ── планы прокачки ───────────────────────────────────────────────────

type FarmPlan struct {
	ID   int64
	Name string
	Body string
}

func (s *Store) FarmPlans() ([]FarmPlan, error) {
	rows, err := s.db.Query(`SELECT id, name, body FROM spfarm_plan ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FarmPlan
	for rows.Next() {
		var p FarmPlan
		if err := rows.Scan(&p.ID, &p.Name, &p.Body); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Store) AddFarmPlan(name, body string) (int64, error) {
	res, err := s.db.Exec(`INSERT INTO spfarm_plan (name, body) VALUES (?, ?)`, name, body)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) DeleteFarmPlan(id int64) error {
	_, err := s.db.Exec(`DELETE FROM spfarm_plan WHERE id = ?`, id)
	return err
}

// ── ростер фермы ─────────────────────────────────────────────────────

// FarmAccounts returns the account labels enrolled in the farm.
func (s *Store) FarmAccounts() (map[string]bool, error) {
	rows, err := s.db.Query(`SELECT account FROM spfarm_account`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var a string
		if err := rows.Scan(&a); err != nil {
			return nil, err
		}
		out[a] = true
	}
	return out, rows.Err()
}

// FarmChars returns farm characters with their skill pools.
func (s *Store) FarmChars() (map[int64]string, error) {
	rows, err := s.db.Query(`SELECT character_id, skill_pool FROM spfarm_char`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]string{}
	for rows.Next() {
		var id int64
		var pool string
		if err := rows.Scan(&id, &pool); err != nil {
			return nil, err
		}
		out[id] = pool
	}
	return out, rows.Err()
}

// SetFarmRoster replaces the whole roster in one transaction — the
// settings form always submits the full picture.
func (s *Store) SetFarmRoster(accounts []string, chars map[int64]string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM spfarm_account`); err != nil {
		return err
	}
	for _, a := range accounts {
		if _, err := tx.Exec(`INSERT OR IGNORE INTO spfarm_account (account) VALUES (?)`, a); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`DELETE FROM spfarm_char`); err != nil {
		return err
	}
	for id, pool := range chars {
		if _, err := tx.Exec(`INSERT INTO spfarm_char (character_id, skill_pool) VALUES (?, ?)`,
			id, pool); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ── предложения магазина EVE ─────────────────────────────────────────

type FarmOffer struct {
	ID     int64
	Name   string
	Plex   float64
	Months int
}

// PlexPerYear нормализует предложение к годовой модели.
func (o FarmOffer) PlexPerYear() float64 {
	if o.Months <= 0 {
		return o.Plex
	}
	return o.Plex * 12 / float64(o.Months)
}

func (s *Store) FarmOffers() ([]FarmOffer, error) {
	rows, err := s.db.Query(`SELECT id, name, plex, months FROM spfarm_offer ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FarmOffer
	for rows.Next() {
		var o FarmOffer
		if err := rows.Scan(&o.ID, &o.Name, &o.Plex, &o.Months); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

func (s *Store) FarmOffer(id int64) (FarmOffer, error) {
	var o FarmOffer
	err := s.db.QueryRow(`SELECT id, name, plex, months FROM spfarm_offer WHERE id = ?`, id).
		Scan(&o.ID, &o.Name, &o.Plex, &o.Months)
	return o, err
}

func (s *Store) AddFarmOffer(o FarmOffer) error {
	_, err := s.db.Exec(`INSERT INTO spfarm_offer (name, plex, months) VALUES (?,?,?)`,
		o.Name, o.Plex, o.Months)
	return err
}

func (s *Store) DeleteFarmOffer(id int64) error {
	_, err := s.db.Exec(`DELETE FROM spfarm_offer WHERE id = ?`, id)
	return err
}

// FarmSnap is one order-book snapshot of one farm good.
type FarmSnap struct {
	TypeID  int64
	At      time.Time
	SellMin float64
	SellP98 float64
	BuyMax  float64
	BuyP98  float64
}

// SaveFarmSnaps appends snapshots; a duplicate (type, second) is ignored.
func (s *Store) SaveFarmSnaps(snaps []FarmSnap) error {
	_, err := s.insertMany(len(snaps), `INSERT OR IGNORE INTO spfarm_price
		(at, type_id, sell_min, sell_p98, buy_max, buy_p98)
		VALUES (?,?,?,?,?,?)`,
		func(i int) []any {
			r := snaps[i]
			return []any{r.At.Unix(), r.TypeID, r.SellMin, r.SellP98, r.BuyMax, r.BuyP98}
		})
	return err
}

// LastFarmSnapAt tells when prices were recorded last — the page uses it
// to write its own snapshot only when the collector has not just done so.
func (s *Store) LastFarmSnapAt() time.Time {
	var at int64
	_ = s.db.QueryRow(`SELECT COALESCE(MAX(at), 0) FROM spfarm_price`).Scan(&at)
	if at == 0 {
		return time.Time{}
	}
	return time.Unix(at, 0)
}

// FarmDay is one day of stored prices, averaged over the day's snapshots.
type FarmDay struct {
	Day  time.Time
	Sell float64 // среднее по sell_min за день
	Buy  float64 // среднее по buy_max за день
}

// FarmDailyPrices rolls the snapshots of one type up to days (UTC),
// oldest first. This is what outlives the 13-month ESI history window.
func (s *Store) FarmDailyPrices(typeID int64) ([]FarmDay, error) {
	rows, err := s.db.Query(`SELECT date(at, 'unixepoch'), AVG(sell_min), AVG(buy_max)
		FROM spfarm_price WHERE type_id = ?
		GROUP BY date(at, 'unixepoch') ORDER BY 1`, typeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FarmDay
	for rows.Next() {
		var day string
		var d FarmDay
		if err := rows.Scan(&day, &d.Sell, &d.Buy); err != nil {
			return nil, err
		}
		if t, err := time.Parse("2006-01-02", day); err == nil {
			d.Day = t
			out = append(out, d)
		}
	}
	return out, rows.Err()
}

// PlexPurchase is one hand-entered PLEX buy.
type PlexPurchase struct {
	ID    int64
	Day   string // YYYY-MM-DD
	Qty   int64
	Price float64 // ISK за штуку
	Note  string
}

func (s *Store) AddPlexPurchase(p PlexPurchase) error {
	_, err := s.db.Exec(`INSERT INTO plex_purchase (day, qty, price, note)
		VALUES (?,?,?,?)`, p.Day, p.Qty, p.Price, p.Note)
	return err
}

func (s *Store) DeletePlexPurchase(id int64) error {
	_, err := s.db.Exec(`DELETE FROM plex_purchase WHERE id = ?`, id)
	return err
}

// PlexPurchases returns the log, newest first.
func (s *Store) PlexPurchases() ([]PlexPurchase, error) {
	rows, err := s.db.Query(`SELECT id, day, qty, price, note
		FROM plex_purchase ORDER BY day DESC, id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PlexPurchase
	for rows.Next() {
		var p PlexPurchase
		if err := rows.Scan(&p.ID, &p.Day, &p.Qty, &p.Price, &p.Note); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
