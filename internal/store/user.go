package store

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
)

// Кабинет (этап 1: только схема, поведение сервиса прежнее).
//
// Личность — это множество персонажей: таблица `users` без имени и пароля,
// персонаж принадлежит ровно одному кабинету (`characters.user_id`).
// Таблицы, у которых своего владельца не было (метки аккаунтов, шаблоны
// планетарки, планы навыков, данные SP-фермы), получают `user_id`; там, где
// ключом была метка аккаунта, первичный ключ становится `(user_id, account)`.
// Таблицы, чей владелец выводится через персонажа или общие/публичные
// (`tokens`, `esi_cache`, `hist_*`, `acc_*`, …), не трогаются.
//
// Разовая миграция старой однопользовательской базы: всё, что в ней есть,
// достаётся пользователю 1 (администратору).

// User — кабинет. Ни имени, ни пароля: личность подтверждается входом
// через EVE SSO любым из своих персонажей.
type User struct {
	ID        int64
	CreatedAt time.Time
	Admin     bool
}

// userSettingKeys — ключи app_settings, которые принадлежат кабинету, а не
// инстансу. Инстансовые (`refresher.mult.*`) остаются в app_settings.
var userSettingKeys = []string{
	"language",
	"route_recent",
	"route_tree",
	"spfarm_params",
	"acc.closed_before",
	"air_reset_at",
	"build_broker",
	"build_sales",
	"build_struct",
	"build_system",
	"build_tax",
}

// userSettingPrefixes — пользовательские ключи с переменным хвостом.
var userSettingPrefixes = []string{"refine_"}

// accountKeyedTables — таблицы, у которых первичным ключом была метка
// аккаунта: их надо пересобрать на составной ключ (user_id, account).
var accountKeyedTables = []string{"account_order", "account_omega", "spfarm_account"}

// userIDTables — таблицы, которым достаточно обычного ALTER.
var userIDTables = []string{"pi_templates", "skill_plans", "spfarm_offer", "spfarm_plan", "plex_purchase"}

// migrateCabinet добавляет user_id всем таблицам кабинета и раздаёт данные
// старой однопользовательской базы пользователю 1. Идемпотентна: повторный
// запуск ничего не меняет.
func (s *Store) migrateCabinet() error {
	for _, t := range accountKeyedTables {
		if err := s.rebuildAccountTable(t); err != nil {
			return fmt.Errorf("%s: %w", t, err)
		}
	}
	for _, t := range userIDTables {
		ddl := fmt.Sprintf(`ALTER TABLE %s ADD COLUMN user_id INTEGER NOT NULL DEFAULT 0`, t)
		if _, err := s.db.Exec(ddl); err != nil && !strings.Contains(err.Error(), "duplicate column") {
			return err
		}
	}
	return s.migrateCabinetData()
}

// hasColumn говорит, есть ли у таблицы колонка. Имена таблиц здесь — свои
// константы, не пользовательский ввод.
func (s *Store) hasColumn(table, column string) (bool, error) {
	rows, err := s.db.Query(fmt.Sprintf(`PRAGMA table_info(%s)`, table))
	if err != nil {
		return false, err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return false, err
	}
	for rows.Next() {
		cells := make([]any, len(cols))
		var name string
		for i := range cells {
			if cols[i] == "name" {
				cells[i] = &name
			} else {
				cells[i] = new(sql.RawBytes)
			}
		}
		if err := rows.Scan(cells...); err != nil {
			return false, err
		}
		if name == column {
			return true, rows.Err()
		}
	}
	return false, rows.Err()
}

// rebuildAccountTable пересобирает таблицу, ключом которой была метка
// аккаунта: SQLite не умеет менять первичный ключ через ALTER, поэтому
// создаём новую, переливаем и переименовываем.
func (s *Store) rebuildAccountTable(table string) error {
	has, err := s.hasColumn(table, "user_id")
	if err != nil || has {
		return err
	}
	var body, cols string
	switch table {
	case "account_order":
		body = `user_id INTEGER NOT NULL DEFAULT 0, account TEXT NOT NULL, position INTEGER NOT NULL,
			PRIMARY KEY (user_id, account)`
		cols = `account, position`
	case "account_omega":
		body = `user_id INTEGER NOT NULL DEFAULT 0, account TEXT NOT NULL,
			omega_until TEXT NOT NULL DEFAULT '', mct1_until TEXT NOT NULL DEFAULT '',
			mct2_until TEXT NOT NULL DEFAULT '', PRIMARY KEY (user_id, account)`
		cols = `account, omega_until, mct1_until, mct2_until`
	case "spfarm_account":
		body = `user_id INTEGER NOT NULL DEFAULT 0, account TEXT NOT NULL,
			PRIMARY KEY (user_id, account)`
		cols = `account`
	default:
		return fmt.Errorf("неизвестная таблица %q", table)
	}

	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, q := range []string{
		fmt.Sprintf(`CREATE TABLE %s_new (%s)`, table, body),
		fmt.Sprintf(`INSERT INTO %s_new (user_id, %s) SELECT 0, %s FROM %s`, table, cols, cols, table),
		fmt.Sprintf(`DROP TABLE %s`, table),
		fmt.Sprintf(`ALTER TABLE %s_new RENAME TO %s`, table, table),
	} {
		if _, err := tx.Exec(q); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// migrateCabinetData отдаёт содержимое старой однопользовательской базы
// пользователю 1: создаёт его, если персонажи есть, а кабинетов ещё нет,
// проставляет user_id там, где он нулевой, и переносит пользовательские
// ключи из app_settings в user_settings.
func (s *Store) migrateCabinetData() error {
	var chars, users int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM characters`).Scan(&chars); err != nil {
		return err
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&users); err != nil {
		return err
	}
	if users == 0 {
		if chars == 0 {
			return nil // пустая база: кабинет заведёт первый вход
		}
		if _, err := s.db.Exec(`INSERT INTO users (id, created_at, admin) VALUES (1, ?, 1)`,
			time.Now().Unix()); err != nil {
			return err
		}
	}
	// Дальше — только если пользователь 1 действительно есть: таблица могла
	// появиться позже, и досыпать в неё нулевые user_id надо и тогда.
	var exists int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM users WHERE id = 1`).Scan(&exists); err != nil {
		return err
	}
	if exists == 0 {
		return nil
	}

	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	tables := append([]string{"characters"}, accountKeyedTables...)
	tables = append(tables, userIDTables...)
	for _, t := range tables {
		if _, err := tx.Exec(fmt.Sprintf(`UPDATE %s SET user_id = 1 WHERE user_id = 0`, t)); err != nil {
			return err
		}
	}
	for _, key := range userSettingKeys {
		if _, err := tx.Exec(`INSERT INTO user_settings (user_id, key, value)
			SELECT 1, key, value FROM app_settings WHERE key = ?
			ON CONFLICT(user_id, key) DO NOTHING`, key); err != nil {
			return err
		}
		if _, err := tx.Exec(`DELETE FROM app_settings WHERE key = ?`, key); err != nil {
			return err
		}
	}
	for _, p := range userSettingPrefixes {
		if _, err := tx.Exec(`INSERT INTO user_settings (user_id, key, value)
			SELECT 1, key, value FROM app_settings WHERE key LIKE ? || '%'
			ON CONFLICT(user_id, key) DO NOTHING`, p); err != nil {
			return err
		}
		if _, err := tx.Exec(`DELETE FROM app_settings WHERE key LIKE ? || '%'`, p); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ── пользователи ─────────────────────────────────────────────────────

// CreateUser заводит пустой кабинет и возвращает его id.
func (s *Store) CreateUser(admin bool) (int64, error) {
	a := 0
	if admin {
		a = 1
	}
	res, err := s.db.Exec(`INSERT INTO users (created_at, admin) VALUES (?, ?)`, time.Now().Unix(), a)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// User возвращает кабинет по id; (nil, nil) — такого кабинета нет.
func (s *Store) User(id int64) (*User, error) {
	var u User
	var created int64
	var admin int
	err := s.db.QueryRow(`SELECT id, created_at, admin FROM users WHERE id = ?`, id).
		Scan(&u.ID, &created, &admin)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	u.CreatedAt, u.Admin = time.Unix(created, 0), admin != 0
	return &u, nil
}

// Users перечисляет кабинеты по возрастанию id. Нужно фоновым задачам,
// у которых нет запроса: они обходят все кабинеты подряд.
func (s *Store) Users() ([]User, error) {
	rows, err := s.db.Query(`SELECT id, created_at, admin FROM users ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []User
	for rows.Next() {
		var u User
		var created int64
		var admin int
		if err := rows.Scan(&u.ID, &created, &admin); err != nil {
			return nil, err
		}
		u.CreatedAt, u.Admin = time.Unix(created, 0), admin != 0
		out = append(out, u)
	}
	return out, rows.Err()
}

// AdminUser — первый администратор (по id). Его настройки считаются
// настройками инстанса там, где запроса нет: язык фоновых задач и CLI.
// (nil, nil) — администратора ещё нет.
func (s *Store) AdminUser() (*User, error) {
	var u User
	var created int64
	err := s.db.QueryRow(`SELECT id, created_at FROM users WHERE admin = 1 ORDER BY id LIMIT 1`).
		Scan(&u.ID, &created)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	u.CreatedAt, u.Admin = time.Unix(created, 0), true
	return &u, nil
}

// AdminSetting — настройка первого администратора ("" когда админа нет).
// Так фоновые задачи и CLI берут язык ESI: своего кабинета у них нет.
func (s *Store) AdminSetting(key string) string {
	u, err := s.AdminUser()
	if err != nil || u == nil {
		return ""
	}
	return s.UserSetting(u.ID, key)
}

// UserCount — сколько кабинетов заведено (политика регистрации `first`).
func (s *Store) UserCount() (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&n)
	return n, err
}

// UserOfCharacter возвращает кабинет персонажа; ok=false — персонаж
// вообще не заведён. user_id=0 значит «персонаж есть, но не привязан».
func (s *Store) UserOfCharacter(charID int64) (int64, bool, error) {
	var uid int64
	err := s.db.QueryRow(`SELECT user_id FROM characters WHERE character_id = ?`, charID).Scan(&uid)
	if err == sql.ErrNoRows {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return uid, true, nil
}

// AttachCharacter переводит персонажа в кабинет. Это единственный способ
// сменить владельца: UpsertCharacter чужого персонажа не забирает.
func (s *Store) AttachCharacter(charID, userID int64) error {
	_, err := s.db.Exec(`UPDATE characters SET user_id = ? WHERE character_id = ?`, userID, charID)
	return err
}

// ── сессии ───────────────────────────────────────────────────────────

// sessionHash — то, что лежит в базе: hex SHA-256 от id из куки. Сам id
// не хранится, поэтому украденная база не даёт войти.
func sessionHash(rawID string) string {
	sum := sha256.Sum256([]byte(rawID))
	return hex.EncodeToString(sum[:])
}

// CreateSession заводит сессию и возвращает id для куки (base64url).
func (s *Store) CreateSession(userID int64, userAgent string, ttl time.Duration) (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	raw := base64.RawURLEncoding.EncodeToString(buf)
	now := time.Now()
	_, err := s.db.Exec(`INSERT INTO sessions (id_hash, user_id, created_at, last_seen, expires_at, user_agent)
		VALUES (?, ?, ?, ?, ?, ?)`,
		sessionHash(raw), userID, now.Unix(), now.Unix(), now.Add(ttl).Unix(), userAgent)
	if err != nil {
		return "", err
	}
	return raw, nil
}

// SessionUser находит кабинет по id из куки; просроченная сессия не
// находится.
func (s *Store) SessionUser(rawID string) (int64, bool, error) {
	if rawID == "" {
		return 0, false, nil
	}
	var uid int64
	err := s.db.QueryRow(`SELECT user_id FROM sessions WHERE id_hash = ? AND expires_at > ?`,
		sessionHash(rawID), time.Now().Unix()).Scan(&uid)
	if err == sql.ErrNoRows {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return uid, true, nil
}

// TouchSession отмечает, что сессией только что пользовались.
func (s *Store) TouchSession(rawID string) error {
	_, err := s.db.Exec(`UPDATE sessions SET last_seen = ? WHERE id_hash = ?`,
		time.Now().Unix(), sessionHash(rawID))
	return err
}

// DeleteSession — выход на этом устройстве.
func (s *Store) DeleteSession(rawID string) error {
	_, err := s.db.Exec(`DELETE FROM sessions WHERE id_hash = ?`, sessionHash(rawID))
	return err
}

// DeleteUserSessions — выход везде.
func (s *Store) DeleteUserSessions(userID int64) error {
	_, err := s.db.Exec(`DELETE FROM sessions WHERE user_id = ?`, userID)
	return err
}

// PurgeSessions выбрасывает просроченные сессии.
func (s *Store) PurgeSessions() error {
	_, err := s.db.Exec(`DELETE FROM sessions WHERE expires_at <= ?`, time.Now().Unix())
	return err
}

// ── настройки кабинета ───────────────────────────────────────────────

// UserSetting возвращает настройку кабинета ("" когда не задана).
func (s *Store) UserSetting(userID int64, key string) string {
	var v string
	_ = s.db.QueryRow(`SELECT value FROM user_settings WHERE user_id = ? AND key = ?`, userID, key).Scan(&v)
	return v
}

// SetUserSetting сохраняет настройку кабинета.
func (s *Store) SetUserSetting(userID int64, key, value string) error {
	_, err := s.db.Exec(`INSERT INTO user_settings (user_id, key, value) VALUES (?, ?, ?)
		ON CONFLICT(user_id, key) DO UPDATE SET value = excluded.value`, userID, key, value)
	return err
}

// UserSettingsPrefix возвращает настройки кабинета с общим префиксом ключа.
func (s *Store) UserSettingsPrefix(userID int64, prefix string) map[string]string {
	out := map[string]string{}
	rows, err := s.db.Query(`SELECT key, value FROM user_settings WHERE user_id = ? AND key LIKE ? || '%'`,
		userID, prefix)
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
