// Package store persists characters and their (encrypted) SSO tokens
// in a local SQLite database.
package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

type Store struct {
	db  *sql.DB
	key []byte // AES-256 key for refresh token encryption
}

type Character struct {
	ID       int64
	UserID   int64 // кабинет-владелец; 0 = не привязан (до миграции кабинета)
	Name     string
	Account  string   // user-assigned account label for sidebar grouping
	Tags     []string // user-assigned tags for sidebar filtering
	AddedAt  time.Time
	Scopes   []string
	TokenExp time.Time
}

// Has — есть ли право в токене персонажа. Источник истины — `scp` из
// JWT, сохранённый в `tokens.scopes`: пресет, который выбрали на входе,
// нигде не хранится, а CCP может выдать не всё, что просили.
func (c Character) Has(scope string) bool {
	for _, s := range c.Scopes {
		if s == scope {
			return true
		}
	}
	return false
}

func Open(path string, encryptionKey []byte) (*Store, error) {
	// synchronous(NORMAL) is the recommended pairing with WAL: an fsync
	// per checkpoint instead of per commit. On the router's flash storage
	// the per-commit fsync made every ESI cache write block the sole
	// connection — and with it every page read — for tens of milliseconds.
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)")
	if err != nil {
		return nil, err
	}
	// SQLite handles one writer at a time; keep it simple.
	db.SetMaxOpenConns(1)

	s := &Store{db: db, key: encryptionKey}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate() error {
	_, err := s.db.Exec(`
CREATE TABLE IF NOT EXISTS characters (
    character_id INTEGER PRIMARY KEY,
    name         TEXT NOT NULL,
    added_at     INTEGER NOT NULL -- unix seconds
);
CREATE TABLE IF NOT EXISTS tokens (
    character_id      INTEGER PRIMARY KEY REFERENCES characters(character_id) ON DELETE CASCADE,
    refresh_token_enc BLOB NOT NULL,
    access_token      TEXT NOT NULL,
    expires_at        INTEGER NOT NULL, -- unix seconds
    scopes            TEXT NOT NULL DEFAULT ''
);
`)
	if err != nil {
		return err
	}
	// Older databases may lack newer columns; ignore "duplicate column".
	for _, ddl := range []string{
		`ALTER TABLE characters ADD COLUMN account TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE characters ADD COLUMN sort_order INTEGER NOT NULL DEFAULT 0`,
		// Каким приложением EVE выдан токен. Refresh-токен привязан к client_id,
		// и база, приехавшая с другой копии, обновиться не сможет — колонка
		// позволяет показать это до первой неудачной попытки. Пусто = токен
		// сохранён до появления учёта, приложение неизвестно.
		`ALTER TABLE tokens ADD COLUMN client_id TEXT NOT NULL DEFAULT ''`,
		// Кабинет-владелец персонажа (см. ARCHITECTURE.md, «Кабинет и
		// пользователи»). 0 = ещё не привязан, миграция кабинета раздаёт 1.
		`ALTER TABLE characters ADD COLUMN user_id INTEGER NOT NULL DEFAULT 0`,
	} {
		if _, err := s.db.Exec(ddl); err != nil && !strings.Contains(err.Error(), "duplicate column") {
			return err
		}
	}
	_, err = s.db.Exec(`
CREATE TABLE IF NOT EXISTS account_order (
    user_id  INTEGER NOT NULL DEFAULT 0,
    account  TEXT NOT NULL,
    position INTEGER NOT NULL,
    PRIMARY KEY (user_id, account)
);
-- Кабинет: личность = множество персонажей, без имени и пароля.
CREATE TABLE IF NOT EXISTS users (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    created_at INTEGER NOT NULL, -- unix seconds
    admin      INTEGER NOT NULL DEFAULT 0
);
-- Серверные сессии: в базе лежит hex SHA-256 от случайного id, сам id
-- живёт только в куке, поэтому копия базы не даёт войти.
CREATE TABLE IF NOT EXISTS sessions (
    id_hash    TEXT PRIMARY KEY,
    user_id    INTEGER NOT NULL,
    created_at INTEGER NOT NULL,
    last_seen  INTEGER NOT NULL,
    expires_at INTEGER NOT NULL,
    user_agent TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS sessions_user ON sessions(user_id);
-- Пользовательские настройки; инстансовые остаются в app_settings.
CREATE TABLE IF NOT EXISTS user_settings (
    user_id INTEGER NOT NULL,
    key     TEXT NOT NULL,
    value   TEXT NOT NULL,
    PRIMARY KEY (user_id, key)
);
-- Согласие члена корпорации показывать её руководству часть своих
-- данных (этап 5 плана кабинета). ESI таких прав не даёт вовсе —
-- всё, что видит CEO, человек показал сам.
CREATE TABLE IF NOT EXISTS char_share (
    character_id   INTEGER PRIMARY KEY,
    corporation_id INTEGER NOT NULL,
    blocks         TEXT NOT NULL, -- список через запятую: skills, lines
    created_at     INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS char_share_corp ON char_share(corporation_id);
CREATE TABLE IF NOT EXISTS esi_cache (
    url     TEXT PRIMARY KEY,
    body    BLOB NOT NULL,
    pages   INTEGER NOT NULL DEFAULT 1,
    expires INTEGER NOT NULL -- unix seconds
);
CREATE TABLE IF NOT EXISTS entity_names (
    id   INTEGER PRIMARY KEY,
    name TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS char_tags (
    character_id INTEGER NOT NULL,
    tag          TEXT NOT NULL,
    PRIMARY KEY (character_id, tag)
);
CREATE TABLE IF NOT EXISTS app_settings (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);
-- planetary colony templates in the game's export/import format
CREATE TABLE IF NOT EXISTS pi_templates (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id      INTEGER NOT NULL DEFAULT 0,
    name         TEXT NOT NULL,
    planet_type  INTEGER NOT NULL DEFAULT 0,
    product_type INTEGER NOT NULL DEFAULT 0,
    cmd_ctr_lv   INTEGER NOT NULL DEFAULT 0,
    payload      TEXT NOT NULL,
    created_at   INTEGER NOT NULL
);
-- skill plans in the game's clipboard format; ESI cannot write the skill
-- queue, so a plan lives here until it is pasted into the client
CREATE TABLE IF NOT EXISTS skill_plans (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id      INTEGER NOT NULL DEFAULT 0,
    name         TEXT NOT NULL,
    character_id INTEGER NOT NULL DEFAULT 0,
    body         TEXT NOT NULL,
    created_at   INTEGER NOT NULL,
    updated_at   INTEGER NOT NULL
);
-- fleet telemetry: ESI shows only the CURRENT composition, so the page
-- diffs every reading against the last one and keeps the changes here.
CREATE TABLE IF NOT EXISTS fleet_state (
    fleet_id     INTEGER NOT NULL,
    character_id INTEGER NOT NULL,
    ship_type_id INTEGER NOT NULL DEFAULT 0,
    system_id    INTEGER NOT NULL DEFAULT 0,
    station_id   INTEGER NOT NULL DEFAULT 0,
    wing_id      INTEGER NOT NULL DEFAULT 0,
    squad_id     INTEGER NOT NULL DEFAULT 0,
    role         TEXT NOT NULL DEFAULT '',
    joined_at    INTEGER NOT NULL DEFAULT 0, -- join_time as ESI reports it
    seen_at      INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (fleet_id, character_id)
);
CREATE TABLE IF NOT EXISTS fleet_events (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    fleet_id     INTEGER NOT NULL,
    character_id INTEGER NOT NULL,
    at           INTEGER NOT NULL, -- unix seconds
    kind         TEXT NOT NULL,
    from_id      INTEGER NOT NULL DEFAULT 0,
    to_id        INTEGER NOT NULL DEFAULT 0,
    text         TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS fleet_events_at ON fleet_events(fleet_id, at);
-- mining ledger: ESI keeps a rolling window, we keep everything. The
-- Jita price OF THAT DAY is stored next to the quantity — ore and ice
-- are only comparable in ISK, and the price of a past day must not be
-- re-derived from today's market.
CREATE TABLE IF NOT EXISTS mining_ledger (
    character_id INTEGER NOT NULL,
    day          INTEGER NOT NULL, -- unix seconds, midnight UTC
    system_id    INTEGER NOT NULL,
    type_id      INTEGER NOT NULL,
    quantity     INTEGER NOT NULL,
    price        REAL NOT NULL DEFAULT 0, -- ISK per unit, Jita average that day
    PRIMARY KEY (character_id, day, system_id, type_id)
);
-- omega / MCT expiry per account label. ESI has no subscription
-- endpoint, so the dates are typed in by hand from the game client.
-- Kept apart from account_order: that table is wiped and rebuilt on
-- every sidebar reorder. Dates are 'YYYY-MM-DD HH:MM' or 'YYYY-MM-DD'
-- in EVE time (UTC); empty string = not active / unknown.
CREATE TABLE IF NOT EXISTS account_omega (
    user_id     INTEGER NOT NULL DEFAULT 0,
    account     TEXT NOT NULL,
    omega_until TEXT NOT NULL DEFAULT '',
    mct1_until  TEXT NOT NULL DEFAULT '',
    mct2_until  TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (user_id, account)
)`)
	if err != nil {
		return err
	}
	// ESI Refresher metadata on the cache (esi/refresher.go): whose token
	// fetched the row, the compatibility-date flag, when it was fetched
	// (TTL = expires − fetched) and when a page last read it. Rows from
	// before the columns keep zeros and re-register on their next read.
	for _, ddl := range []string{
		`ALTER TABLE esi_cache ADD COLUMN char_id INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE esi_cache ADD COLUMN compat INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE esi_cache ADD COLUMN fetched INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE esi_cache ADD COLUMN last_read INTEGER NOT NULL DEFAULT 0`,
	} {
		if _, err := s.db.Exec(ddl); err != nil && !strings.Contains(err.Error(), "duplicate column") {
			return err
		}
	}
	if err := s.migrateHistory(); err != nil {
		return err
	}
	if err := s.migrateSPFarm(); err != nil {
		return err
	}
	if err := s.migrateAir(); err != nil {
		return err
	}
	// Последней: раздаёт user_id по уже созданным таблицам.
	return s.migrateCabinet()
}

// ── mining ledger ────────────────────────────────────────────────────

// MiningRow is one stored ledger line.
type MiningRow struct {
	CharacterID int64
	Day         time.Time
	SystemID    int64
	TypeID      int64
	Quantity    int64
	Price       float64
}

// SaveMiningRows upserts ledger lines. A zero price never overwrites a
// known one: the market history lags, so today's rows arrive priceless
// and get their price on a later pass.
func (s *Store) SaveMiningRows(rows []MiningRow) error {
	if len(rows) == 0 {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, r := range rows {
		if _, err := tx.Exec(`INSERT INTO mining_ledger
			(character_id, day, system_id, type_id, quantity, price) VALUES (?,?,?,?,?,?)
			ON CONFLICT(character_id, day, system_id, type_id) DO UPDATE SET
				quantity = excluded.quantity,
				price = CASE WHEN excluded.price > 0 THEN excluded.price ELSE mining_ledger.price END`,
			r.CharacterID, r.Day.Unix(), r.SystemID, r.TypeID, r.Quantity, r.Price); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// MiningRows returns stored ledger lines from the given day on.
func (s *Store) MiningRows(since time.Time) ([]MiningRow, error) {
	rows, err := s.db.Query(`SELECT character_id, day, system_id, type_id, quantity, price
		FROM mining_ledger WHERE day >= ? ORDER BY day`, since.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MiningRow
	for rows.Next() {
		var r MiningRow
		var day int64
		if err := rows.Scan(&r.CharacterID, &day, &r.SystemID, &r.TypeID, &r.Quantity, &r.Price); err != nil {
			return nil, err
		}
		r.Day = time.Unix(day, 0).UTC()
		out = append(out, r)
	}
	return out, rows.Err()
}

// ── fleet telemetry ──────────────────────────────────────────────────

// FleetMemberState is the last observed position of one fleet member.
type FleetMemberState struct {
	CharacterID int64
	ShipTypeID  int64
	SystemID    int64
	StationID   int64
	WingID      int64
	SquadID     int64
	Role        string
	JoinedAt    time.Time
	SeenAt      time.Time
}

// FleetEvent is one recorded change in a fleet.
type FleetEvent struct {
	ID          int64
	FleetID     int64
	CharacterID int64
	At          time.Time
	Kind        string // join | leave | ship | system | dock | undock | role
	FromID      int64
	ToID        int64
	Text        string
}

// FleetStates returns the last known composition of a fleet.
func (s *Store) FleetStates(fleetID int64) (map[int64]FleetMemberState, error) {
	rows, err := s.db.Query(`SELECT character_id, ship_type_id, system_id, station_id,
		wing_id, squad_id, role, joined_at, seen_at FROM fleet_state WHERE fleet_id = ?`, fleetID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]FleetMemberState{}
	for rows.Next() {
		var m FleetMemberState
		var joined, seen int64
		if err := rows.Scan(&m.CharacterID, &m.ShipTypeID, &m.SystemID, &m.StationID,
			&m.WingID, &m.SquadID, &m.Role, &joined, &seen); err != nil {
			return nil, err
		}
		m.JoinedAt, m.SeenAt = time.Unix(joined, 0), time.Unix(seen, 0)
		out[m.CharacterID] = m
	}
	return out, rows.Err()
}

// SaveFleetSnapshot replaces the stored composition and appends events
// in one transaction — a half-written reading would produce phantom
// joins on the next poll.
func (s *Store) SaveFleetSnapshot(fleetID int64, members []FleetMemberState, gone []int64, events []FleetEvent) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	for _, m := range members {
		if _, err := tx.Exec(`INSERT INTO fleet_state
			(fleet_id, character_id, ship_type_id, system_id, station_id, wing_id, squad_id, role, joined_at, seen_at)
			VALUES (?,?,?,?,?,?,?,?,?,?)
			ON CONFLICT(fleet_id, character_id) DO UPDATE SET
				ship_type_id = excluded.ship_type_id, system_id = excluded.system_id,
				station_id = excluded.station_id, wing_id = excluded.wing_id,
				squad_id = excluded.squad_id, role = excluded.role,
				joined_at = excluded.joined_at, seen_at = excluded.seen_at`,
			fleetID, m.CharacterID, m.ShipTypeID, m.SystemID, m.StationID,
			m.WingID, m.SquadID, m.Role, m.JoinedAt.Unix(), m.SeenAt.Unix()); err != nil {
			return err
		}
	}
	for _, id := range gone {
		if _, err := tx.Exec(`DELETE FROM fleet_state WHERE fleet_id = ? AND character_id = ?`,
			fleetID, id); err != nil {
			return err
		}
	}
	for _, e := range events {
		if _, err := tx.Exec(`INSERT INTO fleet_events
			(fleet_id, character_id, at, kind, from_id, to_id, text) VALUES (?,?,?,?,?,?,?)`,
			fleetID, e.CharacterID, e.At.Unix(), e.Kind, e.FromID, e.ToID, e.Text); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// FleetEvents returns the recorded history of a fleet, newest first.
func (s *Store) FleetEvents(fleetID int64, limit int) ([]FleetEvent, error) {
	rows, err := s.db.Query(`SELECT id, character_id, at, kind, from_id, to_id, text
		FROM fleet_events WHERE fleet_id = ? ORDER BY at DESC, id DESC LIMIT ?`, fleetID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FleetEvent
	for rows.Next() {
		e := FleetEvent{FleetID: fleetID}
		var at int64
		if err := rows.Scan(&e.ID, &e.CharacterID, &at, &e.Kind, &e.FromID, &e.ToID, &e.Text); err != nil {
			return nil, err
		}
		e.At = time.Unix(at, 0)
		out = append(out, e)
	}
	return out, rows.Err()
}

// FleetIDsSeen lists fleets we have history for, newest activity first.
func (s *Store) FleetIDsSeen() ([]int64, error) {
	rows, err := s.db.Query(`SELECT fleet_id, MAX(at) m FROM fleet_events
		GROUP BY fleet_id ORDER BY m DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id, at int64
		if err := rows.Scan(&id, &at); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// PITemplate is a stored planetary colony layout.
type PITemplate struct {
	ID          int64
	Name        string
	PlanetType  int64
	ProductType int64
	CmdCtrLv    int
	Payload     string
	CreatedAt   time.Time
}

func (s *Store) PITemplates(userID int64) ([]PITemplate, error) {
	rows, err := s.db.Query(`SELECT id, name, planet_type, product_type, cmd_ctr_lv, payload, created_at
		FROM pi_templates WHERE user_id = ? ORDER BY created_at DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PITemplate
	for rows.Next() {
		var t PITemplate
		var ts int64
		if err := rows.Scan(&t.ID, &t.Name, &t.PlanetType, &t.ProductType, &t.CmdCtrLv, &t.Payload, &ts); err != nil {
			return nil, err
		}
		t.CreatedAt = time.Unix(ts, 0)
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *Store) AddPITemplate(userID int64, t PITemplate) (int64, error) {
	res, err := s.db.Exec(`INSERT INTO pi_templates
		(user_id, name, planet_type, product_type, cmd_ctr_lv, payload, created_at) VALUES (?,?,?,?,?,?,?)`,
		userID, t.Name, t.PlanetType, t.ProductType, t.CmdCtrLv, t.Payload, time.Now().Unix())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) DeletePITemplate(userID, id int64) error {
	_, err := s.db.Exec(`DELETE FROM pi_templates WHERE id = ? AND user_id = ?`, id, userID)
	return err
}

// SkillPlan is a saved training plan in the game's clipboard format.
type SkillPlan struct {
	ID          int64
	Name        string
	CharacterID int64
	Body        string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

func (s *Store) SkillPlans(userID int64) ([]SkillPlan, error) {
	rows, err := s.db.Query(`SELECT id, name, character_id, body, created_at, updated_at
		FROM skill_plans WHERE user_id = ? ORDER BY updated_at DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SkillPlan
	for rows.Next() {
		var p SkillPlan
		var created, updated int64
		if err := rows.Scan(&p.ID, &p.Name, &p.CharacterID, &p.Body, &created, &updated); err != nil {
			return nil, err
		}
		p.CreatedAt, p.UpdatedAt = time.Unix(created, 0), time.Unix(updated, 0)
		out = append(out, p)
	}
	return out, rows.Err()
}

// SaveSkillPlan inserts a new plan or overwrites one by id.
func (s *Store) SaveSkillPlan(userID int64, p SkillPlan) (int64, error) {
	now := time.Now().Unix()
	if p.ID > 0 {
		_, err := s.db.Exec(`UPDATE skill_plans SET name = ?, character_id = ?, body = ?, updated_at = ?
			WHERE id = ? AND user_id = ?`, p.Name, p.CharacterID, p.Body, now, p.ID, userID)
		return p.ID, err
	}
	res, err := s.db.Exec(`INSERT INTO skill_plans (user_id, name, character_id, body, created_at, updated_at)
		VALUES (?,?,?,?,?,?)`, userID, p.Name, p.CharacterID, p.Body, now, now)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) DeleteSkillPlan(userID, id int64) error {
	_, err := s.db.Exec(`DELETE FROM skill_plans WHERE id = ? AND user_id = ?`, id, userID)
	return err
}

// Setting returns an app setting value ("" when unset).
func (s *Store) Setting(key string) string {
	var v string
	_ = s.db.QueryRow(`SELECT value FROM app_settings WHERE key = ?`, key).Scan(&v)
	return v
}

// SettingsPrefix returns every app setting whose key starts with prefix.
func (s *Store) SettingsPrefix(prefix string) map[string]string {
	out := map[string]string{}
	rows, err := s.db.Query(`SELECT key, value FROM app_settings WHERE key LIKE ? || '%'`, prefix)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var k, v string
		if rows.Scan(&k, &v) == nil {
			out[k] = v
		}
	}
	return out
}

// SetSetting stores an app setting.
func (s *Store) SetSetting(key, value string) error {
	_, err := s.db.Exec(`INSERT INTO app_settings (key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}

// SetTags replaces the tag set of a character.
func (s *Store) SetTags(characterID int64, tags []string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM char_tags WHERE character_id = ?`, characterID); err != nil {
		return err
	}
	for _, t := range tags {
		if _, err := tx.Exec(`INSERT OR IGNORE INTO char_tags (character_id, tag) VALUES (?, ?)`, characterID, t); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) allTags() (map[int64][]string, error) {
	rows, err := s.db.Query(`SELECT character_id, tag FROM char_tags ORDER BY tag`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64][]string{}
	for rows.Next() {
		var id int64
		var tag string
		if err := rows.Scan(&id, &tag); err != nil {
			return nil, err
		}
		out[id] = append(out[id], tag)
	}
	return out, rows.Err()
}

// ── ESI response cache (survives restarts) ──────────────────────────

// CacheMeta is everything esi_cache knows about a row besides the body.
type CacheMeta struct {
	URL      string
	CharID   int64 // whose token fetched it; 0 = public route
	Compat   bool  // fetched with X-Compatibility-Date
	Pages    int
	Expires  time.Time
	Fetched  time.Time // zero for rows written before the column existed
	LastRead time.Time // zero until a page has read it since the column appeared
}

func (s *Store) CacheGet(url string) (body []byte, meta CacheMeta, ok bool) {
	var exp, fetched, compat int64
	err := s.db.QueryRow(`SELECT body, pages, expires, char_id, compat, fetched FROM esi_cache WHERE url = ?`, url).
		Scan(&body, &meta.Pages, &exp, &meta.CharID, &compat, &fetched)
	if err != nil {
		return nil, CacheMeta{}, false
	}
	meta.URL = url
	meta.Expires = time.Unix(exp, 0)
	meta.Compat = compat != 0
	if fetched > 0 {
		meta.Fetched = time.Unix(fetched, 0)
	}
	return body, meta, true
}

// CachePut stores a response; last_read is left to CacheTouch.
func (s *Store) CachePut(url string, body []byte, m CacheMeta) {
	compat := 0
	if m.Compat {
		compat = 1
	}
	_, _ = s.db.Exec(`
INSERT INTO esi_cache (url, body, pages, expires, char_id, compat, fetched) VALUES (?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(url) DO UPDATE SET body = excluded.body, pages = excluded.pages, expires = excluded.expires,
    char_id = excluded.char_id, compat = excluded.compat, fetched = excluded.fetched`,
		url, body, m.Pages, m.Expires.Unix(), m.CharID, compat, m.Fetched.Unix())
}

// CacheRenew extends a row whose body came back unchanged: the deadline
// moves, the blob stays on disk untouched.
func (s *Store) CacheRenew(url string, expires, fetched time.Time) {
	_, _ = s.db.Exec(`UPDATE esi_cache SET expires = ?, fetched = ? WHERE url = ?`,
		expires.Unix(), fetched.Unix(), url)
}

// CacheMetas lists every cached URL without its body — the refresher's
// registry at startup.
func (s *Store) CacheMetas() []CacheMeta {
	rows, err := s.db.Query(`SELECT url, pages, expires, char_id, compat, fetched, last_read FROM esi_cache`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []CacheMeta
	for rows.Next() {
		var m CacheMeta
		var exp, fetched, read, compat int64
		if err := rows.Scan(&m.URL, &m.Pages, &exp, &m.CharID, &compat, &fetched, &read); err != nil {
			continue
		}
		m.Expires = time.Unix(exp, 0)
		m.Compat = compat != 0
		if fetched > 0 {
			m.Fetched = time.Unix(fetched, 0)
		}
		if read > 0 {
			m.LastRead = time.Unix(read, 0)
		}
		out = append(out, m)
	}
	return out
}

// CacheTouch records when pages last read the URLs, in one transaction.
func (s *Store) CacheTouch(reads map[string]time.Time) {
	tx, err := s.db.Begin()
	if err != nil {
		return
	}
	defer tx.Rollback()
	stmt, err := tx.Prepare(`UPDATE esi_cache SET last_read = ? WHERE url = ? AND last_read < ?`)
	if err != nil {
		return
	}
	defer stmt.Close()
	for url, at := range reads {
		_, _ = stmt.Exec(at.Unix(), url, at.Unix())
	}
	_ = tx.Commit()
}

// ── entity name cache ────────────────────────────────────────────────

func (s *Store) NamesGet(ids []int64) map[int64]string {
	out := map[int64]string{}
	for _, id := range ids {
		var name string
		if err := s.db.QueryRow(`SELECT name FROM entity_names WHERE id = ?`, id).Scan(&name); err == nil {
			out[id] = name
		}
	}
	return out
}

func (s *Store) NamesPut(names map[int64]string) {
	for id, name := range names {
		_, _ = s.db.Exec(`INSERT OR REPLACE INTO entity_names (id, name) VALUES (?, ?)`, id, name)
	}
}

// SidebarGroup is one account group in user-defined order.
type SidebarGroup struct {
	Account string  `json:"account"`
	Chars   []int64 `json:"chars"`
}

// SaveSidebarOrder persists the drag&drop arrangement: group order,
// character order inside groups and account reassignment on cross-group moves.
func (s *Store) SaveSidebarOrder(userID int64, groups []SidebarGroup) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`DELETE FROM account_order WHERE user_id = ?`, userID); err != nil {
		return err
	}
	for gi, g := range groups {
		if _, err := tx.Exec(`INSERT INTO account_order (user_id, account, position) VALUES (?, ?, ?)`,
			userID, g.Account, gi); err != nil {
			return err
		}
		for ci, id := range g.Chars {
			if _, err := tx.Exec(`UPDATE characters SET account = ?, sort_order = ?
				WHERE character_id = ? AND user_id = ?`, g.Account, ci, id, userID); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

// SetAccount updates the user-assigned account label of a character.
// Чужой персонаж не найдётся и не изменится.
func (s *Store) SetAccount(userID, characterID int64, account string) error {
	_, err := s.db.Exec(`UPDATE characters SET account = ? WHERE character_id = ? AND user_id = ?`,
		account, characterID, userID)
	return err
}

// ErrAccountExists is returned by RenameAccount when the new label is
// already taken — silently merging two accounts is not what anyone meant.
var ErrAccountExists = errors.New("account already exists")

// RenameAccount changes an account label everywhere it lives: on the
// characters, in the sidebar order and on the omega dates.
func (s *Store) RenameAccount(userID int64, oldName, newName string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var n int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM characters WHERE account = ? AND user_id = ?`,
		newName, userID).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return ErrAccountExists
	}
	// Orphan rows under the new name (an account everyone was moved away
	// from keeps its order/omega rows) would break the PK on UPDATE.
	for _, q := range []string{
		`DELETE FROM account_order WHERE account = ? AND user_id = ?`,
		`DELETE FROM account_omega WHERE account = ? AND user_id = ?`,
	} {
		if _, err := tx.Exec(q, newName, userID); err != nil {
			return err
		}
	}
	for _, q := range []string{
		`UPDATE characters SET account = ? WHERE account = ? AND user_id = ?`,
		`UPDATE account_order SET account = ? WHERE account = ? AND user_id = ?`,
		`UPDATE account_omega SET account = ? WHERE account = ? AND user_id = ?`,
	} {
		if _, err := tx.Exec(q, newName, oldName, userID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// AccountOmega holds the hand-entered subscription dates of one account:
// omega itself plus the two extra training slots (MCT certificates).
// Dates are 'YYYY-MM-DD HH:MM' or 'YYYY-MM-DD' in EVE time (UTC);
// empty = not active / unknown.
type AccountOmega struct {
	Account    string
	OmegaUntil string
	MCT1Until  string
	MCT2Until  string
}

// AccountOmegas returns the stored subscription dates keyed by account label.
func (s *Store) AccountOmegas(userID int64) (map[string]AccountOmega, error) {
	rows, err := s.db.Query(`SELECT account, omega_until, mct1_until, mct2_until
		FROM account_omega WHERE user_id = ?`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]AccountOmega{}
	for rows.Next() {
		var o AccountOmega
		if err := rows.Scan(&o.Account, &o.OmegaUntil, &o.MCT1Until, &o.MCT2Until); err != nil {
			return nil, err
		}
		out[o.Account] = o
	}
	return out, rows.Err()
}

// SetAccountOmega upserts the subscription dates of one account; all
// three dates empty removes the row.
func (s *Store) SetAccountOmega(userID int64, o AccountOmega) error {
	if o.OmegaUntil == "" && o.MCT1Until == "" && o.MCT2Until == "" {
		_, err := s.db.Exec(`DELETE FROM account_omega WHERE account = ? AND user_id = ?`, o.Account, userID)
		return err
	}
	_, err := s.db.Exec(`INSERT INTO account_omega (user_id, account, omega_until, mct1_until, mct2_until)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(user_id, account) DO UPDATE SET
			omega_until = excluded.omega_until,
			mct1_until  = excluded.mct1_until,
			mct2_until  = excluded.mct2_until`,
		userID, o.Account, o.OmegaUntil, o.MCT1Until, o.MCT2Until)
	return err
}

// UpsertCharacter stores/updates a character and its tokens after login.
// Кабинет проставляется только новому персонажу (или непривязанному,
// user_id=0): чужого персонажа вход не забирает — для переноса есть
// AttachCharacter.
func (s *Store) UpsertCharacter(userID, id int64, name string, refreshToken, accessToken string, expiresAt time.Time, scopes []string, clientID string) error {
	enc, err := encrypt(s.key, []byte(refreshToken))
	if err != nil {
		return fmt.Errorf("encrypt refresh token: %w", err)
	}

	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`
INSERT INTO characters (character_id, name, added_at, user_id) VALUES (?, ?, ?, ?)
ON CONFLICT(character_id) DO UPDATE SET
    name    = excluded.name,
    user_id = CASE WHEN characters.user_id = 0 THEN excluded.user_id ELSE characters.user_id END`,
		id, name, time.Now().Unix(), userID); err != nil {
		return err
	}
	if _, err := tx.Exec(`
INSERT INTO tokens (character_id, refresh_token_enc, access_token, expires_at, scopes, client_id)
VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT(character_id) DO UPDATE SET
    refresh_token_enc = excluded.refresh_token_enc,
    access_token      = excluded.access_token,
    expires_at        = excluded.expires_at,
    scopes            = excluded.scopes,
    client_id         = excluded.client_id`,
		id, enc, accessToken, expiresAt.Unix(), strings.Join(scopes, " "), clientID); err != nil {
		return err
	}
	return tx.Commit()
}

// UpdateTokens saves a refreshed token pair.
func (s *Store) UpdateTokens(characterID int64, refreshToken, accessToken string, expiresAt time.Time) error {
	enc, err := encrypt(s.key, []byte(refreshToken))
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`
UPDATE tokens SET refresh_token_enc = ?, access_token = ?, expires_at = ?
WHERE character_id = ?`, enc, accessToken, expiresAt.Unix(), characterID)
	return err
}

// TokenClients maps character_id to the EVE application that issued the
// stored token. An empty value means the token predates the column.
func (s *Store) TokenClients() (map[int64]string, error) {
	rows, err := s.db.Query(`SELECT character_id, client_id FROM tokens`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]string{}
	for rows.Next() {
		var id int64
		var client string
		if err := rows.Scan(&id, &client); err != nil {
			return nil, err
		}
		out[id] = client
	}
	return out, rows.Err()
}

// RefreshToken returns the decrypted refresh token for a character.
func (s *Store) RefreshToken(characterID int64) (string, error) {
	var enc []byte
	err := s.db.QueryRow(`SELECT refresh_token_enc FROM tokens WHERE character_id = ?`, characterID).Scan(&enc)
	if err != nil {
		return "", err
	}
	plain, err := decrypt(s.key, enc)
	if err != nil {
		return "", fmt.Errorf("decrypt refresh token: %w", err)
	}
	return string(plain), nil
}

// CharacterScopes returns the scopes of the stored token, or nil when the
// character has no token yet. Used at login to tell a widening set of
// permissions from a narrowing one (internal/web/scopes.go).
func (s *Store) CharacterScopes(characterID int64) ([]string, error) {
	var scopes string
	err := s.db.QueryRow(`SELECT scopes FROM tokens WHERE character_id = ?`, characterID).Scan(&scopes)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return strings.Fields(scopes), nil
}

// AccessToken returns a cached access token and its expiry.
func (s *Store) AccessToken(characterID int64) (string, time.Time, error) {
	var tok string
	var exp int64
	err := s.db.QueryRow(`SELECT access_token, expires_at FROM tokens WHERE character_id = ?`, characterID).Scan(&tok, &exp)
	return tok, time.Unix(exp, 0), err
}

// AllCharacters lists every stored character regardless of cabinet — the
// collector and the ESI Refresher work over the whole instance.
func (s *Store) AllCharacters() ([]Character, error) { return s.characters(0, false) }

// Characters lists the characters of one cabinet.
func (s *Store) Characters(userID int64) ([]Character, error) { return s.characters(userID, true) }

func (s *Store) characters(userID int64, filter bool) ([]Character, error) {
	where, args := "", []any(nil)
	if filter {
		where = "WHERE c.user_id = ?"
		args = append(args, userID)
	}
	rows, err := s.db.Query(`
SELECT c.character_id, c.user_id, c.name, c.account, c.added_at, COALESCE(t.scopes, ''), COALESCE(t.expires_at, 0)
FROM characters c
LEFT JOIN tokens t ON t.character_id = c.character_id
LEFT JOIN account_order ao ON ao.account = c.account AND ao.user_id = c.user_id
`+where+`
ORDER BY COALESCE(ao.position, 999999), c.account, c.sort_order, c.added_at`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Character
	for rows.Next() {
		var ch Character
		var scopes string
		var addedAt, tokenExp int64
		if err := rows.Scan(&ch.ID, &ch.UserID, &ch.Name, &ch.Account, &addedAt, &scopes, &tokenExp); err != nil {
			return nil, err
		}
		ch.AddedAt = time.Unix(addedAt, 0)
		ch.TokenExp = time.Unix(tokenExp, 0)
		ch.Scopes = strings.Fields(scopes)
		out = append(out, ch)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	tags, err := s.allTags()
	if err != nil {
		return nil, err
	}
	for i := range out {
		out[i].Tags = tags[out[i].ID]
	}
	return out, nil
}

// DeleteCharacter removes a character, its tokens and its corporation
// consent: убрав персонажа из кабинета, человек перестаёт им делиться.
func (s *Store) DeleteCharacter(characterID int64) error {
	_, err := s.db.Exec(`DELETE FROM characters WHERE character_id = ?`, characterID)
	if err == nil {
		_, err = s.db.Exec(`DELETE FROM tokens WHERE character_id = ?`, characterID)
	}
	if err == nil {
		err = s.DeleteCharShare(characterID)
	}
	return err
}
