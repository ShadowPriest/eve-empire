package web

// Раздел «Корпорация» (этап 5 плана кабинета, ARCHITECTURE.md
// «Корпорация»).
//
// ESI не отдаёт руководству корпорации ни навыки её членов, ни их
// производственные линии: таких эндпоинтов нет вовсе. Всё, что видно на
// этой странице, человек показал сам — вошёл в свой кабинет
// производственным пресетом и тем самым согласился делиться
// (`char_share`). Читаем мы его данные его же токеном.
//
// Зритель страницы — CEO: корпорация «своя», если её `ceo_id`
// (публичная карточка) — это id одного из персонажей кабинета.

import (
	"net/http"
	"sort"
	"sync"

	"eve-empire/internal/esi"
	"eve-empire/internal/store"
)

// shareSkills — навыки, которые показывает блок `skills`. Порядок тот
// же, что в дереве навыков игры: сперва общие, потом линии, потом
// исследования. Имена в EVE не локализуются иначе как через ESI по
// каждому типу, поэтому для короткого фиксированного списка они
// записаны как в игре.
var shareSkills = []struct {
	ID   int64
	Name string
}{
	{3380, "Industry"},
	{3388, "Advanced Industry"},
	{skillMassProduction, "Mass Production"},
	{skillAdvMassProduction, "Advanced Mass Production"},
	{skillLabOperation, "Laboratory Operation"},
	{skillAdvLabOperation, "Advanced Laboratory Operation"},
	{3403, "Research"},
	{3409, "Metallurgy"},
	{3402, "Science"},
}

// corpSkill — один навык члена корпорации в строке.
type corpSkill struct {
	Name  string
	Level int
}

// corpMember — строка страницы: один член корпорации, показавший свои
// данные. Пустые Skills/Lines означают «нет права в токене» — прочерк,
// а не ошибка страницы.
type corpMember struct {
	ID     int64
	Name   string
	Skills []corpSkill
	Lines  *lineStats
}

// corpGroup — одна корпорация зрителя со списком её членов.
type corpGroup struct {
	Corp    corpEntry
	Members []corpMember
}

func (s *Server) handleCorp(w http.ResponseWriter, r *http.Request) {
	ec, stale := s.esiFor(r)
	data, _, err := s.shell(r, ec, 0, "")
	if err != nil {
		httpError(w, "loading characters", err)
		return
	}
	user := userFrom(r)
	if user == nil {
		httpError(w, "loading characters", errNoUser)
		return
	}
	own, err := s.Store.Characters(user.ID)
	if err != nil {
		httpError(w, "loading characters", err)
		return
	}
	mine := make(map[int64]bool, len(own))
	for _, ch := range own {
		mine[ch.ID] = true
	}

	var groups []corpGroup
	for _, corp := range corpsWhereCEO(ec, data, mine) {
		groups = append(groups, corpGroup{
			Corp:    corp,
			Members: s.corpMembers(ec, corp.ID, mine),
		})
	}
	data["CorpGroups"] = groups
	data["JoinURL"] = serviceURL(r) + "/login"
	s.render(w, "corp", data, stale)
}

// corpsWhereCEO оставляет из корпораций кабинета те, где CEO — свой
// персонаж. `ceo_id` — публичное поле карточки корпорации; роль
// Director и прочие права ESI тут ни при чём.
func corpsWhereCEO(ec *esi.Client, data map[string]any, mine map[int64]bool) []corpEntry {
	corps, _ := data["Corporations"].([]corpEntry)
	var out []corpEntry
	for _, c := range corps {
		info, err := ec.CorporationInfo(c.ID)
		if err != nil || info == nil || !mine[info.CEOID] {
			continue
		}
		out = append(out, c)
	}
	return out
}

// corpMembers собирает строки членов корпорации: кто выдал согласие,
// всё ещё состоит в ней и чей токен позволяет прочитать блок.
func (s *Server) corpMembers(ec *esi.Client, corpID int64, mine map[int64]bool) []corpMember {
	shares, err := s.Store.CharSharesForCorp(corpID)
	if err != nil || len(shares) == 0 {
		return nil
	}
	// Имя и права персонажа берутся из базы инстанса: показываем только
	// тех, кто согласие выдал, так что чужого кабинета это не раскрывает.
	all, err := s.Store.AllCharacters()
	if err != nil {
		return nil
	}
	chars := make(map[int64]store.Character, len(all))
	for _, ch := range all {
		chars[ch.ID] = ch
	}

	members := make([]corpMember, len(shares))
	var wg sync.WaitGroup
	for i, sh := range shares {
		ch, ok := chars[sh.CharacterID]
		if !ok || mine[sh.CharacterID] {
			continue // свои персонажи на странице не нужны
		}
		wg.Add(1)
		go func(i int, sh store.CharShare, ch store.Character) {
			defer wg.Done()
			// Членство проверяем на каждом рендере: согласие хранит
			// корпорацию на момент входа, а человек мог уйти. Запись при
			// этом не трогаем — он мог и вернуться.
			if corpNow, _, err := ec.CharacterPublic(ch.ID); err != nil || corpNow != corpID {
				return
			}
			members[i] = s.corpMember(ec, sh, ch)
		}(i, sh, ch)
	}
	wg.Wait()

	out := make([]corpMember, 0, len(members))
	for _, m := range members {
		if m.ID != 0 {
			out = append(out, m)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// corpMember читает блоки согласия токеном самого члена. Права
// укладываются в производственный пресет; чего в токене нет, то и не
// спрашиваем — на странице будет прочерк, а не ошибка.
func (s *Server) corpMember(ec *esi.Client, sh store.CharShare, ch store.Character) corpMember {
	m := corpMember{ID: ch.ID, Name: ch.Name}
	needSkills := (sh.Has(store.ShareSkills) || sh.Has(store.ShareLines)) && ch.Has(scopeSkills)
	var sheet *esi.SkillSheet
	if needSkills {
		sheet, _ = ec.Skills(ch.ID)
	}
	if sheet != nil && sh.Has(store.ShareSkills) {
		lvl := map[int64]int{}
		for _, sk := range sheet.Skills {
			lvl[sk.SkillID] = sk.ActiveLevel
		}
		for _, sk := range shareSkills {
			m.Skills = append(m.Skills, corpSkill{Name: sk.Name, Level: lvl[sk.ID]})
		}
	}
	if sheet != nil && sh.Has(store.ShareLines) {
		var jobs []esi.IndustryJob
		if ch.Has(scopeJobs) {
			jobs, _ = ec.IndustryJobs(ch.ID)
		}
		ls := industryLines(sheet, jobs)
		m.Lines = &ls
	}
	return m
}

// serviceURL — адрес этого сервиса, каким его видит зритель: страницу
// входа надо как-то передать члену корпорации.
func serviceURL(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}
