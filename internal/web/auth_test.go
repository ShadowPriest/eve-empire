package web

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"eve-empire/internal/config"
	"eve-empire/internal/esi"
	"eve-empire/internal/sde"
	"eve-empire/internal/sso"
	"eve-empire/internal/store"
)

func TestResolveLogin(t *testing.T) {
	me := &store.User{ID: 7}
	cases := []struct {
		name        string
		intent      string
		signup      string
		current     *store.User
		owner       int64
		known       bool
		userCount   int
		corpAllowed bool
		want        loginAction
		wantUser    int64
		wantDeny    string
	}{
		{"вход известным персонажем", "signin", "first", nil, 3, true, 2, false, loginSignIn, 3, ""},
		{"вход известным при закрытой регистрации", "signin", "closed", nil, 3, true, 2, false, loginSignIn, 3, ""},
		{"первый вход в пустую базу", "signin", "first", nil, 0, false, 0, false, loginCreate, 0, ""},
		{"first: второй неизвестный — отказ", "signin", "first", nil, 0, false, 1, false, loginDeny, 0, denyClosed},
		{"open: новый кабинет всегда", "signin", "open", nil, 0, false, 5, false, loginCreate, 0, ""},
		{"open: первый тоже заводится", "signin", "open", nil, 0, false, 0, false, loginCreate, 0, ""},
		{"closed: неизвестный — отказ", "signin", "closed", nil, 0, false, 0, false, loginDeny, 0, denyClosed},
		{"corp: первый вход в пустую базу заводит админа", "signin", "corp", nil, 0, false, 0, false, loginCreate, 0, ""},
		{"corp: сокорпоративник администратора", "signin", "corp", nil, 0, false, 1, true, loginCreate, 0, ""},
		{"corp: посторонний — отказ", "signin", "corp", nil, 0, false, 1, false, loginDeny, 0, denyClosed},
		{"corp: известный персонаж входит как обычно", "signin", "corp", nil, 3, true, 2, false, loginSignIn, 3, ""},
		{"добавление неизвестного персонажа", "add", "closed", me, 0, false, 2, false, loginAttach, 7, ""},
		{"добавление своего — обновление токена", "add", "first", me, 7, true, 2, false, loginAttach, 7, ""},
		{"добавление чужого — отказ", "add", "open", me, 9, true, 2, false, loginDeny, 0, denyForeign},
		{"сессия отвалилась: add превращается во вход", "add", "first", nil, 7, true, 1, false, loginSignIn, 7, ""},
		{"сессия отвалилась, персонаж чужой и неизвестный", "add", "closed", nil, 0, false, 1, false, loginDeny, 0, denyClosed},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			act, uid, deny := resolveLogin(c.intent, c.signup, c.current, c.owner, c.known, c.userCount, c.corpAllowed)
			if act != c.want || uid != c.wantUser || deny != c.wantDeny {
				t.Errorf("resolveLogin = (%s, %d, %q), want (%s, %d, %q)",
					act, uid, deny, c.want, c.wantUser, c.wantDeny)
			}
		})
	}
}

// testServer поднимает сервер на временной базе: сеть не трогается,
// SSO-клиент нужен только ради AuthorizeURL.
func testServer(t *testing.T) (*Server, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"), make([]byte, 32))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	ssoClient := sso.New("cid", "secret", "http://localhost:8080/callback", []string{"publicData"}, "test")
	auth := AuthConfig{Signup: "first", SessionTTL: 24 * time.Hour}
	for _, p := range (&config.Config{Scopes: config.DefaultScopes()}).Presets() {
		auth.Presets = append(auth.Presets, ScopePreset{Key: p.Key, Title: p.Title, Scopes: p.Scopes})
	}
	srv, err := New(ssoClient, esi.New(ssoClient, st, "test"), st, sde.Open(filepath.Join(t.TempDir(), "none.db")),
		auth)
	if err != nil {
		t.Fatalf("web.New: %v", err)
	}
	return srv, st
}

func TestWallRedirectsAnonymous(t *testing.T) {
	srv, _ := testServer(t)
	h := srv.Routes()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if rec.Code != http.StatusFound {
		t.Fatalf("/ без сессии: %d, ждали 302", rec.Code)
	}
	if loc := rec.Header().Get("Location"); !strings.HasPrefix(loc, "/login") {
		t.Errorf("/ без сессии ведёт на %q, ждали /login", loc)
	}

	// Ручки JSON редиректом отвечать не должны: их ответ вклеивается
	// в открытую страницу.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/refresher", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("/api/refresher без сессии: %d, ждали 401", rec.Code)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/events?page=/", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("/events без сессии: %d, ждали 401", rec.Code)
	}

	// Иконки типов публичны. Без sde.db хендлер сам отвечает редиректом
	// на CDN — важно, что это не редирект на страницу входа.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/icons/1", nil))
	if rec.Code == http.StatusUnauthorized || strings.HasPrefix(rec.Header().Get("Location"), "/login") {
		t.Errorf("/icons/1 без сессии: %d → %q, стена его пускать не должна",
			rec.Code, rec.Header().Get("Location"))
	}

	// Страница входа рисуется целиком, без layout.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/login", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Войти через EVE Online") {
		t.Errorf("/login: %d, тела на %d байт", rec.Code, rec.Body.Len())
	}
}

func TestLoginWithSessionGoesToSSO(t *testing.T) {
	srv, st := testServer(t)
	h := srv.Routes()
	uid, err := st.CreateUser(true)
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	raw, err := st.CreateSession(uid, "test", time.Hour)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	req := httptest.NewRequest("GET", "/login", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: raw})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusFound {
		t.Fatalf("/login с сессией: %d, ждали 302", rec.Code)
	}
	if loc := rec.Header().Get("Location"); !strings.HasPrefix(loc, "https://login.eveonline.com/") {
		t.Errorf("/login с сессией ведёт на %q, ждали SSO", loc)
	}
	// Намерение и возврат уехали в одной куке.
	var back string
	for _, c := range rec.Result().Cookies() {
		if c.Name == backCookie {
			back = c.Value
		}
	}
	if back != "add=alt" {
		t.Errorf("кука намерения %q, ждали add=alt", back)
	}
}

func TestLoginIntentCookie(t *testing.T) {
	for _, c := range []struct{ raw, intent, preset, back string }{
		{"add=alt/reauth", "add", "alt", "/reauth"},
		{"signin=industry", "signin", "industry", ""},
		{"signin=alt/settings", "signin", "alt", "/settings"},
		{"add/reauth", "add", "", "/reauth"}, // кука без пресета
		{"signin", "signin", "", ""},         //
		{"signin/settings", "signin", "", "/settings"},
		{"/settings", "signin", "", "/settings"}, // кука старой сборки
		{"addhttp://evil/", "add", "", ""},       // чужой хост отбрасывается
		{"add=industryhttp://evil/", "add", "industryhttp:", ""},
	} {
		intent, preset, back := loginIntent(c.raw)
		if intent != c.intent || preset != c.preset || back != c.back {
			t.Errorf("loginIntent(%q) = (%q, %q, %q), want (%q, %q, %q)",
				c.raw, intent, preset, back, c.intent, c.preset, c.back)
		}
	}
}

func TestPostFromForeignOriginRejected(t *testing.T) {
	srv, _ := testServer(t)
	h := srv.Routes()

	req := httptest.NewRequest("POST", "/logout", nil)
	req.Host = "localhost:8080"
	req.Header.Set("Origin", "http://evil.example")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("POST с чужим Origin: %d, ждали 403", rec.Code)
	}

	// Свой Origin проходит.
	req = httptest.NewRequest("POST", "/logout", nil)
	req.Host = "localhost:8080"
	req.Header.Set("Origin", "http://localhost:8080")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusFound {
		t.Errorf("POST со своим Origin: %d, ждали 302", rec.Code)
	}

	// Без Origin и Referer (curl) — пропускаем.
	req = httptest.NewRequest("POST", "/logout", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusFound {
		t.Errorf("POST без заголовков: %d, ждали 302", rec.Code)
	}
}

func TestLogoutDropsSession(t *testing.T) {
	srv, st := testServer(t)
	h := srv.Routes()
	uid, _ := st.CreateUser(true)
	raw, err := st.CreateSession(uid, "test", time.Hour)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	req := httptest.NewRequest("POST", "/logout", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: raw})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/login" {
		t.Fatalf("POST /logout: %d → %q", rec.Code, rec.Header().Get("Location"))
	}
	if _, ok, _ := st.SessionUser(raw); ok {
		t.Error("сессия пережила выход")
	}
}
