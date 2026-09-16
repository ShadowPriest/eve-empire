package store

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

// openAt открывает базу по готовому пути (миграция включена).
func openAt(t *testing.T, path string) *Store {
	t.Helper()
	s, err := Open(path, make([]byte, 32))
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestMigrateCabinetEmptyDB(t *testing.T) {
	s := testStore(t)

	for _, tbl := range []string{"users", "sessions", "user_settings"} {
		var n int
		if err := s.db.QueryRow(
			`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name = ?`, tbl).Scan(&n); err != nil {
			t.Fatalf("sqlite_master %s: %v", tbl, err)
		}
		if n != 1 {
			t.Fatalf("таблица %s не создана", tbl)
		}
	}
	n, err := s.UserCount()
	if err != nil {
		t.Fatalf("UserCount: %v", err)
	}
	if n != 0 {
		t.Fatalf("пустая база: кабинетов %d, ждали 0", n)
	}
	chars, err := s.AllCharacters()
	if err != nil || len(chars) != 0 {
		t.Fatalf("AllCharacters на пустой базе: %v / %d", err, len(chars))
	}
}

// makeLegacyDB создаёт базу в том виде, в каком она была до кабинета:
// characters без user_id, account_order/account_omega с ключом-меткой,
// app_settings вперемешку с инстансовыми ключами.
func makeLegacyDB(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "legacy.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(`
CREATE TABLE characters (
    character_id INTEGER PRIMARY KEY,
    name         TEXT NOT NULL,
    added_at     INTEGER NOT NULL,
    account      TEXT NOT NULL DEFAULT '',
    sort_order   INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE account_order (
    account  TEXT PRIMARY KEY,
    position INTEGER NOT NULL
);
CREATE TABLE account_omega (
    account     TEXT PRIMARY KEY,
    omega_until TEXT NOT NULL DEFAULT '',
    mct1_until  TEXT NOT NULL DEFAULT '',
    mct2_until  TEXT NOT NULL DEFAULT ''
);
CREATE TABLE app_settings (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);
INSERT INTO characters (character_id, name, added_at, account, sort_order) VALUES
    (900000001, 'Testus Primus',  100, 'Alpha', 0),
    (900000002, 'Testus Secundus',101, 'Alpha', 1),
    (900000003, 'Testus Tertius', 102, 'Beta',  0);
INSERT INTO account_order (account, position) VALUES ('Beta', 0), ('Alpha', 1);
INSERT INTO account_omega (account, omega_until) VALUES ('Alpha', '2030-01-01');
INSERT INTO app_settings (key, value) VALUES
    ('language', 'ru'),
    ('refine_struct', 'tatara'),
    ('refresher.mult.skills', '4');
`); err != nil {
		t.Fatalf("наполнение старой схемы: %v", err)
	}
	return path
}

func TestMigrateCabinetLegacyDB(t *testing.T) {
	path := makeLegacyDB(t)
	s := openAt(t, path)

	u, err := s.User(1)
	if err != nil {
		t.Fatalf("User(1): %v", err)
	}
	if u == nil || !u.Admin {
		t.Fatalf("пользователь 1: %+v, ждали администратора", u)
	}
	if n, _ := s.UserCount(); n != 1 {
		t.Fatalf("кабинетов %d, ждали 1", n)
	}

	// Персонажи достались пользователю 1 и видны через Characters(1).
	chars, err := s.Characters(1)
	if err != nil {
		t.Fatalf("Characters(1): %v", err)
	}
	if len(chars) != 3 {
		t.Fatalf("персонажей у кабинета 1: %d, ждали 3", len(chars))
	}
	for _, c := range chars {
		if c.UserID != 1 {
			t.Fatalf("персонаж %d с user_id=%d", c.ID, c.UserID)
		}
	}
	// Порядок сохранён: account_order ставит Beta впереди Alpha.
	if chars[0].Account != "Beta" {
		t.Fatalf("порядок сбит: первым %q", chars[0].Account)
	}
	if other, _ := s.Characters(2); len(other) != 0 {
		t.Fatalf("чужой кабинет видит %d персонажей", len(other))
	}
	if all, _ := s.AllCharacters(); len(all) != 3 {
		t.Fatalf("AllCharacters: %d, ждали 3", len(all))
	}

	// Метки аккаунтов переехали вместе с user_id.
	if uid := rowUserID(t, s, `SELECT user_id FROM account_order WHERE account = 'Beta'`); uid != 1 {
		t.Fatalf("account_order.user_id = %d", uid)
	}
	omegas, err := s.AccountOmegas(1)
	if err != nil {
		t.Fatalf("AccountOmegas: %v", err)
	}
	if omegas["Alpha"].OmegaUntil != "2030-01-01" {
		t.Fatalf("омега потерялась: %+v", omegas)
	}

	// Пользовательские ключи уехали, инстансовые остались.
	if v := s.UserSetting(1, "language"); v != "ru" {
		t.Fatalf("language в user_settings = %q", v)
	}
	if v := s.Setting("language"); v != "" {
		t.Fatalf("language остался в app_settings: %q", v)
	}
	if v := s.UserSetting(1, "refine_struct"); v != "tatara" {
		t.Fatalf("refine_struct в user_settings = %q", v)
	}
	if v := s.Setting("refine_struct"); v != "" {
		t.Fatalf("refine_struct остался в app_settings: %q", v)
	}
	if v := s.Setting("refresher.mult.skills"); v != "4" {
		t.Fatalf("инстансовый ключ потерян: %q", v)
	}
	if v := s.UserSetting(1, "refresher.mult.skills"); v != "" {
		t.Fatalf("инстансовый ключ уехал в кабинет: %q", v)
	}

	// Первичный ключ стал составным: одна и та же метка живёт у двух
	// кабинетов независимо.
	if err := s.SetAccountOmega(2, AccountOmega{Account: "Alpha", OmegaUntil: "2031-02-02"}); err != nil {
		t.Fatalf("SetAccountOmega(2): %v", err)
	}
	if err := s.SaveSidebarOrder(2, []SidebarGroup{{Account: "Alpha"}}); err != nil {
		t.Fatalf("SaveSidebarOrder(2): %v", err)
	}
	if err := s.SetFarmRoster(2, []string{"Alpha"}, nil); err != nil {
		t.Fatalf("SetFarmRoster(2): %v", err)
	}
	if err := s.SetFarmRoster(1, []string{"Alpha"}, nil); err != nil {
		t.Fatalf("SetFarmRoster(1): %v", err)
	}
	if o, _ := s.AccountOmegas(1); o["Alpha"].OmegaUntil != "2030-01-01" {
		t.Fatalf("омега кабинета 1 затёрта кабинетом 2: %+v", o)
	}
	for _, uid := range []int64{1, 2} {
		acc, err := s.FarmAccounts(uid)
		if err != nil || !acc["Alpha"] {
			t.Fatalf("FarmAccounts(%d) = %v / %v", uid, acc, err)
		}
	}
}

func rowUserID(t *testing.T, s *Store, q string) int64 {
	t.Helper()
	var uid int64
	if err := s.db.QueryRow(q).Scan(&uid); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return uid
}

func TestMigrateCabinetIdempotent(t *testing.T) {
	path := makeLegacyDB(t)

	first := openAt(t, path)
	before := schemaAndData(t, first)
	first.Close()

	second := openAt(t, path)
	after := schemaAndData(t, second)

	if before != after {
		t.Fatalf("повторная миграция изменила базу:\n%s\n---\n%s", before, after)
	}
	if n, _ := second.UserCount(); n != 1 {
		t.Fatalf("после второго открытия кабинетов %d, ждали 1", n)
	}
}

// schemaAndData снимает отпечаток схемы и содержимого мигрируемых таблиц.
func schemaAndData(t *testing.T, s *Store) string {
	t.Helper()
	out := ""
	rows, err := s.db.Query(`SELECT name, sql FROM sqlite_master WHERE type='table' ORDER BY name`)
	if err != nil {
		t.Fatalf("sqlite_master: %v", err)
	}
	for rows.Next() {
		var name string
		var ddl sql.NullString
		if err := rows.Scan(&name, &ddl); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out += name + ": " + ddl.String + "\n"
	}
	rows.Close()
	for _, q := range []string{
		`SELECT character_id, user_id, name, account FROM characters ORDER BY character_id`,
		`SELECT user_id, account, position FROM account_order ORDER BY user_id, account`,
		`SELECT user_id, account, omega_until FROM account_omega ORDER BY user_id, account`,
		`SELECT id, created_at > 0, admin FROM users ORDER BY id`,
		`SELECT user_id, key, value FROM user_settings ORDER BY user_id, key`,
		`SELECT key, value FROM app_settings ORDER BY key`,
	} {
		out += "-- " + q + "\n" + dumpQuery(t, s, q)
	}
	return out
}

func dumpQuery(t *testing.T, s *Store, q string) string {
	t.Helper()
	rows, err := s.db.Query(q)
	if err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	defer rows.Close()
	cols, _ := rows.Columns()
	out := ""
	for rows.Next() {
		cells := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range cells {
			ptrs[i] = &cells[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			t.Fatalf("scan %s: %v", q, err)
		}
		for _, c := range cells {
			if b, ok := c.([]byte); ok {
				c = string(b)
			}
			out += fmt.Sprintf("%v|", c)
		}
		out += "\n"
	}
	return out
}

func TestSessions(t *testing.T) {
	s := testStore(t)

	uid, err := s.CreateUser(true)
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	raw, err := s.CreateSession(uid, "тестовый агент", time.Hour)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if raw == "" {
		t.Fatal("пустой id сессии")
	}
	// В базе лежит хэш, а не сам id.
	var stored string
	if err := s.db.QueryRow(`SELECT id_hash FROM sessions`).Scan(&stored); err != nil {
		t.Fatalf("чтение сессии: %v", err)
	}
	if stored == raw {
		t.Fatal("в базе лежит сам id сессии")
	}

	got, ok, err := s.SessionUser(raw)
	if err != nil || !ok || got != uid {
		t.Fatalf("SessionUser: %d / %v / %v", got, ok, err)
	}
	if _, ok, _ := s.SessionUser(raw + "x"); ok {
		t.Fatal("чужой id нашёл сессию")
	}

	if err := s.TouchSession(raw); err != nil {
		t.Fatalf("TouchSession: %v", err)
	}
	var created, seen int64
	if err := s.db.QueryRow(`SELECT created_at, last_seen FROM sessions`).Scan(&created, &seen); err != nil {
		t.Fatalf("чтение меток: %v", err)
	}
	if seen < created {
		t.Fatalf("last_seen %d < created_at %d", seen, created)
	}

	if err := s.DeleteSession(raw); err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}
	if _, ok, _ := s.SessionUser(raw); ok {
		t.Fatal("удалённая сессия находится")
	}

	// Просроченная не находится, PurgeSessions её выбрасывает.
	dead, err := s.CreateSession(uid, "", -time.Minute)
	if err != nil {
		t.Fatalf("CreateSession (просроченная): %v", err)
	}
	if _, ok, _ := s.SessionUser(dead); ok {
		t.Fatal("просроченная сессия находится")
	}
	live, err := s.CreateSession(uid, "", time.Hour)
	if err != nil {
		t.Fatalf("CreateSession (живая): %v", err)
	}
	if err := s.PurgeSessions(); err != nil {
		t.Fatalf("PurgeSessions: %v", err)
	}
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM sessions`).Scan(&n); err != nil {
		t.Fatalf("COUNT: %v", err)
	}
	if n != 1 {
		t.Fatalf("после PurgeSessions осталось %d сессий, ждали 1", n)
	}
	if _, ok, _ := s.SessionUser(live); !ok {
		t.Fatal("живая сессия потерялась")
	}

	if err := s.DeleteUserSessions(uid); err != nil {
		t.Fatalf("DeleteUserSessions: %v", err)
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM sessions`).Scan(&n); err != nil {
		t.Fatalf("COUNT: %v", err)
	}
	if n != 0 {
		t.Fatalf("после выхода везде осталось %d сессий", n)
	}
}

func TestUpsertCharacterKeepsOwner(t *testing.T) {
	s := testStore(t)

	one, _ := s.CreateUser(true)
	two, _ := s.CreateUser(false)
	const charID = 900000042

	if err := s.UpsertCharacter(one, charID, "Testus Quartus", "rt", "at",
		time.Now().Add(time.Hour), []string{"publicData"}, "client"); err != nil {
		t.Fatalf("UpsertCharacter: %v", err)
	}
	uid, ok, err := s.UserOfCharacter(charID)
	if err != nil || !ok || uid != one {
		t.Fatalf("UserOfCharacter: %d / %v / %v", uid, ok, err)
	}

	// Повторный вход чужим кабинетом владельца не меняет.
	if err := s.UpsertCharacter(two, charID, "Testus Quartus", "rt2", "at2",
		time.Now().Add(time.Hour), []string{"publicData"}, "client"); err != nil {
		t.Fatalf("UpsertCharacter (повтор): %v", err)
	}
	if uid, _, _ := s.UserOfCharacter(charID); uid != one {
		t.Fatalf("владелец сменился на %d", uid)
	}

	// Явный перенос — только через AttachCharacter.
	if err := s.AttachCharacter(charID, two); err != nil {
		t.Fatalf("AttachCharacter: %v", err)
	}
	if uid, _, _ := s.UserOfCharacter(charID); uid != two {
		t.Fatalf("AttachCharacter не перенёс: %d", uid)
	}
	if _, ok, _ := s.UserOfCharacter(1); ok {
		t.Fatal("несуществующий персонаж нашёлся")
	}
}
