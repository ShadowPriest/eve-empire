// Package store persists characters and their (encrypted) SSO tokens
// in a local SQLite database.
package store

import (
	"database/sql"
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
	Name     string
	Account  string   // user-assigned account label for sidebar grouping
	Tags     []string // user-assigned tags for sidebar filtering
	AddedAt  time.Time
	Scopes   []string
	TokenExp time.Time
}

func Open(path string, encryptionKey []byte) (*Store, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
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
	} {
		if _, err := s.db.Exec(ddl); err != nil && !strings.Contains(err.Error(), "duplicate column") {
			return err
		}
	}
	_, err = s.db.Exec(`
CREATE TABLE IF NOT EXISTS account_order (
    account  TEXT PRIMARY KEY,
    position INTEGER NOT NULL
);
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
)`)
	if err != nil {
		return err
	}
	return s.migrateHistory()
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

func (s *Store) PITemplates() ([]PITemplate, error) {
	rows, err := s.db.Query(`SELECT id, name, planet_type, product_type, cmd_ctr_lv, payload, created_at
		FROM pi_templates ORDER BY created_at DESC`)
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

func (s *Store) AddPITemplate(t PITemplate) (int64, error) {
	res, err := s.db.Exec(`INSERT INTO pi_templates
		(name, planet_type, product_type, cmd_ctr_lv, payload, created_at) VALUES (?,?,?,?,?,?)`,
		t.Name, t.PlanetType, t.ProductType, t.CmdCtrLv, t.Payload, time.Now().Unix())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) DeletePITemplate(id int64) error {
	_, err := s.db.Exec(`DELETE FROM pi_templates WHERE id = ?`, id)
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

func (s *Store) SkillPlans() ([]SkillPlan, error) {
	rows, err := s.db.Query(`SELECT id, name, character_id, body, created_at, updated_at
		FROM skill_plans ORDER BY updated_at DESC`)
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
func (s *Store) SaveSkillPlan(p SkillPlan) (int64, error) {
	now := time.Now().Unix()
	if p.ID > 0 {
		_, err := s.db.Exec(`UPDATE skill_plans SET name = ?, character_id = ?, body = ?, updated_at = ?
			WHERE id = ?`, p.Name, p.CharacterID, p.Body, now, p.ID)
		return p.ID, err
	}
	res, err := s.db.Exec(`INSERT INTO skill_plans (name, character_id, body, created_at, updated_at)
		VALUES (?,?,?,?,?)`, p.Name, p.CharacterID, p.Body, now, now)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) DeleteSkillPlan(id int64) error {
	_, err := s.db.Exec(`DELETE FROM skill_plans WHERE id = ?`, id)
	return err
}

// Setting returns an app setting value ("" when unset).
func (s *Store) Setting(key string) string {
	var v string
	_ = s.db.QueryRow(`SELECT value FROM app_settings WHERE key = ?`, key).Scan(&v)
	return v
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

func (s *Store) CacheGet(url string) (body []byte, pages int, expires time.Time, ok bool) {
	var exp int64
	err := s.db.QueryRow(`SELECT body, pages, expires FROM esi_cache WHERE url = ?`, url).
		Scan(&body, &pages, &exp)
	if err != nil {
		return nil, 0, time.Time{}, false
	}
	return body, pages, time.Unix(exp, 0), true
}

func (s *Store) CachePut(url string, body []byte, pages int, expires time.Time) {
	_, _ = s.db.Exec(`
INSERT INTO esi_cache (url, body, pages, expires) VALUES (?, ?, ?, ?)
ON CONFLICT(url) DO UPDATE SET body = excluded.body, pages = excluded.pages, expires = excluded.expires`,
		url, body, pages, expires.Unix())
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
func (s *Store) SaveSidebarOrder(groups []SidebarGroup) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`DELETE FROM account_order`); err != nil {
		return err
	}
	for gi, g := range groups {
		if _, err := tx.Exec(`INSERT INTO account_order (account, position) VALUES (?, ?)`, g.Account, gi); err != nil {
			return err
		}
		for ci, id := range g.Chars {
			if _, err := tx.Exec(`UPDATE characters SET account = ?, sort_order = ? WHERE character_id = ?`,
				g.Account, ci, id); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

// SetAccount updates the user-assigned account label of a character.
func (s *Store) SetAccount(characterID int64, account string) error {
	_, err := s.db.Exec(`UPDATE characters SET account = ? WHERE character_id = ?`, account, characterID)
	return err
}

// UpsertCharacter stores/updates a character and its tokens after login.
func (s *Store) UpsertCharacter(id int64, name string, refreshToken, accessToken string, expiresAt time.Time, scopes []string, clientID string) error {
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
INSERT INTO characters (character_id, name, added_at) VALUES (?, ?, ?)
ON CONFLICT(character_id) DO UPDATE SET name = excluded.name`, id, name, time.Now().Unix()); err != nil {
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

// AccessToken returns a cached access token and its expiry.
func (s *Store) AccessToken(characterID int64) (string, time.Time, error) {
	var tok string
	var exp int64
	err := s.db.QueryRow(`SELECT access_token, expires_at FROM tokens WHERE character_id = ?`, characterID).Scan(&tok, &exp)
	return tok, time.Unix(exp, 0), err
}

// Characters lists all stored characters.
func (s *Store) Characters() ([]Character, error) {
	rows, err := s.db.Query(`
SELECT c.character_id, c.name, c.account, c.added_at, COALESCE(t.scopes, ''), COALESCE(t.expires_at, 0)
FROM characters c
LEFT JOIN tokens t ON t.character_id = c.character_id
LEFT JOIN account_order ao ON ao.account = c.account
ORDER BY COALESCE(ao.position, 999999), c.account, c.sort_order, c.added_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Character
	for rows.Next() {
		var ch Character
		var scopes string
		var addedAt, tokenExp int64
		if err := rows.Scan(&ch.ID, &ch.Name, &ch.Account, &addedAt, &scopes, &tokenExp); err != nil {
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

// DeleteCharacter removes a character and its tokens.
func (s *Store) DeleteCharacter(characterID int64) error {
	_, err := s.db.Exec(`DELETE FROM characters WHERE character_id = ?`, characterID)
	if err == nil {
		_, err = s.db.Exec(`DELETE FROM tokens WHERE character_id = ?`, characterID)
	}
	return err
}
