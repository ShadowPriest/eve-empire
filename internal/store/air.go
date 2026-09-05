package store

import (
	"strconv"
	"strings"
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
	AirMonthDays  = 30
	AirYearMonths = 12
	AirDaySP      = 10_000
	AirFinalDay   = 15
	AirFinalSP    = 75_000
	AirFinalOmega = 150_000
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
    days_done    INTEGER NOT NULL DEFAULT 0, -- 0..30
    hand         INTEGER NOT NULL DEFAULT 0  -- 1 = выставлено руками
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
	if err != nil {
		return err
	}
	// hand: рука святее валета — клейм-хвосты прошлого месяца завышают
	// счёт по журналу, и различить их нечем, поэтому ручной счётчик синк
	// не трогает. Базы до колонки заполнялись руками — им hand=1.
	if _, err := s.db.Exec(`ALTER TABLE air_state ADD COLUMN hand INTEGER NOT NULL DEFAULT 0`); err != nil {
		if !strings.Contains(err.Error(), "duplicate column") {
			return err
		}
	} else if _, err := s.db.Exec(`UPDATE air_state SET hand = 1 WHERE days_done > 0`); err != nil {
		return err
	}
	return nil
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

// SetAirDays sets the day counter of one character (a tile click) and
// marks it hand-set: the wallet sync must not fight a human correction.
func (s *Store) SetAirDays(characterID int64, days int) error {
	days = clampAir(days, 0, AirMonthDays)
	_, err := s.db.Exec(`INSERT INTO air_state (character_id, month_no, days_done, hand)
		VALUES (?, 1, ?, 1)
		ON CONFLICT(character_id) DO UPDATE SET days_done = excluded.days_done, hand = 1`,
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
		if _, err := tx.Exec(`INSERT INTO air_state (character_id, month_no, days_done, hand)
			VALUES (?, ?, 0, 0)
			ON CONFLICT(character_id) DO UPDATE SET
				month_no = excluded.month_no, days_done = 0, hand = 0`,
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

// Механика ежедневных целей AIR: в день доступно 5 заданий (по 500к ISK
// + 500 EverMarks каждое), дневная награда шкалы (деление + 10к SP)
// даётся за выполнение МИНИМУМ ДВУХ любых; активный персонаж закрывает
// больше двух просто повседневной игрой. Каждая выплата попадает в
// журнал записью daily_goal_payouts, и на каждую их две: личная и, при
// корп-налоге, дубль в валете корпорации (персонаж там в
// second_party_id). reason — id: у заданий свой на каждый день (697xxx,
// 712xxx — там же вехи шкалы вроде 8M ISK), а выплата за выполненную
// дневную цель — всегда 1004953, ровно одна на засчитанный день.
//
// Поэтому дни = число записей reason=1004953. Считать дни по выплатам
// заданий («≥2 в сутки») нельзя: задание ВЫПОЛНЯЕТСЯ активностью сразу
// (и цель зачитывается сразу), а клеймится — и попадает в журнал — когда
// угодно; накопленное клеймят пачкой одной секундой, и такой счёт
// разъезжается. Сверено с живой базой: личных 1004953 ровно столько,
// сколько плиток у владельца.
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

// airMonthStart — начало шкалы по моменту её окончания. Шкала живёт по
// календарному месяцу (сверено с живой базой и владельцем): сентябрьская
// кончается 01.10 в ДТ, а её первый дневной пункт открылся 31.08 в ДТ —
// то есть с даунтайма НАКАНУНЕ 1-го числа. «Минус 30 суток» здесь
// неверно: такой месяц длится 31 день.
func airMonthStart(reset time.Time) time.Time {
	prev := reset.UTC().Add(-24 * time.Hour) // любой момент внутри месяца шкалы
	first := time.Date(prev.Year(), prev.Month(), 1, 11, 0, 0, 0, time.UTC)
	return first.Add(-24 * time.Hour)
}

// airNextCalendarReset — ближайшее обновление шкалы по календарю EVE:
// ДТ 1-го числа. Раз шкала живёт по календарному месяцу, момент
// обновления известен и без ручной синхронизации — свежая копия (прод)
// работает сразу, без обязательной настройки таймера.
func airNextCalendarReset(now time.Time) time.Time {
	now = now.UTC()
	cur := time.Date(now.Year(), now.Month(), 1, 11, 0, 0, 0, time.UTC)
	if cur.After(now) {
		return cur
	}
	return time.Date(now.Year(), now.Month()+1, 1, 11, 0, 0, 0, time.UTC)
}

// AirResetEffective — действующий момент обновления шкалы: заданный
// синхронизацией или, когда таймер не задан, вычисленный по календарю.
func (s *Store) AirResetEffective(now time.Time) (at time.Time, stored bool) {
	if at := s.AirResetAt(); !at.IsZero() {
		return at, true
	}
	return airNextCalendarReset(now), false
}

// airWalletWindow — окно текущего месяца AIR: от его начала до
// действующего момента обновления шкалы.
func (s *Store) airWalletWindow(now time.Time) (from, to time.Time) {
	at, _ := s.AirResetEffective(now)
	return airMonthStart(at), at
}

// AirSyncWalletDays подтягивает счётчики дней из собранных журналов
// (личных и корповых) и возвращает дни по валету для сверки на странице.
// Счёт режется потолком прошедших дней месяца: больше физически быть не
// может, а клейм-хвосты прошлого месяца, заклеймленные после обновления
// шкалы, иначе завышают первые дни. Хвост в пределах потолка неотличим —
// поэтому ручной счётчик (hand) синк не трогает вовсе, а без ручной
// метки ведёт счётчик за валетом в обе стороны. «В валете пусто» не
// значит «не выполнял»: журнал собирается с конца июля и не у всех
// альтов, поэтому нулевые дни без записей не обнуляют ничего.
func (s *Store) AirSyncWalletDays(now time.Time) (map[int64]int, error) {
	from, to := s.airWalletWindow(now)
	maxDays := int(now.Sub(from).Hours()/24) + 1
	maxDays = clampAir(maxDays, 0, AirMonthDays)
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
		if days > maxDays {
			days = maxDays
			wallet[id] = days
		}
		if _, err := s.db.Exec(`INSERT INTO air_state (character_id, month_no, days_done, hand)
			VALUES (?, 1, ?, 0)
			ON CONFLICT(character_id) DO UPDATE SET
				days_done = excluded.days_done
				WHERE air_state.hand = 0`, id, days); err != nil {
			return wallet, err
		}
	}
	return wallet, nil
}

// ── диагностика сырья ────────────────────────────────────────────────

// AirWalletDiag — видно ли синку сырьё. Пустой бейдж «валет» выглядит
// одинаково и когда цели не выполнялись, и когда сбор журналов мёртв
// (выключен коллектор, мёртвые токены) — страница должна их различать:
// на проде это стоило дня недоумения.
type AirWalletDiag struct {
	InWindow   int             // записей daily_goal_payouts в окне месяца
	LastPayout time.Time       // самая свежая такая запись вообще (UTC)
	Wallet     CollectorStatus // последний прогон сбора валетов
}

func (s *Store) AirWalletDiag(now time.Time) AirWalletDiag {
	var d AirWalletDiag
	from, to := s.airWalletWindow(now)
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM hist_journal
		WHERE ref_type = 'daily_goal_payouts' AND date >= ? AND date < ?`,
		from.Unix(), to.Unix()).Scan(&d.InWindow)
	var last int64
	_ = s.db.QueryRow(`SELECT COALESCE(MAX(date), 0) FROM hist_journal
		WHERE ref_type = 'daily_goal_payouts'`).Scan(&last)
	if last > 0 {
		d.LastPayout = time.Unix(last, 0).UTC()
	}
	if sts, err := s.CollectorStatuses(); err == nil {
		for _, st := range sts {
			if st.Task == "wallet" {
				d.Wallet = st
			}
		}
	}
	return d
}

// ── таймер автообновления ────────────────────────────────────────────

// Шкала AIR обновляется в даунтайм, а он статичен — 11:00 UTC каждый
// день. Поэтому дата окончания всегда округляется до ближайших 11:00:
// остаток вводится руками с игрового таймера, и любая задержка между
// «посмотрел» и «вбил» (до ±12 часов) гасится округлением. Окно месяца
// (окончание −30 суток) от этого тоже встаёт ровно на даунтайм.
func airSnapToDowntime(t time.Time) time.Time {
	t = t.UTC()
	dt := time.Date(t.Year(), t.Month(), t.Day(), 11, 0, 0, 0, time.UTC)
	if t.Sub(dt) >= 12*time.Hour {
		return dt.Add(24 * time.Hour)
	}
	return dt
}

// AirResetAt returns the stored auto-reset moment (zero when unset).
func (s *Store) AirResetAt() time.Time {
	v, err := strconv.ParseInt(s.Setting("air_reset_at"), 10, 64)
	if err != nil || v <= 0 {
		return time.Time{}
	}
	return time.Unix(v, 0)
}

// SetAirResetAt stores the auto-reset moment, snapped to the 11:00 UTC
// downtime; zero clears it.
func (s *Store) SetAirResetAt(t time.Time) error {
	if t.IsZero() {
		return s.SetSetting("air_reset_at", "")
	}
	return s.SetSetting("air_reset_at", strconv.FormatInt(airSnapToDowntime(t).Unix(), 10))
}

// airAdvanceReset moves a due reset moment to the next monthly downtime
// still ahead. One close per catch-up: after months of downtime the
// intermediate results are unknowable anyway, so a single close must not
// spawn duplicate rows.
func (s *Store) airAdvanceReset(at, now time.Time) error {
	at = at.UTC()
	for !at.After(now) {
		// ДТ 1-го числа следующего месяца; time.Date нормализует декабрь+1.
		at = time.Date(at.Year(), at.Month()+1, 1, 11, 0, 0, 0, time.UTC)
	}
	return s.SetAirResetAt(at)
}

// AirAutoClose closes the month when the effective reset moment (stored
// or calendar-derived) has passed and schedules the next one. Both the
// collector task and the page open call it — the page is the insurance
// for a copy with collection off.
func (s *Store) AirAutoClose(now time.Time) (bool, error) {
	at, _ := s.AirResetEffective(now)
	if at.After(now) {
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
