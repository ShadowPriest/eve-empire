package web

// Вход в кабинет: серверные сессии, стена для анонима и правила
// `/callback` (этап 2 плана кабинета, ARCHITECTURE.md «Кабинет и
// пользователи»).
//
// Личность подтверждается только входом через EVE SSO: паролей нет.
// Сессия — строка в SQLite (в базе лежит SHA-256 от id, сам id живёт
// в куке), поэтому выход работает на всех устройствах и копия базы
// войти не даёт.

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"eve-empire/internal/store"
)

// sessionCookie — кука сессии кабинета. Без Secure: прод стоит в LAN
// на голом HTTP, с Secure браузер куку просто не сохранит.
const sessionCookie = "eve_session"

// AuthConfig — то, что веб-слою нужно знать о входе (из config).
type AuthConfig struct {
	// Signup — политика регистрации: first|open|closed|corp.
	Signup string
	// SessionTTL — срок жизни сессии и куки.
	SessionTTL time.Duration
	// Presets — наборы прав, которые предлагает страница входа
	// (config.Presets), в порядке показа. Первый считается основным.
	Presets []ScopePreset
}

// ScopePreset — пресет прав со страницы входа. Копия config.Preset:
// веб-слой знает о конфиге ровно столько, сколько ему передали.
type ScopePreset struct {
	Key    string
	Title  string
	Scopes []string
}

// Ключи пресетов. Они же значения `?preset=` в адресной строке, поэтому
// живут и здесь, и в config.
const (
	presetAlt      = "alt"
	presetIndustry = "industry"
)

// presets — пресеты входа. Пустой конфиг (тесты, старые вызовы) даёт
// один «Альт» с полным набором SSO-клиента, чтобы вход работал.
func (s *Server) presets() []ScopePreset {
	if len(s.Auth.Presets) > 0 {
		return s.Auth.Presets
	}
	return []ScopePreset{{Key: presetAlt, Title: "Альт", Scopes: s.SSO.Scopes}}
}

// presetScopes — права пресета для AuthorizeURL. Неизвестный ключ (или
// пустой) означает основной пресет: ссылка с опечаткой не должна
// ронять вход, а лишних прав она так не выпросит.
func (s *Server) presetScopes(key string) (string, []string) {
	list := s.presets()
	for _, p := range list {
		if p.Key == key {
			return p.Key, p.Scopes
		}
	}
	return list[0].Key, list[0].Scopes
}

// ttl возвращает срок жизни сессии, подставляя 90 дней, если конфиг
// не заполнен (тесты, старые вызовы).
func (a AuthConfig) ttl() time.Duration {
	if a.SessionTTL <= 0 {
		return 90 * 24 * time.Hour
	}
	return a.SessionTTL
}

type ctxKey int

const userCtxKey ctxKey = 0

// userFrom достаёт кабинет текущего запроса; nil — аноним (такое
// бывает только на открытых путях: всё остальное закрыто middleware).
func userFrom(r *http.Request) *store.User {
	u, _ := r.Context().Value(userCtxKey).(*store.User)
	return u
}

// errNoUser — запрос без кабинета там, где он обязателен. Такого быть
// не может: всё, кроме открытых путей, закрыто middleware; ошибка нужна
// лишь чтобы ни одна страница не отрисовалась «ничьей».
var errNoUser = errors.New("нет кабинета в контексте запроса")

// ownsCharacter — принадлежит ли персонаж кабинету запроса. Ответ
// «нет» и «такого персонажа нет» намеренно неотличимы: хендлеры на
// этом отвечают 404, а не 403, чтобы не подтверждать существование
// чужого персонажа.
func (s *Server) ownsCharacter(r *http.Request, charID int64) bool {
	u := userFrom(r)
	if u == nil || charID == 0 {
		return false
	}
	owner, ok, err := s.Store.UserOfCharacter(charID)
	return err == nil && ok && owner == u.ID
}

// isAdmin — админ ли кабинет запроса.
func isAdmin(r *http.Request) bool {
	u := userFrom(r)
	return u != nil && u.Admin
}

// sessionTouch помнит, когда сессию последний раз отмечали живой:
// UPDATE на каждый запрос страницы с тремя десятками картинок и SSE
// того не стоит, раза в минуту достаточно.
type sessionTouch struct {
	mu   sync.Mutex
	seen map[string]time.Time
}

const touchEvery = time.Minute

func (t *sessionTouch) due(raw string) bool {
	now := time.Now()
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.seen == nil {
		t.seen = map[string]time.Time{}
	}
	if last, ok := t.seen[raw]; ok && now.Sub(last) < touchEvery {
		return false
	}
	// Карта не должна расти бесконечно: сессий немного, но чистим.
	if len(t.seen) > 1000 {
		for k, v := range t.seen {
			if now.Sub(v) > touchEvery {
				delete(t.seen, k)
			}
		}
	}
	t.seen[raw] = now
	return true
}

// openPath — что видно без сессии: страница входа, возврат из SSO,
// выход и иконки типов (публичный статический ресурс из SDE).
func openPath(r *http.Request) bool {
	p := r.URL.Path
	switch {
	case r.Method == http.MethodGet && (p == "/login" || p == "/callback"):
		return true
	case r.Method == http.MethodPost && p == "/logout":
		return true
	case r.Method == http.MethodGet && strings.HasPrefix(p, "/icons/"):
		return true
	}
	return false
}

// wantsJSON — запрос, которому редирект на страницу входа только
// навредит: его ответ вклеивается в открытую страницу. `/events` и
// `/api/*` такие по определению, остальные признаются по заголовкам.
func wantsJSON(r *http.Request) bool {
	p := r.URL.Path
	if p == "/events" || strings.HasPrefix(p, "/api/") {
		return true
	}
	if strings.Contains(r.Header.Get("Accept"), "application/json") {
		return true
	}
	return r.Header.Get("X-Requested-With") != ""
}

// sameOrigin защищает POST от отправки с чужой страницы. SameSite=Lax
// уже отсекает межсайтовые POST в современных браузерах; это второй
// рубеж. Запрос вообще без Origin и Referer пропускаем — так ходят curl
// и старые клиенты.
func sameOrigin(r *http.Request) bool {
	if o := r.Header.Get("Origin"); o != "" && o != "null" {
		u, err := url.Parse(o)
		return err == nil && u.Host == r.Host
	}
	if ref := r.Header.Get("Referer"); ref != "" {
		u, err := url.Parse(ref)
		return err == nil && u.Host == r.Host
	}
	return true
}

// withAuth оборачивает mux: проверка Origin на записи, чтение сессии,
// стена входа для анонима.
func (s *Server) withAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
		default:
			if !sameOrigin(r) {
				http.Error(w, "cross-origin request", http.StatusForbidden)
				return
			}
		}

		var user *store.User
		if c, err := r.Cookie(sessionCookie); err == nil && c.Value != "" {
			if uid, ok, err := s.Store.SessionUser(c.Value); err == nil && ok {
				if u, err := s.Store.User(uid); err == nil && u != nil {
					user = u
					if s.touch.due(c.Value) {
						_ = s.Store.TouchSession(c.Value)
					}
				}
			}
		}

		if user == nil && !openPath(r) {
			if wantsJSON(r) {
				http.Error(w, "нет сессии", http.StatusUnauthorized)
				return
			}
			http.Redirect(w, r, "/login?back="+url.QueryEscape(localPath(r.URL.RequestURI())),
				http.StatusFound)
			return
		}
		if user != nil {
			r = r.WithContext(context.WithValue(r.Context(), userCtxKey, user))
		}
		next.ServeHTTP(w, r)
	})
}

// ── решение о входе ──────────────────────────────────────────────────

// loginAction — что делает `/callback` после успешного SSO.
type loginAction string

const (
	loginDeny   loginAction = "deny"   // отказ, персонажа не сохраняем
	loginSignIn loginAction = "signin" // войти в кабинет, которому персонаж уже принадлежит
	loginCreate loginAction = "create" // завести новый кабинет
	loginAttach loginAction = "attach" // добавить персонажа в текущий кабинет
)

// Коды отказа (они же значения `?denied=` на странице входа).
const (
	denyClosed  = "closed"  // регистрация новых кабинетов закрыта
	denyForeign = "foreign" // персонаж уже в чужом кабинете
)

// resolveLogin — все правила входа в одном месте (решение 5 плана).
// Чистая функция: ни http, ни базы, вся ветвистость под табличным
// тестом.
//
//	intent      — "signin" (входим) или "add" (добавляем персонажа);
//	signup      — политика регистрации;
//	current     — кабинет текущей сессии (nil, если сессии нет);
//	owner       — кабинет персонажа (0 — персонаж ничей);
//	known       — персонаж вообще есть в базе;
//	userCount   — сколько кабинетов уже заведено;
//	corpAllowed — персонаж состоит в корпорации, где есть персонаж
//	              администратора (нужно только при SIGNUP=corp; считает
//	              `/callback`, и только когда решение от этого зависит).
//
// Возвращает действие, кабинет, к которому оно относится (для create —
// 0, кабинет ещё не создан), и код отказа.
func resolveLogin(intent, signup string, current *store.User, owner int64, known bool, userCount int, corpAllowed bool) (loginAction, int64, string) {
	// Сессия могла истечь, пока персонаж ходил на login.eveonline.com:
	// тогда добавлять некуда, и это обычный вход.
	if current == nil {
		intent = "signin"
	}

	if intent == "add" {
		switch {
		case owner == 0: // ничей персонаж (новый или осиротевший) — берём себе
			return loginAttach, current.ID, ""
		case owner == current.ID: // свой: вход просто обновляет токен
			return loginAttach, current.ID, ""
		default:
			return loginDeny, 0, denyForeign
		}
	}

	if owner != 0 {
		return loginSignIn, owner, ""
	}
	// Персонаж неизвестен (или заведён, но ничей — known, owner == 0):
	// и то и другое означает «кабинета нет», решает политика.
	switch signup {
	case "open":
		return loginCreate, 0, ""
	case "first":
		if userCount == 0 {
			return loginCreate, 0, ""
		}
	case "corp":
		// Первый вход в пустую базу заводит администратора (как first),
		// дальше кабинет получает сокорпоративник администратора.
		if userCount == 0 || corpAllowed {
			return loginCreate, 0, ""
		}
	}
	return loginDeny, 0, denyClosed
}

// sharesAdminCorp — состоит ли персонаж в корпорации, где есть персонаж
// администратора. Это и есть политика `SIGNUP=corp`: кабинет заводится
// членам корпораций владельца, чужому — отказ `closed`.
//
// Корпорация персонажа — публичное поле (`/characters/{id}/`), поэтому
// запросы идут без токена (characterID = 0) и отвечает на них кэш ESI:
// персонажи администратора уже прочитаны сайдбаром.
func (s *Server) sharesAdminCorp(charID int64) bool {
	corpID, _, err := s.ESI.CharacterPublic(charID)
	if err != nil || corpID == 0 {
		return false
	}
	admin, err := s.Store.AdminUser()
	if err != nil || admin == nil {
		return false
	}
	chars, err := s.Store.Characters(admin.ID)
	if err != nil {
		return false
	}
	for _, ch := range chars {
		if id, _, err := s.ESI.CharacterPublic(ch.ID); err == nil && id == corpID {
			return true
		}
	}
	return false
}

// denyText — текст отказа для страницы входа.
func denyText(reason string) string {
	switch reason {
	case denyForeign:
		return "Персонаж уже добавлен в другой кабинет. Выйдите и войдите этим персонажем."
	case denyClosed:
		return "Регистрация новых кабинетов закрыта. Войдите персонажем, который уже добавлен."
	}
	return ""
}

// ── выход ────────────────────────────────────────────────────────────

// handleLogout закрывает сессию: текущую или все сессии кабинета
// (`?all=1` — кнопка «Выйти на всех устройствах» в настройках).
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil && c.Value != "" {
		if r.URL.Query().Get("all") == "1" {
			if uid, ok, err := s.Store.SessionUser(c.Value); err == nil && ok {
				_ = s.Store.DeleteUserSessions(uid)
			}
		}
		_ = s.Store.DeleteSession(c.Value)
	}
	clearSessionCookie(w)
	http.Redirect(w, r, "/login", http.StatusFound)
}

// setSessionCookie выдаёт куку сессии.
func (s *Server) setSessionCookie(w http.ResponseWriter, raw string) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    raw,
		Path:     "/",
		MaxAge:   int(s.Auth.ttl() / time.Second),
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
}

func clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
}

// ownCharacterIDs оставляет из присланного списка только персонажей
// этого кабинета. Массовые действия (маршрут, слежение) получают id
// пачкой из сайдбара: чужой id молча выпадает, а не становится ошибкой.
func (s *Server) ownCharacterIDs(r *http.Request, ids []int64) []int64 {
	u := userFrom(r)
	if u == nil {
		return nil
	}
	chars, err := s.Store.Characters(u.ID)
	if err != nil {
		return nil
	}
	own := make(map[int64]bool, len(chars))
	for _, ch := range chars {
		own[ch.ID] = true
	}
	out := ids[:0]
	for _, id := range ids {
		if own[id] {
			out = append(out, id)
		}
	}
	return out
}
