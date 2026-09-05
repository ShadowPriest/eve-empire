package web

// Вкладка AIR обзора империи: шкала прогресса за месяц по каждому альту.
// ESI прогресс AIR не отдаёт вообще, поэтому плитки выставляются руками,
// а «другие сервисы» (модель SP-фермы) берут итоги из закрытых месяцев
// (store.AirMonths). Закрытие месяца — кнопкой или автоматически по
// таймеру, синхронизированному с игровой шкалой («обновится через …»).

import (
	"fmt"
	"log"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"eve-empire/internal/store"
)

// airDayTile — метаданные одной дневной плитки, общие для всех строк:
// каждый 3-й день — веха, 15-й — финальная награда.
type airDayTile struct {
	N         int
	Milestone bool
	Final     bool
}

func airDayTiles() []airDayTile {
	out := make([]airDayTile, store.AirMonthDays)
	for i := range out {
		n := i + 1
		out[i] = airDayTile{N: n, Milestone: n%3 == 0, Final: n == store.AirFinalDay}
	}
	return out
}

// airMonthTile — одна месячная плитка строки персонажа.
type airMonthTile struct {
	No       int
	State    string // past | cur | future
	Fill     int    // % заливки при рендере — доля выполненных дней
	HistFill int    // % последнего закрытого результата — для перерисовки кликом
	Title    string
}

// airCharRow — строка персонажа на странице.
type airCharRow struct {
	sideChar
	Days       int
	MonthNo    int
	Omega      bool
	WalletDays int   // дни с выплатами daily goals в журналах за месяц
	LiveSP     int64 // текущий незакрытый месяц по формуле
	YearSP     int64 // сумма до 12 последних закрытых месяцев
	Months     []airMonthTile
}

func (s *Server) handleAIR(w http.ResponseWriter, r *http.Request) {
	now := time.Now().UTC()
	// Сначала подтянуть дни из валетов, потом проверить таймер: если
	// месяц пора закрывать, в историю должны попасть свежие дни.
	// Открытие страницы дублирует задачу коллектора — страховка для
	// копии с выключенным сбором (как снапшоты SP-фермы).
	wallet, err := s.Store.AirSyncWalletDays(now)
	if err != nil {
		log.Printf("AIR: дни из валета: %v", err)
	}
	if closed, err := s.Store.AirAutoClose(now); err != nil {
		log.Printf("AIR: автозакрытие месяца: %v", err)
	} else if closed {
		log.Printf("AIR: месяц закрыт по таймеру при открытии страницы")
		// Началось новое окно — старая сверка про закрытый месяц.
		if wallet, err = s.Store.AirSyncWalletDays(now); err != nil {
			log.Printf("AIR: дни из валета: %v", err)
		}
	}

	ec, stale := s.esiFor(r)
	data, _, err := s.shell(ec, 0, "")
	if err != nil {
		httpError(w, "loading characters", err)
		return
	}
	chars := empireChars(data)
	if len(chars) == 0 {
		s.render(w, "welcome", data, stale)
		return
	}

	states, err := s.Store.AirStates()
	if err != nil {
		httpError(w, "loading AIR state", err)
		return
	}
	hist, err := s.Store.AirMonths()
	if err != nil {
		httpError(w, "loading AIR history", err)
		return
	}
	omegas, _ := s.Store.AccountOmegas()

	// Свежайшая закрытая строка каждого (персонаж, номер месяца) — ей
	// заливаются пройденные плитки; счётчик закрытых месяцев — для суммы.
	type histKey struct {
		char  int64
		month int
	}
	latest := map[histKey]store.AirMonthRow{}
	yearSP := map[int64]int64{}
	yearN := map[int64]int{}
	for _, h := range hist { // новые первыми
		k := histKey{h.CharacterID, h.MonthNo}
		if _, ok := latest[k]; !ok {
			latest[k] = h
		}
		if yearN[h.CharacterID] < store.AirYearMonths {
			yearN[h.CharacterID]++
			yearSP[h.CharacterID] += h.SP
		}
	}

	rows := make([]airCharRow, len(chars))
	var liveTotal int64
	finals := 0
	for i, ch := range chars {
		st := states[ch.ID]
		if st.MonthNo < 1 {
			st.MonthNo = 1
		}
		omega := false
		if o, ok := omegas[ch.Account]; ok && o.OmegaUntil != "" {
			omega = omegaDeadline(o.OmegaUntil).After(now)
		}
		row := airCharRow{
			sideChar:   ch,
			Days:       st.DaysDone,
			MonthNo:    st.MonthNo,
			Omega:      omega,
			WalletDays: wallet[ch.ID],
			LiveSP:     store.AirMonthSP(st.DaysDone, omega),
			YearSP:     yearSP[ch.ID],
		}
		for no := 1; no <= store.AirYearMonths; no++ {
			t := airMonthTile{No: no}
			h, hasHist := latest[histKey{ch.ID, no}]
			if hasHist {
				t.HistFill = h.DaysDone * 100 / store.AirMonthDays
			}
			switch {
			case no == st.MonthNo:
				t.State = "cur"
				t.Fill = st.DaysDone * 100 / store.AirMonthDays
				t.Title = fmt.Sprintf("Месяц %d — текущий: %d/%d дн.", no, st.DaysDone, store.AirMonthDays)
			case no < st.MonthNo:
				t.State = "past"
				if hasHist {
					t.Fill = h.DaysDone * 100 / store.AirMonthDays
					t.Title = fmt.Sprintf("Месяц %d: %d/%d дн. · %s SP (%s)",
						no, h.DaysDone, store.AirMonthDays, numStr(h.SP), h.ClosedAt.Format("02.01.2006"))
				} else {
					t.Title = fmt.Sprintf("Месяц %d: не записан", no)
				}
			default:
				t.State = "future"
				t.Title = fmt.Sprintf("Месяц %d", no)
				if hasHist {
					t.Title += fmt.Sprintf(" · прошлый цикл: %d дн. (%s)",
						h.DaysDone, h.ClosedAt.Format("02.01.2006"))
				}
			}
			row.Months = append(row.Months, t)
		}
		rows[i] = row
		liveTotal += row.LiveSP
		if st.DaysDone >= store.AirFinalDay {
			finals++
		}
	}

	data["Chars"] = rows
	data["DayTiles"] = airDayTiles()
	data["LiveTotal"] = liveTotal
	data["Finals"] = finals
	resetAt, resetStored := s.Store.AirResetEffective(now)
	data["ResetAt"] = resetAt.UTC() // подпись на странице — EVE-время
	data["ResetCalc"] = !resetStored
	s.render(w, "empire_air", data, stale)
}

// numStr — 1 234 567 для тултипов, собранных в Go-коде.
func numStr(v int64) string {
	s := strconv.FormatInt(v, 10)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + " " + s[i:]
	}
	return s
}

// handleAIRDays сохраняет счётчик дней (клик по плитке, fetch без
// перезагрузки).
func (s *Server) handleAIRDays(w http.ResponseWriter, r *http.Request) {
	char, err1 := strconv.ParseInt(r.FormValue("char"), 10, 64)
	days, err2 := strconv.Atoi(r.FormValue("days"))
	if err1 != nil || err2 != nil || days < 0 || days > store.AirMonthDays {
		http.Error(w, "bad params", http.StatusBadRequest)
		return
	}
	if err := s.Store.SetAirDays(char, days); err != nil {
		httpError(w, "saving AIR days", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleAIRMonth сохраняет номер месяца (клик по плитке).
func (s *Server) handleAIRMonth(w http.ResponseWriter, r *http.Request) {
	char, err1 := strconv.ParseInt(r.FormValue("char"), 10, 64)
	month, err2 := strconv.Atoi(r.FormValue("month"))
	if err1 != nil || err2 != nil || month < 1 || month > store.AirYearMonths {
		http.Error(w, "bad params", http.StatusBadRequest)
		return
	}
	if err := s.Store.SetAirMonth(char, month); err != nil {
		httpError(w, "saving AIR month", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleAIRClose — кнопка «Начать новый месяц».
func (s *Server) handleAIRClose(w http.ResponseWriter, r *http.Request) {
	n, err := s.Store.AirManualClose(time.Now().UTC())
	if err != nil {
		httpError(w, "closing AIR month", err)
		return
	}
	log.Printf("AIR: месяц закрыт вручную, записано персонажей — %d", n)
	http.Redirect(w, r, "/air", http.StatusFound)
}

// handleAIRReset устанавливает таймер автообновления: в поле вбивается
// остаток как его показывает игра («28д 1ч 2мин 29с»); пустое поле
// выключает таймер.
func (s *Server) handleAIRReset(w http.ResponseWriter, r *http.Request) {
	raw := strings.TrimSpace(r.FormValue("left"))
	if raw == "" {
		if err := s.Store.SetAirResetAt(time.Time{}); err != nil {
			httpError(w, "clearing AIR timer", err)
			return
		}
		http.Redirect(w, r, "/air", http.StatusFound)
		return
	}
	d, err := parseAirLeft(raw)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.Store.SetAirResetAt(time.Now().UTC().Add(d)); err != nil {
		httpError(w, "saving AIR timer", err)
		return
	}
	http.Redirect(w, r, "/air", http.StatusFound)
}

var airLeftRe = regexp.MustCompile(`^(\d+)\s*([a-zа-я]*)\s*`)

// parseAirLeft разбирает «28д 1ч 2мин 29с» (допускаются латинские
// d/h/m/s). Голое число — дни, но только когда оно одно: «28 1 2» без
// юнитов — почти наверняка не то, что имелось в виду. Всё, что не
// разобралось, — ошибка, а не молчаливый пропуск.
func parseAirLeft(raw string) (time.Duration, error) {
	var d time.Duration
	rest := strings.ToLower(strings.TrimSpace(raw))
	tokens := 0
	bare := false
	for rest != "" {
		m := airLeftRe.FindStringSubmatch(rest)
		if m == nil {
			return 0, fmt.Errorf("не понял %q: нужен формат вида «28д 1ч 2мин 29с»", raw)
		}
		n, err := strconv.Atoi(m[1])
		if err != nil {
			return 0, fmt.Errorf("не понял %q: нужен формат вида «28д 1ч 2мин 29с»", raw)
		}
		var unit time.Duration
		switch u := m[2]; {
		case u == "":
			bare = true
			unit = 24 * time.Hour
		case strings.HasPrefix(u, "д") || strings.HasPrefix(u, "d"):
			unit = 24 * time.Hour
		case strings.HasPrefix(u, "ч") || strings.HasPrefix(u, "h"):
			unit = time.Hour
		case strings.HasPrefix(u, "мин") || u == "м" || strings.HasPrefix(u, "min") || u == "m":
			unit = time.Minute
		case strings.HasPrefix(u, "с") || strings.HasPrefix(u, "s"):
			unit = time.Second
		default:
			return 0, fmt.Errorf("не понял %q: нужен формат вида «28д 1ч 2мин 29с»", raw)
		}
		d += time.Duration(n) * unit
		tokens++
		rest = rest[len(m[0]):]
	}
	if tokens == 0 || (bare && tokens > 1) {
		return 0, fmt.Errorf("не понял %q: нужен формат вида «28д 1ч 2мин 29с»", raw)
	}
	return d, nil
}
