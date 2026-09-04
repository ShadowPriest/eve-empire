package store

import (
	"testing"
	"time"
)

// TestAirSyncWalletDays закрывает сверенную с живой базой механику:
// деление шкалы AIR — запись daily_goal_payouts с reason дневного пункта
// (1004953); на каждый пункт две записи — личная и корп-дубль, их счёт
// не складывается, берётся больший. Задания дня (другие reason) и вехи
// делений не дают; накопленные пункты клеймятся пачкой одной секундой.
// Счётчик только повышается.
func TestAirSyncWalletDays(t *testing.T) {
	s := testStore(t)
	now := time.Now().UTC()

	for _, c := range []struct {
		id   int64
		name string
	}{{100, "Taxed"}, {200, "CorpOnly"}} {
		if _, err := s.db.Exec(`INSERT INTO characters (character_id, name, added_at)
			VALUES (?, ?, ?)`, c.id, c.name, now.Unix()); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.SetAirResetAt(now.Add(10 * 24 * time.Hour)); err != nil {
		t.Fatal(err)
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

	// Персонаж 100: дневной пункт позавчера, потом два накопленных
	// клеймлены пачкой в одну секунду — 3 деления. Корп-дубли отстают
	// (собраны только 2) — берётся большее, а не сумма. Задание дня
	// (697667) и веха (712833) делений не дают.
	d1, d2 := now.Add(-72*time.Hour), now.Add(-24*time.Hour)
	journal(100, 0, 100, d1, 500, daily)
	journal(100, 0, 100, d2, 500, daily)
	journal(100, 0, 100, d2, 500, daily)
	journal(999, 1, 100, d1, 499500, daily)
	journal(999, 1, 100, d2, 499500, daily)
	journal(100, 0, 100, d1, 500, "697667")
	journal(100, 0, 100, d2, 8000000, "712833")
	// Персонаж 200: личный журнал не собирается, корп-дубли есть — 2.
	journal(999, 1, 200, d1, 499500, daily)
	journal(999, 1, 200, d2, 499500, daily)
	journal(999, 1, 200, d2, 499500, "697671")
	// Мимо окна месяца и мимо ростера — не считается.
	journal(100, 0, 100, now.Add(-25*24*time.Hour), 500, daily)
	journal(300, 0, 300, d1, 500000, daily)

	wallet, err := s.AirSyncWalletDays(now)
	if err != nil {
		t.Fatal(err)
	}
	if wallet[100] != 3 || wallet[200] != 2 {
		t.Fatalf("дни по валету: 100=%d 200=%d, ждали 3 и 2", wallet[100], wallet[200])
	}
	if _, ok := wallet[300]; ok {
		t.Fatal("чужой персонаж 300 не должен попадать в сверку")
	}

	states, err := s.AirStates()
	if err != nil {
		t.Fatal(err)
	}
	if got := states[100].DaysDone; got != 3 {
		t.Fatalf("персонаж 100: days=%d, ждали 3", got)
	}
	if got := states[200].DaysDone; got != 2 {
		t.Fatalf("персонаж 200: days=%d, ждали 2", got)
	}

	// Руками выставлено больше, чем видит валет, — не понижаем.
	if err := s.SetAirDays(200, 7); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AirSyncWalletDays(now); err != nil {
		t.Fatal(err)
	}
	states, _ = s.AirStates()
	if got := states[200].DaysDone; got != 7 {
		t.Fatalf("персонаж 200: days=%d, ручные 7 не должны понижаться", got)
	}
}
