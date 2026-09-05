package store

import (
	"testing"
	"time"
)

// TestAirSnapToDowntime: дата окончания месяца всегда встаёт на 11:00
// UTC (даунтайм) — остаток вводится руками, и задержка между «посмотрел
// в игре» и «вбил» до ±12 часов должна гаситься округлением.
func TestAirSnapToDowntime(t *testing.T) {
	cases := []struct{ in, want string }{
		{"2026-10-01 18:45:24", "2026-10-01 11:00:00"}, // вбили на 7ч45м позже
		{"2026-10-01 03:10:00", "2026-10-01 11:00:00"}, // вбили заранее
		{"2026-10-01 23:30:00", "2026-10-02 11:00:00"}, // ближе к следующему ДТ
		{"2026-10-01 11:00:00", "2026-10-01 11:00:00"}, // уже ровно
	}
	for _, c := range cases {
		in, _ := time.Parse("2006-01-02 15:04:05", c.in)
		if got := airSnapToDowntime(in).Format("2006-01-02 15:04:05"); got != c.want {
			t.Errorf("snap(%s) = %s, ждали %s", c.in, got, c.want)
		}
	}
}

// TestAirNextCalendarReset: без ручной синхронизации момент обновления
// шкалы известен по календарю — ДТ 1-го числа. Свежая копия должна
// работать сразу, без обязательной настройки таймера.
func TestAirNextCalendarReset(t *testing.T) {
	cases := []struct{ in, want string }{
		{"2026-09-04 04:00:00", "2026-10-01 11:00:00"}, // середина месяца
		{"2026-10-01 05:00:00", "2026-10-01 11:00:00"}, // 1-е число, до ДТ
		{"2026-10-01 12:00:00", "2026-11-01 11:00:00"}, // 1-е число, после ДТ
		{"2026-12-15 12:00:00", "2027-01-01 11:00:00"}, // через новый год
	}
	for _, c := range cases {
		in, _ := time.Parse("2006-01-02 15:04:05", c.in)
		if got := airNextCalendarReset(in).Format("2006-01-02 15:04:05"); got != c.want {
			t.Errorf("next(%s) = %s, ждали %s", c.in, got, c.want)
		}
	}
}

// TestAirSyncWalletDays закрывает сверенную с живой базой механику:
// деление шкалы AIR — запись daily_goal_payouts с reason дневного пункта
// (1004953); на каждый пункт две записи — личная и корп-дубль, их счёт
// не складывается, берётся больший. Задания дня (другие reason) и вехи
// делений не дают; накопленные пункты клеймятся пачкой одной секундой.
// Окно месяца — календарное (с ДТ накануне 1-го числа), счёт режется
// потолком прошедших дней, ручной счётчик синк не трогает.
func TestAirSyncWalletDays(t *testing.T) {
	s := testStore(t)
	// Сентябрь 2026: шкала открылась 31.08 11:00, обновится 01.10 11:00.
	// Таймер намеренно НЕ задан: окно должно выйти из календаря само.
	now := time.Date(2026, 9, 4, 4, 0, 0, 0, time.UTC) // идёт 4-й день

	for _, c := range []struct {
		id   int64
		name string
	}{{100, "Taxed"}, {200, "CorpOnly"}, {400, "Banked"}} {
		if _, err := s.db.Exec(`INSERT INTO characters (character_id, name, added_at)
			VALUES (?, ?, ?)`, c.id, c.name, now.Unix()); err != nil {
			t.Fatal(err)
		}
	}
	jid := int64(0)
	journal := func(owner, div, charID int64, at time.Time, amount float64, reason string) {
		jid++
		if _, err := s.db.Exec(`INSERT INTO hist_journal
			(owner_id, division, id, date, ref_type, amount, balance, second_party_id, reason)
			VALUES (?,?,?,?,'daily_goal_payouts',?,0,?,?)`,
			owner, div, jid, at.Unix(), amount, charID, reason); err != nil {
			t.Fatal(err)
		}
	}
	daily := airDailyGoalReason
	day1 := time.Date(2026, 8, 31, 15, 0, 0, 0, time.UTC) // первый день: открылся в ДТ 31.08
	day3 := time.Date(2026, 9, 3, 14, 0, 0, 0, time.UTC)

	// Персонаж 100: пункт в первый день (31.08 после ДТ — уже сентябрьская
	// шкала), потом два накопленных клеймлены пачкой в одну секунду — 3
	// деления. Корп-дубли отстают (собраны 2) — берётся большее, не сумма.
	// Задание дня (697667) и веха (712833) делений не дают.
	journal(100, 0, 100, day1, 500, daily)
	journal(100, 0, 100, day3, 500, daily)
	journal(100, 0, 100, day3, 500, daily)
	journal(999, 1, 100, day1, 499500, daily)
	journal(999, 1, 100, day3, 499500, daily)
	journal(100, 0, 100, day1, 500, "697667")
	journal(100, 0, 100, day3, 8000000, "712833")
	// Персонаж 200: личный журнал не собирается, корп-дубли есть — 2.
	journal(999, 1, 200, day1, 499500, daily)
	journal(999, 1, 200, day3, 499500, daily)
	journal(999, 1, 200, day3, 499500, "697671")
	// Персонаж 400: 5 пунктов пачкой — клейм-хвосты августа; прошло
	// только 4 дня месяца, потолок режет до 4.
	for i := 0; i < 5; i++ {
		journal(400, 0, 400, day3, 500000, daily)
	}
	// До открытия шкалы (31.08 до ДТ) и мимо ростера — не считается.
	journal(100, 0, 100, time.Date(2026, 8, 31, 9, 0, 0, 0, time.UTC), 500, daily)
	journal(300, 0, 300, day3, 500000, daily)

	wallet, err := s.AirSyncWalletDays(now)
	if err != nil {
		t.Fatal(err)
	}
	if wallet[100] != 3 || wallet[200] != 2 || wallet[400] != 4 {
		t.Fatalf("дни по валету: 100=%d 200=%d 400=%d, ждали 3, 2 и 4",
			wallet[100], wallet[200], wallet[400])
	}
	if _, ok := wallet[300]; ok {
		t.Fatal("чужой персонаж 300 не должен попадать в сверку")
	}

	states, err := s.AirStates()
	if err != nil {
		t.Fatal(err)
	}
	for id, want := range map[int64]int{100: 3, 200: 2, 400: 4} {
		if got := states[id].DaysDone; got != want {
			t.Fatalf("персонаж %d: days=%d, ждали %d", id, got, want)
		}
	}

	// Рука святее валета: ручная правка вниз (клейм-хвост в пределах
	// потолка синк не различит) не перетирается следующим синком.
	if err := s.SetAirDays(400, 3); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AirSyncWalletDays(now); err != nil {
		t.Fatal(err)
	}
	states, _ = s.AirStates()
	if got := states[400].DaysDone; got != 3 {
		t.Fatalf("персонаж 400: days=%d, ручные 3 не должны перетираться", got)
	}
	// А без ручной метки счётчик следует за валетом в обе стороны.
	if _, err := s.db.Exec(`DELETE FROM hist_journal WHERE second_party_id = 200 AND date = ?`,
		day3.Unix()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AirSyncWalletDays(now); err != nil {
		t.Fatal(err)
	}
	states, _ = s.AirStates()
	if got := states[200].DaysDone; got != 1 {
		t.Fatalf("персонаж 200: days=%d, без ручной метки счётчик следует за валетом", got)
	}
}
