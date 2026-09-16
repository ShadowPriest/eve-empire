package web

// Карта «раздел → право» (этап 4 плана кабинета, ARCHITECTURE.md
// «Кабинет и пользователи»).
//
// С появлением узкого пресета (config.PresetIndustry) у персонажа может
// не быть прав на половину кабинета. Честное поведение такое:
//
//   - вкладки персонажа, на которые права нет, не показываются вовсе;
//   - открытая по прямой ссылке вкладка объясняет, какого права не
//     хватает, вместо 401 из ESI в списке ошибок;
//   - сводные страницы империи такого персонажа молча пропускают: ESI
//     его не спрашивают и в ошибки не пишут.
//
// Всё это держится на одной таблице ниже. Добавляя раздел или страницу,
// которая ходит в ESI по персонажу, добавляй сюда её право — тест
// TestSectionScopesCoverSections следит, чтобы ни один раздел не остался
// без записи.

import (
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strings"

	"eve-empire/internal/store"
)

// Права, на которые ссылается карта. Имена те же, что в config.
const (
	scopeSkills     = "esi-skills.read_skills.v1"
	scopeQueue      = "esi-skills.read_skillqueue.v1"
	scopeWallet     = "esi-wallet.read_character_wallet.v1"
	scopeJobs       = "esi-industry.read_character_jobs.v1"
	scopeOrders     = "esi-markets.read_character_orders.v1"
	scopeAssets     = "esi-assets.read_assets.v1"
	scopeClones     = "esi-clones.read_clones.v1"
	scopeBlueprints = "esi-characters.read_blueprints.v1"
	scopePlanets    = "esi-planets.manage_planets.v1"
	scopeMail       = "esi-mail.read_mail.v1"
	scopeMining     = "esi-industry.read_character_mining.v1"
	scopeOnline     = "esi-location.read_online.v1"
	scopeCorpStruct = "esi-corporations.read_structures.v1"
	scopeCorpJobs   = "esi-industry.read_corporation_jobs.v1"
	scopeFleet      = "esi-fleets.read_fleet.v1"
)

// sectionScopes — вкладка персонажа → право, без которого она пуста.
// Обзор («») прав не требует: там публичная карточка.
var sectionScopes = map[string]string{
	"skills":     scopeSkills,
	"wallet":     scopeWallet,
	"industry":   scopeJobs,
	"market":     scopeOrders,
	"assets":     scopeAssets,
	"clones":     scopeClones,
	"blueprints": scopeBlueprints,
	"planets":    scopePlanets,
	"mail":       scopeMail,
}

// pageScopes — сводная страница империи (и инструмент, который ходит в
// ESI по персонажу) → право, без которого персонажу там делать нечего.
// Страницы, которых здесь нет (сводка «/», настройки, учёт), берут
// персонажей целиком: они либо читают базу, либо фильтруют сами.
var pageScopes = map[string]string{
	"/planets":             scopePlanets,
	"/mining":              scopeMining,
	"/wallets":             scopeWallet,
	"/training":            scopeQueue,
	"/industry":            scopeJobs,
	"/structures":          scopeCorpStruct,
	"/air":                 scopeWallet,
	"/tools/fleet":         scopeFleet,
	"/tools/ore":           scopeSkills,
	"/tools/skill-planner": scopeSkills,
}

// sectionAllowed — можно ли показывать персонажу эту вкладку.
func sectionAllowed(ch store.Character, section string) bool {
	scope, ok := sectionScopes[section]
	return !ok || ch.Has(scope)
}

// allowedSections — карта для шаблона: какие вкладки рисовать у
// выбранного персонажа (layout.html, `.SecOK`).
func allowedSections(ch store.Character) map[string]bool {
	out := make(map[string]bool, len(sectionScopes))
	for section := range sectionScopes {
		out[section] = ch.Has(sectionScopes[section])
	}
	return out
}

// charsWith оставляет персонажей, у которых есть право.
func charsWith(chars []sideChar, scope string) []sideChar {
	if scope == "" {
		return chars
	}
	out := make([]sideChar, 0, len(chars))
	for _, ch := range chars {
		if ch.Has(scope) {
			out = append(out, ch)
		}
	}
	return out
}

// empireCharsFor — персонажи кабинета, которых имеет смысл спрашивать на
// этой странице. Один вход вместо empireChars там, где страница ходит в
// ESI по каждому персонажу.
func empireCharsFor(data map[string]any, page string) []sideChar {
	return charsWith(empireChars(data), pageScopes[page])
}

// ── страница «нет права» ─────────────────────────────────────────────

// renderNoScope рисует вкладку, на которую у токена нет права: одна
// строка объяснения и ссылка на вход с полным набором. Отдаём 200, а не
// 403: это не отказ в доступе к чужому, а неполный токен своего же
// персонажа.
func (s *Server) renderNoScope(w http.ResponseWriter, r *http.Request, data map[string]any, section string) {
	data["MissingScope"] = sectionScopes[section]
	data["FullLoginHref"] = loginHref(localPath(r.URL.RequestURI()))
	s.render(w, "noscope", data, nil)
}

// loginHref — ссылка «войти с полным набором» на указанный возврат.
func loginHref(back string) string {
	href := "/login?preset=alt"
	if back != "" {
		href += "&back=" + url.QueryEscape(back)
	}
	return href
}

// ── метка пресета в настройках ───────────────────────────────────────

// presetLabel — как подписан токен персонажа на странице настроек.
type presetLabel struct {
	Text string
	Full bool // полный набор: перелогиниваться незачем
}

// presetOf узнаёт пресет по тому, что реально лежит в tokens.scopes.
// Считается по составу, а не по сохранённой метке: между входами набор в
// config мог измениться, и «частичный N из M» честнее любой метки.
//
// «Полный» — это покрытие, а не равенство: у старых токенов прав бывает
// БОЛЬШЕ нынешних (когда-то просили всё подряд), и кабинету от этого
// только хорошо — перелогиниваться незачем.
func presetOf(ch store.Character, presets []ScopePreset) presetLabel {
	full := altScopes(presets)
	if coversScopes(ch.Scopes, full) {
		return presetLabel{Text: "полный", Full: true}
	}
	for _, p := range presets {
		if p.Key != presetAlt && sameScopes(ch.Scopes, p.Scopes) {
			return presetLabel{Text: strings.ToLower(p.Title)}
		}
	}
	return presetLabel{Text: fmt.Sprintf("частичный (%d из %d)", len(ch.Scopes), len(full))}
}

// altScopes — полный набор из списка пресетов.
func altScopes(presets []ScopePreset) []string {
	for _, p := range presets {
		if p.Key == presetAlt {
			return p.Scopes
		}
	}
	return nil
}

// coversScopes — есть ли в have всё, что перечислено в need.
func coversScopes(have, need []string) bool {
	set := make(map[string]bool, len(have))
	for _, s := range have {
		set[s] = true
	}
	for _, s := range need {
		if !set[s] {
			return false
		}
	}
	return true
}

// sameScopes сравнивает наборы как множества: порядок в JWT свой.
func sameScopes(a, b []string) bool {
	return len(a) == len(b) && coversScopes(a, b)
}

// ── отзыв токена при сужении набора ──────────────────────────────────

// scopesShrunk — стал ли набор прав уже: есть право, которое было и
// пропало. Расширение и равенство отзыва не требуют. Чистая функция:
// вся логика решения под табличным тестом.
func scopesShrunk(old, fresh []string) bool {
	if len(old) == 0 {
		return false
	}
	set := make(map[string]bool, len(fresh))
	for _, s := range fresh {
		set[s] = true
	}
	for _, s := range old {
		if !set[s] {
			return true
		}
	}
	return false
}

// revokeIfNarrowed отзывает прежний refresh-токен персонажа, если новый
// вход дал набор прав уже старого. Вызывается до UpsertCharacter — после
// него старого токена в базе уже нет.
func (s *Server) revokeIfNarrowed(charID int64, name string, fresh []string) {
	old, err := s.Store.CharacterScopes(charID)
	if err != nil || !scopesShrunk(old, fresh) {
		return
	}
	refresh, err := s.Store.RefreshToken(charID)
	if err != nil || refresh == "" {
		return
	}
	if err := s.SSO.Revoke(refresh); err != nil {
		log.Printf("отзыв старого токена %s (%d): %v", name, charID, err)
		return
	}
	log.Printf("отозван прежний токен %s (%d): прав было %d, стало %d",
		name, charID, len(old), len(fresh))
}
