package store

import (
	"strconv"
	"time"
)

// Программа AIR: шкала прогресса за месяц. ESI её не отдаёт вообще
// (эндпоинта нет, как и для Homefront), поэтому счётчики вводятся руками
// со страницы AIR, а закрытие месяца пишет историю сюда — SP фиксируется
// готовым числом на момент закрытия: омега-статус — факт того месяца,
// а награды CCP может и поменять.
//
// В игре прогресс — счётчик «X из 30», а не набор дат (пропущенный день
// просто не двигает шкалу), поэтому храним числа, плитки на странице —
// только визуализация.

// Награды AIR: 10 000 SP за каждый день, финальная веха на 15-м дне —
// 75 000 SP альфе и ещё 150 000 SP омеге.
const (
	AirMonthDays   = 30
	AirYearMonths  = 12
	AirDaySP       = 10_000
	AirFinalDay    = 15
	AirFinalSP     = 75_000
	AirFinalOmega  = 150_000
	airResetPeriod = AirMonthDays * 24 * time.Hour
)

// AirMonthSP считает SP за месяц AIR по числу выполненных дней.
func AirMonthSP(days int, omega bool) int64 {
	sp := int64(days) * AirDaySP
	if days >= AirFinalDay {
		sp += AirFinalSP
		if omega {
			sp += AirFinalOmega
		}
	}
	return sp
}

func (s *Store) migrateAir() error {
	_, err := s.db.Exec(`
CREATE TABLE IF NOT EXISTS air_state (
    character_id INTEGER PRIMARY KEY,
    month_no     INTEGER NOT NULL DEFAULT 1, -- 1..12
    days_done    INTEGER NOT NULL DEFAULT 0  -- 0..30
);
CREATE TABLE IF NOT EXISTS air_month (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    character_id INTEGER NOT NULL,
    closed_at    INTEGER NOT NULL, -- unix seconds
    month_no     INTEGER NOT NULL,
    days_done    INTEGER NOT NULL,
    omega        INTEGER NOT NULL, -- статус на момент закрытия
    sp           INTEGER NOT NULL  -- зафиксированный итог месяца
)`)
	return err
}

// AirState is the live AIR progress of one character.
type AirState struct {
	MonthNo  int
	DaysDone int
}

// AirStates returns the live progress keyed by character; a character
// without a row is month 1, day 0.
func (s *Store) AirStates() (map[int64]AirState, error) {
	rows, err := s.db.Query(`SELECT character_id, month_no, days_done FROM air_state`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]AirState{}
	for rows.Next() {
		var id int64
		var st AirState
		if err := rows.Scan(&id, &st.MonthNo, &st.DaysDone); err != nil {
			return nil, err
		}
		out[id] = st
	}
	return out, rows.Err()
}

func clampAir(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// SetAirDays sets the day counter of one character (a tile click).
func (s *Store) SetAirDays(characterID int64, days int) error {
	days = clampAir(days, 0, AirMonthDays)
	_, err := s.db.Exec(`INSERT INTO air_state (character_id, month_no, days_done)
		VALUES (?, 1, ?)
		ON CONFLICT(character_id) DO UPDATE SET days_done = excluded.days_done`,
		characterID, days)
	return err
}

// SetAirMonth sets the month counter of one character (a tile click).
func (s *Store) SetAirMonth(characterID int64, month int) error {
	month = clampAir(month, 1, AirYearMonths)
	_, err := s.db.Exec(`INSERT INTO air_state (character_id, month_no, days_done)
		VALUES (?, ?, 0)
		ON CONFLICT(character_id) DO UPDATE SET month_no = excluded.month_no`,
		characterID, month)
	return err
}

// AirMonthRow is one closed AIR month of one character.
type AirMonthRow struct {
	CharacterID int64
	ClosedAt    time.Time
	MonthNo     int
	DaysDone    int
	Omega       bool
	SP          int64
}

// AirMonths returns the whole closed-month history, newest first —
// what the page and any other consumer read the totals from.
func (s *Store) AirMonths() ([]AirMonthRow, error) {
	rows, err := s.db.Query(`SELECT character_id, closed_at, month_no, days_done, omega, sp
		FROM air_month ORDER BY closed_at DESC, id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AirMonthRow
	for rows.Next() {
		var r AirMonthRow
		var at int64
		var omega int
		if err := rows.Scan(&r.CharacterID, &at, &r.MonthNo, &r.DaysDone, &omega, &r.SP); err != nil {
			return nil, err
		}
		r.ClosedAt = time.Unix(at, 0)
		r.Omega = omega != 0
		out = append(out, r)
	}
	return out, rows.Err()
}

// airOmegaActive tells whether a stored omega date (see account_omega:
// 'YYYY-MM-DD HH:MM' or 'YYYY-MM-DD', EVE time = UTC) is still running.
// A date without a time keeps working through that whole day.
func airOmegaActive(raw string, now time.Time) bool {
	if t, err := time.Parse("2006-01-02 15:04", raw); err == nil {
		return t.After(now)
	}
	if t, err := time.Parse("2006-01-02", raw); err == nil {
		return t.Add(24 * time.Hour).After(now)
	}
	return false
}

// CloseAirMonth records the month result of EVERY character (zero-day
// rows included — the whole picture matters), resets the day counters and
// advances the month numbers, all in one transaction. Returns how many
// characters were closed.
func (s *Store) CloseAirMonth(now time.Time) (int, error) {
	omegas, err := s.AccountOmegas()
	if err != nil {
		return 0, err
	}
	states, err := s.AirStates()
	if err != nil {
		return 0, err
	}

	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	rows, err := tx.Query(`SELECT character_id, account FROM characters`)
	if err != nil {
		return 0, err
	}
	type charRow struct {
		id      int64
		account string
	}
	var chars []charRow
	for rows.Next() {
		var c charRow
		if err := rows.Scan(&c.id, &c.account); err != nil {
			rows.Close()
			return 0, err
		}
		chars = append(chars, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	for _, c := range chars {
		st := states[c.id]
		if st.MonthNo < 1 {
			st.MonthNo = 1
		}
		omega := airOmegaActive(omegas[c.account].OmegaUntil, now)
		if _, err := tx.Exec(`INSERT INTO air_month
			(character_id, closed_at, month_no, days_done, omega, sp) VALUES (?,?,?,?,?,?)`,
			c.id, now.Unix(), st.MonthNo, st.DaysDone, boolInt(omega),
			AirMonthSP(st.DaysDone, omega)); err != nil {
			return 0, err
		}
		if _, err := tx.Exec(`INSERT INTO air_state (character_id, month_no, days_done)
			VALUES (?, ?, 0)
			ON CONFLICT(character_id) DO UPDATE SET
				month_no = excluded.month_no, days_done = 0`,
			c.id, st.MonthNo%AirYearMonths+1); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return len(chars), nil
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// ── дни из валета ────────────────────────────────────────────────────

// Выполненный пункт AIR оставляет в журнале запись daily_goal_payouts,
// и на каждый пункт их две: личная и, при корп-налоге, её дубль в валете
// корпорации (персонаж там в second_party_id; в NPC-корпе дубля нет).
// reason — id пункта, и пункты разные: дневной пункт шкалы — всегда
// 1004953, задания дня — прочие id (697xxx), вехи (8M ISK) — свои.
//
// Деления шкалы двигает только дневной пункт, поэтому дни = число
// записей reason=1004953. Считать «дни с выплатами» нельзя: задания
// приходят в разное время, а накопленные пункты клеймятся пачкой одной
// секундой (сверено с живой базой: личных 1004953 ровно столько, сколько
// плиток у владельца, прочих записей — больше).
//
// СУММЫ В ЛОГИКЕ НЕ УЧАСТВУЮТ — только текст записи. Налог корпорации
// произвольно перекраивает выплаты (0% — и корп-записей нет вовсе), а
// к корп-валету может не быть доступа. Поэтому личный и корповый журнал
// считаются НЕЗАВИСИМО и побеждает больший счёт (не сумма — это дубли
// одной выплаты): личного нет у альтов с мёртвыми токенами, корпового —
// в NPC-корпе и без налога, и вдобавок корп-журнал у ESI отстаёт
// (наблюдалось 3 против 4 в личном). Короткое замыкание «нашли в корпе —
// личный не смотрим» именно поэтому не годится; читаем оба, благо это
// локальные выборки из hist_journal, а не походы в ESI.
const airDailyGoalReason = "1004953"

// airWalletWindow — окно текущего месяца AIR: от известного момента
// обновления шкалы минус 30 суток. Без таймера — от последнего закрытия.
// Совсем без ориентиров окна нет — валет не применяем, только руки.
func (s *Store) airWalletWindow(now time.Time) (from, to time.Time, ok bool) {
	if at := s.AirResetAt(); !at.IsZero() {
		return at.Add(-airResetPeriod), at, true
	}
	var closed int64
	_ = s.db.QueryRow(`SELECT COALESCE(MAX(closed_at), 0) FROM air_month`).Scan(&closed)
	if closed > 0 {
		return time.Unix(closed, 0), now, true
	}
	return time.Time{}, time.Time{}, false
}

// AirSyncWalletDays подтягивает счётчики дней из собранных журналов
// (личных и корповых) и возвращает дни по валету для сверки на странице.
// Только повышает: журнал собирается с конца июля и не у всех альтов,
// так что «в валете пусто» не значит «не выполнял». Погрешность на стыке
// месяцев возможна: пункты, скопленные в старом месяце и заклеймленные
// после обновления шкалы, попадают в новое окно — руки поправят.
func (s *Store) AirSyncWalletDays(now time.Time) (map[int64]int, error) {
	from, to, ok := s.airWalletWindow(now)
	if !ok {
		return nil, nil
	}
	wallet := map[int64]int{}
	for _, q := range []string{
		// личный журнал: владелец — сам персонаж
		`SELECT j.owner_id, COUNT(*) FROM hist_journal j
			JOIN characters c ON c.character_id = j.owner_id
			WHERE j.ref_type = 'daily_goal_payouts' AND j.reason = ?
			  AND j.division = 0 AND j.date >= ? AND j.date < ?
			GROUP BY j.owner_id`,
		// корп-дубли: персонаж в second_party_id
		`SELECT j.second_party_id, COUNT(*) FROM hist_journal j
			JOIN characters c ON c.character_id = j.second_party_id
			WHERE j.ref_type = 'daily_goal_payouts' AND j.reason = ?
			  AND j.division > 0 AND j.date >= ? AND j.date < ?
			GROUP BY j.second_party_id`,
	} {
		rows, err := s.db.Query(q, airDailyGoalReason, from.Unix(), to.Unix())
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id int64
			var days int
			if err := rows.Scan(&id, &days); err != nil {
				rows.Close()
				return nil, err
			}
			if days > wallet[id] {
				wallet[id] = days
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	for id, days := range wallet {
		if days > AirMonthDays {
			days = AirMonthDays
		}
		if _, err := s.db.Exec(`INSERT INTO air_state (character_id, month_no, days_done)
			VALUES (?, 1, ?)
			ON CONFLICT(character_id) DO UPDATE SET
				days_done = MAX(days_done, excluded.days_done)`, id, days); err != nil {
			return wallet, err
		}
	}
	return wallet, nil
}

// ── таймер автообновления ────────────────────────────────────────────

// AirResetAt returns the stored auto-reset moment (zero when unset).
func (s *Store) AirResetAt() time.Time {
	v, err := strconv.ParseInt(s.Setting("air_reset_at"), 10, 64)
	if err != nil || v <= 0 {
		return time.Time{}
	}
	return time.Unix(v, 0)
}

// SetAirResetAt stores the auto-reset moment; zero clears it.
func (s *Store) SetAirResetAt(t time.Time) error {
	if t.IsZero() {
		return s.SetSetting("air_reset_at", "")
	}
	return s.SetSetting("air_reset_at", strconv.FormatInt(t.Unix(), 10))
}

// airAdvanceReset moves a due reset moment forward in 30-day steps. One
// step per close: after months of downtime the intermediate results are
// unknowable anyway, so a single close must not spawn duplicate rows.
func (s *Store) airAdvanceReset(at, now time.Time) error {
	for !at.After(now) {
		at = at.Add(airResetPeriod)
	}
	return s.SetAirResetAt(at)
}

// AirAutoClose closes the month when the stored reset moment has passed
// and schedules the next one. Both the collector task and the page open
// call it — the page is the insurance for a copy with collection off.
func (s *Store) AirAutoClose(now time.Time) (bool, error) {
	at := s.AirResetAt()
	if at.IsZero() || at.After(now) {
		return false, nil
	}
	if _, err := s.CloseAirMonth(now); err != nil {
		return false, err
	}
	return true, s.airAdvanceReset(at, now)
}

// AirYearSP — сколько SP от AIR каждый персонаж реально получил в этом
// календарном году: закрытые месяцы с 1 января плюс текущий незакрытый
// прогресс (награда за день выдаётся сразу). Потребитель — статистика
// SP-фермы.
func (s *Store) AirYearSP(now time.Time) (map[int64]int64, error) {
	yearStart := time.Date(now.Year(), 1, 1, 0, 0, 0, 0, time.UTC)
	rows, err := s.db.Query(`SELECT character_id, SUM(sp) FROM air_month
		WHERE closed_at >= ? GROUP BY character_id`, yearStart.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]int64{}
	for rows.Next() {
		var id, sp int64
		if err := rows.Scan(&id, &sp); err != nil {
			return nil, err
		}
		out[id] = sp
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	states, err := s.AirStates()
	if err != nil {
		return out, err
	}
	omegas, err := s.AccountOmegas()
	if err != nil {
		return out, err
	}
	accounts := map[int64]string{}
	crows, err := s.db.Query(`SELECT character_id, account FROM characters`)
	if err != nil {
		return out, err
	}
	defer crows.Close()
	for crows.Next() {
		var id int64
		var acc string
		if err := crows.Scan(&id, &acc); err != nil {
			return out, err
		}
		accounts[id] = acc
	}
	for id, st := range states {
		omega := airOmegaActive(omegas[accounts[id]].OmegaUntil, now)
		out[id] += AirMonthSP(st.DaysDone, omega)
	}
	return out, crows.Err()
}

// AirManualClose is the button: close now; a reset moment already behind
// us moves forward, one still ahead is left alone (the game will reset
// then regardless of what was closed by hand).
func (s *Store) AirManualClose(now time.Time) (int, error) {
	n, err := s.CloseAirMonth(now)
	if err != nil {
		return n, err
	}
	if at := s.AirResetAt(); !at.IsZero() && !at.After(now) {
		err = s.airAdvanceReset(at, now)
	}
	return n, err
}
