package web

import (
	"testing"
	"time"

	"eve-empire/internal/store"
)

func TestParseAirLeft(t *testing.T) {
	day := 24 * time.Hour
	ok := []struct {
		in   string
		want time.Duration
	}{
		{"28д 1ч 2мин 29с", 28*day + time.Hour + 2*time.Minute + 29*time.Second},
		{"28d 1h 2min 29s", 28*day + time.Hour + 2*time.Minute + 29*time.Second},
		{"28д1ч2мин29с", 28*day + time.Hour + 2*time.Minute + 29*time.Second},
		{"28", 28 * day},
		{"5 дней", 5 * day},
		{"1ч 30 мин", time.Hour + 30*time.Minute},
		{"2М", 2 * time.Minute}, // регистр не важен; «м» — минуты, как в игре
	}
	for _, c := range ok {
		got, err := parseAirLeft(c.in)
		if err != nil {
			t.Errorf("parseAirLeft(%q): %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("parseAirLeft(%q) = %v, ждали %v", c.in, got, c.want)
		}
	}

	// Числа без юнитов допустимы только одиночные, мусор — ошибка:
	// молчаливое «всё стало днями» уже давало таймер на 60 суток.
	for _, in := range []string{"", "28 1 2", "28x", "завтра", "д28"} {
		if _, err := parseAirLeft(in); err == nil {
			t.Errorf("parseAirLeft(%q): ждали ошибку", in)
		}
	}
}

func TestAirMonthSP(t *testing.T) {
	cases := []struct {
		days  int
		omega bool
		want  int64
	}{
		{0, false, 0},
		{14, true, 140_000},  // до финальной вехи омега ничего не добавляет
		{15, false, 225_000}, // 150к дневных + 75к финалка альфы
		{15, true, 375_000},  // + 150к омеге
		{30, true, 525_000},
	}
	for _, c := range cases {
		if got := store.AirMonthSP(c.days, c.omega); got != c.want {
			t.Errorf("AirMonthSP(%d, %v) = %d, ждали %d", c.days, c.omega, got, c.want)
		}
	}
}
