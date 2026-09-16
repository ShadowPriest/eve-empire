package web

// Карта «раздел → право» (этап 4 плана кабинета). Персонажи и имена
// выдуманы: репозиторий публичный.

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"eve-empire/internal/config"
	"eve-empire/internal/store"
)

// industryScopes — ровно то, что даёт узкий пресет.
var industryScopes = func() []string {
	sc, ok := (&config.Config{Scopes: config.DefaultScopes()}).PresetScopes(config.PresetIndustry)
	if !ok {
		panic("пресета industry нет")
	}
	return sc
}()

// TestScopeMapUsesRealScopes: каждое право из карты кабинет реально
// просит на входе. Опечатка здесь спрятала бы вкладку навсегда.
func TestScopeMapUsesRealScopes(t *testing.T) {
	full := config.DefaultScopes()
	for section, scope := range sectionScopes {
		if !slices.Contains(full, scope) {
			t.Errorf("раздел %q требует %q, которого нет в defaultScopes", section, scope)
		}
	}
	for page, scope := range pageScopes {
		if !slices.Contains(full, scope) {
			t.Errorf("страница %q требует %q, которого нет в defaultScopes", page, scope)
		}
	}
}

// TestSectionScopesCoverSections: ни одна вкладка персонажа не осталась
// без записи в карте — иначе она показывалась бы с пустым содержимым.
func TestSectionScopesCoverSections(t *testing.T) {
	for section := range charSections {
		if _, ok := sectionScopes[section]; !ok {
			t.Errorf("вкладка %q есть в charSections, но не в карте прав", section)
		}
	}
	for section := range sectionScopes {
		if !charSections[section] {
			t.Errorf("в карте прав есть %q, а такой вкладки нет", section)
		}
	}
}

// TestIndustryPresetAllowsOnlySkillsAndIndustry: узкий токен открывает
// навыки и производство, и больше ничего.
func TestIndustryPresetAllowsOnlySkillsAndIndustry(t *testing.T) {
	ch := store.Character{ID: 9001, Name: "Alpha One", Scopes: industryScopes}
	want := map[string]bool{"skills": true, "industry": true}
	for section := range charSections {
		if got := sectionAllowed(ch, section); got != want[section] {
			t.Errorf("sectionAllowed(%q) = %v, ждали %v", section, got, want[section])
		}
	}
	// Обзор прав не требует: там публичная карточка персонажа.
	if !sectionAllowed(ch, "") {
		t.Error("обзор персонажа должен открываться и с узким токеном")
	}
	for section, ok := range allowedSections(ch) {
		if ok != want[section] {
			t.Errorf("allowedSections[%q] = %v, ждали %v", section, ok, want[section])
		}
	}
}

// TestEmpirePagesSkipNarrowTokens: на сводных страницах узкий токен
// просто выпадает из списка, кроме той, где его право есть.
func TestEmpirePagesSkipNarrowTokens(t *testing.T) {
	narrow := sideChar{Character: store.Character{ID: 9001, Name: "Alpha One", Scopes: industryScopes}}
	wide := sideChar{Character: store.Character{ID: 9002, Name: "Beta Two", Scopes: config.DefaultScopes()}}
	chars := []sideChar{narrow, wide}

	data := map[string]any{"Groups": []accountGroup{{Chars: chars}}}
	for page, scope := range pageScopes {
		got := empireCharsFor(data, page)
		// Полный токен проходит везде; узкий — только там, где нужное
		// странице право входит в его четыре.
		want := 1
		if slices.Contains(industryScopes, scope) {
			want = 2
		}
		if len(got) != want {
			t.Errorf("%s: прошло %d персонажей, ждали %d", page, len(got), want)
		}
	}
	// Страница не из карты берёт всех: она фильтрует сама (сводка) или
	// читает только базу.
	if got := empireCharsFor(data, "/accounting"); len(got) != 2 {
		t.Errorf("/accounting: прошло %d персонажей, ждали 2", len(got))
	}
}

// TestNoScopeTabInsteadOfESIError: вкладка без права не показывается в
// строке вкладок, а открытая по ссылке объясняет, чего не хватает.
func TestNoScopeTabInsteadOfESIError(t *testing.T) {
	srv, st := testServer(t)
	h := srv.Routes()
	c := newCabinet(t, st, true, nil)
	if err := st.UpsertCharacter(c.ID, 9001, "Alpha One", "rt", "at",
		time.Now().Add(time.Hour), industryScopes, "cid"); err != nil {
		t.Fatalf("UpsertCharacter: %v", err)
	}

	// Смотрим именно строку вкладок: перекрёстные ссылки внутри самой
	// карточки персонажа — дело её страницы, не сайдбара.
	tabs := between(t, c.do(h, "GET", "/characters/9001", "").Body.String(),
		`<nav class="tabs">`, `</nav>`)
	for _, section := range []string{"skills", "industry"} {
		if !strings.Contains(tabs, fmt.Sprintf(`/characters/9001/%s"`, section)) {
			t.Errorf("вкладки %q нет, а право на неё есть", section)
		}
	}
	for _, section := range []string{"wallet", "assets", "clones", "blueprints", "planets", "mail", "market"} {
		if strings.Contains(tabs, fmt.Sprintf(`/characters/9001/%s"`, section)) {
			t.Errorf("вкладка %q показана, хотя права на неё нет", section)
		}
	}

	rec := c.do(h, "GET", "/characters/9001/wallet", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("вкладка без права: %d, ждали 200", rec.Code)
	}
	page := rec.Body.String()
	if !strings.Contains(page, scopeWallet) {
		t.Error("на странице не сказано, какого права не хватает")
	}
	if !strings.Contains(page, "/login?preset=alt&amp;back=%2Fcharacters%2F9001%2Fwallet") {
		t.Error("нет ссылки на вход с полным набором и возвратом на эту же страницу")
	}
}

// TestSettingsShowsPreset: метка пресета выводится из tokens.scopes.
func TestSettingsShowsPreset(t *testing.T) {
	srv, st := testServer(t)
	h := srv.Routes()
	c := newCabinet(t, st, true, nil)
	for _, ch := range []struct {
		id     int64
		name   string
		scopes []string
	}{
		{9001, "Alpha One", industryScopes},
		{9002, "Beta Two", config.DefaultScopes()},
		{9003, "Gamma Three", []string{"publicData", "esi-skills.read_skills.v1"}},
	} {
		if err := st.UpsertCharacter(c.ID, ch.id, ch.name, "rt", "at",
			time.Now().Add(time.Hour), ch.scopes, "cid"); err != nil {
			t.Fatalf("UpsertCharacter %s: %v", ch.name, err)
		}
	}

	// Старый токен с ЛИШНИМИ правами — всё равно полный: покрытие, а не
	// равенство.
	if err := st.UpsertCharacter(c.ID, 9004, "Delta Four", "rt", "at",
		time.Now().Add(time.Hour), append(config.DefaultScopes(), "esi-zzz.read_old.v1"), "cid"); err != nil {
		t.Fatalf("UpsertCharacter Delta Four: %v", err)
	}

	body := c.do(h, "GET", "/settings", "").Body.String()
	for _, want := range []string{
		"права: производство",
		"права: полный",
		fmt.Sprintf("права: частичный (2 из %d)", len(config.DefaultScopes())),
	} {
		if !strings.Contains(body, want) {
			t.Errorf("в настройках нет метки %q", want)
		}
	}
	if !strings.Contains(body, `href="/login?preset=alt&amp;back=%2Fsettings"`) {
		t.Error("у неполного токена нет ссылки «войти с полным набором»")
	}
}

// TestNarrowTokenIsOnTheReauthList: узкий набор — повод перелогиниться.
func TestNarrowTokenIsOnTheReauthList(t *testing.T) {
	srv, st := testServer(t)
	h := srv.Routes()
	c := newCabinet(t, st, true, nil)
	if err := st.UpsertCharacter(c.ID, 9001, "Alpha One", "rt", "at",
		time.Now().Add(time.Hour), industryScopes, "cid"); err != nil {
		t.Fatalf("UpsertCharacter: %v", err)
	}

	body := c.do(h, "GET", "/reauth", "").Body.String()
	if !strings.Contains(body, "узкий набор") {
		t.Error("персонаж с узким токеном не помечен в списке перелогина")
	}
	if !strings.Contains(body, "Осталось войти: <b>1</b>") {
		t.Error("узкий токен не попал в счётчик «осталось войти»")
	}
	if !strings.Contains(body, "/login?preset=alt&amp;back=/reauth") {
		t.Error("кнопка страницы не просит полный набор")
	}
}

// TestScopesShrunk: отзывать старый токен нужно только когда права
// пропали, а не когда их стало больше.
func TestScopesShrunk(t *testing.T) {
	full := []string{"publicData", "a", "b", "c"}
	for _, c := range []struct {
		name       string
		old, fresh []string
		want       bool
	}{
		{"первый вход: старого токена нет", nil, full, false},
		{"тот же набор", full, full, false},
		{"порядок другой, состав тот же", full, []string{"c", "b", "a", "publicData"}, false},
		{"набор расширили", []string{"publicData", "a"}, full, false},
		{"сузили до производственного", full, []string{"publicData", "a"}, true},
		{"обменяли одно право на другое", []string{"publicData", "a"}, []string{"publicData", "b"}, true},
		{"новый токен пуст", full, nil, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := scopesShrunk(c.old, c.fresh); got != c.want {
				t.Errorf("scopesShrunk(%v, %v) = %v, ждали %v", c.old, c.fresh, got, c.want)
			}
		})
	}
}

// TestLoginPageOffersBothPresets: на странице входа две ссылки, и узкая
// уводит на SSO ровно с четырьмя правами.
func TestLoginPageOffersBothPresets(t *testing.T) {
	srv, _ := testServer(t)
	h := srv.Routes()

	body := anon(h, "GET", "/login").Body.String()
	for _, want := range []string{"preset=alt", "preset=industry"} {
		if !strings.Contains(body, want) {
			t.Errorf("на странице входа нет ссылки с %q", want)
		}
	}

	rec := anon(h, "GET", "/login?go=1&preset=industry")
	if rec.Code != http.StatusFound {
		t.Fatalf("/login?go=1&preset=industry: %d, ждали 302", rec.Code)
	}
	loc := rec.Header().Get("Location")
	if !strings.HasPrefix(loc, "https://login.eveonline.com/") {
		t.Fatalf("редирект на %q, ждали SSO", loc)
	}
	got := scopeParam(t, loc)
	if !slices.Equal(got, industryScopes) {
		t.Errorf("узкий вход просит %v, ждали %v", got, industryScopes)
	}
	if len(got) != 4 {
		t.Errorf("прав в узком запросе %d, ждали 4", len(got))
	}

	// Неизвестный пресет не должен молча просить меньше или больше:
	// он ведёт себя как основной.
	alt := scopeParam(t, anon(h, "GET", "/login?go=1&preset=alt").Header().Get("Location"))
	unknown := scopeParam(t, anon(h, "GET", "/login?go=1&preset=нет-такого").Header().Get("Location"))
	if !slices.Equal(alt, unknown) {
		t.Errorf("неизвестный пресет просит %v, а основной %v", unknown, alt)
	}
}

// anon выполняет запрос без сессии.
func anon(h http.Handler, method, target string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, target, nil))
	return rec
}

// scopeParam достаёт список прав из authorize-ссылки SSO.
func scopeParam(t *testing.T, loc string) []string {
	t.Helper()
	u, err := url.Parse(loc)
	if err != nil {
		t.Fatalf("разбор %q: %v", loc, err)
	}
	return strings.Fields(u.Query().Get("scope"))
}

// between вырезает кусок страницы между двумя метками.
func between(t *testing.T, body, from, to string) string {
	t.Helper()
	i := strings.Index(body, from)
	if i < 0 {
		t.Fatalf("на странице нет %q", from)
	}
	rest := body[i+len(from):]
	j := strings.Index(rest, to)
	if j < 0 {
		t.Fatalf("на странице нет %q после %q", to, from)
	}
	return rest[:j]
}
