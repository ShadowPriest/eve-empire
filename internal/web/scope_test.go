package web

// Область видимости кабинета (этап 3): что видит и чего не видит один
// кабинет, когда в базе их два. Персонажи и метки — выдуманные:
// репозиторий публичный.

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"eve-empire/internal/store"
)

// cabinet — кабинет с персонажами и готовой кукой сессии.
type cabinet struct {
	ID      int64
	Cookie  string
	CharIDs []int64
}

// newCabinet заводит кабинет, привязывает к нему персонажей и открывает
// сессию.
func newCabinet(t *testing.T, st *store.Store, admin bool, names map[int64]string) cabinet {
	t.Helper()
	uid, err := st.CreateUser(admin)
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	c := cabinet{ID: uid}
	for id, name := range names {
		if err := st.UpsertCharacter(uid, id, name, "rt", "at",
			time.Now().Add(time.Hour), []string{"publicData"}, "cid"); err != nil {
			t.Fatalf("UpsertCharacter %s: %v", name, err)
		}
		c.CharIDs = append(c.CharIDs, id)
	}
	raw, err := st.CreateSession(uid, "test", time.Hour)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	c.Cookie = raw
	return c
}

// do выполняет запрос от имени кабинета.
func (c cabinet) do(h http.Handler, method, target string, body string) *httptest.ResponseRecorder {
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, target, nil)
	} else {
		r = httptest.NewRequest(method, target, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	r.AddCookie(&http.Cookie{Name: sessionCookie, Value: c.Cookie})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

// TestForeignCharacterIsNotFound: кабинет A не достаёт ничего, что
// принадлежит кабинету B, и получает именно 404 — существование чужого
// персонажа не подтверждается.
func TestForeignCharacterIsNotFound(t *testing.T) {
	srv, st := testServer(t)
	h := srv.Routes()
	a := newCabinet(t, st, true, map[int64]string{9001: "Alpha One"})
	b := newCabinet(t, st, false, map[int64]string{9002: "Beta Two"})

	if rec := a.do(h, "GET", fmt.Sprintf("/characters/%d", a.CharIDs[0]), ""); rec.Code != http.StatusOK {
		t.Errorf("свой персонаж: %d, ждали 200", rec.Code)
	}
	for _, target := range []string{
		fmt.Sprintf("/characters/%d", b.CharIDs[0]),
		fmt.Sprintf("/characters/%d/skills", b.CharIDs[0]),
		fmt.Sprintf("/api/mail/%d/12345", b.CharIDs[0]),
	} {
		if rec := a.do(h, "GET", target, ""); rec.Code != http.StatusNotFound {
			t.Errorf("GET %s из чужого кабинета: %d, ждали 404", target, rec.Code)
		}
	}
	post := fmt.Sprintf("/characters/%d/account", b.CharIDs[0])
	if rec := a.do(h, "POST", post, "account=Чужой"); rec.Code != http.StatusNotFound {
		t.Errorf("POST %s: %d, ждали 404", post, rec.Code)
	}
	for _, target := range []string{
		fmt.Sprintf("/characters/%d/tags", b.CharIDs[0]),
		fmt.Sprintf("/characters/%d/delete", b.CharIDs[0]),
	} {
		if rec := a.do(h, "POST", target, "tags=x"); rec.Code != http.StatusNotFound {
			t.Errorf("POST %s: %d, ждали 404", target, rec.Code)
		}
	}
	// Персонаж B должен уцелеть целиком.
	if chars, err := st.Characters(b.ID); err != nil || len(chars) != 1 {
		t.Fatalf("персонажи B после чужих POST: %v, %v", chars, err)
	}
}

// TestSidebarOrderKeepsForeignCharacters: массовая ручка сайдбара
// получает id пачкой; чужой id не должен ничего переставить.
func TestSidebarOrderKeepsForeignCharacters(t *testing.T) {
	srv, st := testServer(t)
	h := srv.Routes()
	a := newCabinet(t, st, true, map[int64]string{9001: "Alpha One"})
	b := newCabinet(t, st, false, map[int64]string{9002: "Beta Two"})
	if err := st.SetAccount(b.ID, b.CharIDs[0], "Бета"); err != nil {
		t.Fatal(err)
	}

	body := fmt.Sprintf(`[{"account":"Альфа","chars":[%d,%d]}]`, a.CharIDs[0], b.CharIDs[0])
	r := httptest.NewRequest("POST", "/sidebar/order", strings.NewReader(body))
	r.AddCookie(&http.Cookie{Name: sessionCookie, Value: a.Cookie})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("POST /sidebar/order: %d, ждали 204", rec.Code)
	}

	chars, err := st.Characters(b.ID)
	if err != nil || len(chars) != 1 {
		t.Fatalf("персонажи B: %v, %v", chars, err)
	}
	if chars[0].Account != "Бета" {
		t.Errorf("аккаунт чужого персонажа стал %q, а должен был остаться «Бета»", chars[0].Account)
	}
}

// TestPITemplatesAreScoped: шаблон планетарки чужого кабинета не виден.
func TestPITemplatesAreScoped(t *testing.T) {
	srv, st := testServer(t)
	h := srv.Routes()
	a := newCabinet(t, st, true, map[int64]string{9001: "Alpha One"})
	b := newCabinet(t, st, false, map[int64]string{9002: "Beta Two"})

	if _, err := st.AddPITemplate(b.ID, store.PITemplate{
		Name: "ЧужойШаблонБеты", PlanetType: 11, ProductType: 2393, CmdCtrLv: 5,
		Payload: `{"P":[],"L":[]}`,
	}); err != nil {
		t.Fatal(err)
	}

	rec := a.do(h, "GET", "/tools/planetary?tab=templates", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("/tools/planetary у A: %d, ждали 200", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "ЧужойШаблонБеты") {
		t.Error("шаблон чужого кабинета попал на страницу")
	}
	if rec := b.do(h, "GET", "/tools/planetary?tab=templates", ""); !strings.Contains(rec.Body.String(), "ЧужойШаблонБеты") {
		t.Error("свой шаблон на странице не показан")
	}
}

// TestRefresherIsAdminOnly: панель инстанса — только администратору,
// остальным 404.
func TestRefresherIsAdminOnly(t *testing.T) {
	srv, st := testServer(t)
	h := srv.Routes()
	admin := newCabinet(t, st, true, map[int64]string{9001: "Alpha One"})
	plain := newCabinet(t, st, false, map[int64]string{9002: "Beta Two"})

	for _, target := range []string{"/refresher", "/api/refresher"} {
		if rec := admin.do(h, "GET", target, ""); rec.Code != http.StatusOK {
			t.Errorf("GET %s у админа: %d, ждали 200", target, rec.Code)
		}
		if rec := plain.do(h, "GET", target, ""); rec.Code != http.StatusNotFound {
			t.Errorf("GET %s у не-админа: %d, ждали 404", target, rec.Code)
		}
	}
	for _, target := range []string{"/api/refresher/mult", "/api/refresher/now"} {
		if rec := plain.do(h, "POST", target, "kind=wallet&mult=1&page=/"); rec.Code != http.StatusNotFound {
			t.Errorf("POST %s у не-админа: %d, ждали 404", target, rec.Code)
		}
	}
	// Пункт меню панели виден только админу.
	if body := admin.do(h, "GET", "/settings", "").Body.String(); !strings.Contains(body, `href="/refresher"`) {
		t.Error("у админа в меню нет ссылки на панель ESI Refresher")
	}
	if body := plain.do(h, "GET", "/settings", "").Body.String(); strings.Contains(body, `href="/refresher"`) {
		t.Error("у не-админа в меню осталась ссылка на панель ESI Refresher")
	}
}

// TestEmptyCabinetRenders: кабинет без персонажей не должен падать ни на
// одной странице, и сайдбар обязан звать добавить первого.
func TestEmptyCabinetRenders(t *testing.T) {
	srv, st := testServer(t)
	h := srv.Routes()
	empty := newCabinet(t, st, true, nil)

	pages := []string{
		"/", "/settings", "/planets", "/training", "/wallets", "/mining",
		"/industry", "/structures", "/accounting", "/air", "/reauth",
		"/tools/spfarm", "/tools/spfarm/model", "/tools/plex",
		"/tools/skill-planner", "/tools/planetary", "/tools/ore",
		"/tools/build", "/tools/orders", "/tools/market", "/tools/fleet",
	}
	for _, p := range pages {
		rec := empty.do(h, "GET", p, "")
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s в пустом кабинете: %d, ждали 200", p, rec.Code)
		}
	}
	body := empty.do(h, "GET", "/settings", "").Body.String()
	if !strings.Contains(body, `class="addchar" href="/login"`) {
		t.Error("в пустом сайдбаре нет ссылки «+ Добавить персонажа» на /login")
	}
}
