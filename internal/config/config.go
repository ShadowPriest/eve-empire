package config

import (
	"bufio"
	"encoding/hex"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
)

// Config holds all runtime settings, loaded from environment variables
// (optionally pre-populated from a .env file in the working directory).
type Config struct {
	ClientID      string
	ClientSecret  string
	CallbackURL   string
	ListenAddr    string
	DBPath        string
	SDEPath       string
	Scopes        []string
	EncryptionKey []byte // 32 bytes for AES-256-GCM
	UserAgent     string
	// Collector turns the background accounting collector on. The dev copy
	// usually wants it off: two copies polling the same characters double
	// the ESI traffic for no benefit, since each has its own database.
	Collector bool
	// Refresher selects the ESI Refresher mode (esi/refresher.go):
	// "full" keeps everything ever read warm in the background, "demand"
	// only fetches what open pages need — the dev copy next to prod.
	Refresher string
	// Signup — политика регистрации кабинетов: кто может завести НОВЫЙ
	// кабинет, войдя персонажем, которого ещё нет в базе.
	//   first  — только самый первый вход в пустую базу (по умолчанию);
	//   open   — любой неизвестный персонаж заводит свой кабинет;
	//   closed — никто: вход только персонажем, который уже добавлен;
	//   corp   — кабинет заводит тот, кто состоит в корпорации, где
	//            есть персонаж администратора (проверяется по публичной
	//            карточке персонажа после SSO).
	// Добавление персонажей в СВОЙ кабинет политика не ограничивает.
	Signup string
	// SessionDays — срок жизни серверной сессии (куки) в днях.
	SessionDays int
}

// signupModes — допустимые значения SIGNUP.
var signupModes = []string{"first", "open", "closed", "corp"}

// Load reads .env (if present) and then the environment.
func Load() (*Config, error) {
	loadDotEnv(".env")

	c := &Config{
		ClientID:     os.Getenv("EVE_CLIENT_ID"),
		ClientSecret: os.Getenv("EVE_CLIENT_SECRET"),
		CallbackURL:  getEnv("EVE_CALLBACK_URL", "http://localhost:8080/callback"),
		ListenAddr:   getEnv("LISTEN_ADDR", ":8080"),
		DBPath:       getEnv("DB_PATH", "eve-empire.db"),
		SDEPath:      getEnv("SDE_PATH", "sde.db"),
		UserAgent:    getEnv("ESI_USER_AGENT", "eve-empire/0.1"),
		Collector:    !isOff(getEnv("COLLECTOR", "on")),
		Refresher:    getEnv("REFRESHER", "full"),
		Signup:       strings.ToLower(strings.TrimSpace(getEnv("SIGNUP", "first"))),
	}

	if c.ClientID == "" || c.ClientSecret == "" {
		return nil, fmt.Errorf("EVE_CLIENT_ID and EVE_CLIENT_SECRET must be set")
	}

	known := false
	for _, m := range signupModes {
		if c.Signup == m {
			known = true
			break
		}
	}
	if !known {
		return nil, fmt.Errorf("SIGNUP must be one of %s (got %q)", strings.Join(signupModes, "|"), c.Signup)
	}

	days, err := strconv.Atoi(getEnv("SESSION_DAYS", "90"))
	if err != nil || days <= 0 {
		return nil, fmt.Errorf("SESSION_DAYS must be a positive integer (got %q)", os.Getenv("SESSION_DAYS"))
	}
	c.SessionDays = days

	scopes := getEnv("EVE_SCOPES", strings.Join(defaultScopes, " "))
	c.Scopes = strings.Fields(scopes)

	keyHex := os.Getenv("ENCRYPTION_KEY")
	if keyHex == "" {
		return nil, fmt.Errorf("ENCRYPTION_KEY must be set (64 hex chars = 32 bytes)")
	}
	key, err := hex.DecodeString(keyHex)
	if err != nil || len(key) != 32 {
		return nil, fmt.Errorf("ENCRYPTION_KEY must be 64 hex characters (32 bytes)")
	}
	c.EncryptionKey = key

	return c, nil
}

// defaultScopes is exactly what the cabinet calls, and nothing else.
//
// GRABLE: не проси «все scopes». Полный список EVE — 72 разрешения и
// 2.4 КБ текста; login.eveonline.com отвечает на такой authorize-запрос
// «request too long» и логин не проходит вовсе. Обходной путь (открыть
// персонажа в отдельном окне, потом позвать авторизацию) лечит симптом,
// а не причину. Ниже — минимальный набор под РЕАЛЬНО используемые
// эндпоинты; добавляя новый вызов ESI, добавляй сюда его scope.
var defaultScopes = []string{
	"publicData",

	// персонаж: где, на чём, в сети ли
	"esi-location.read_location.v1",  // /characters/{id}/location/
	"esi-location.read_ship_type.v1", // /characters/{id}/ship/
	"esi-location.read_online.v1",    // /characters/{id}/online/
	"esi-ui.write_waypoint.v1",       // /ui/autopilot/waypoint/ (массовая прокладка)

	// навыки, атрибуты, клоны
	"esi-skills.read_skills.v1",     // /skills/ и /attributes/
	"esi-skills.read_skillqueue.v1", // /skillqueue/
	"esi-clones.read_clones.v1",     // /clones/
	"esi-clones.read_implants.v1",   // /implants/

	// кошелёк и рынок
	"esi-wallet.read_character_wallet.v1",  // /wallet/, /journal/, /transactions/
	"esi-markets.read_character_orders.v1", // /orders/
	"esi-markets.structure_markets.v1",     // /markets/structures/{id}/

	// имущество, чертежи, производство
	"esi-assets.read_assets.v1",             // /assets/
	"esi-characters.read_blueprints.v1",     // /blueprints/
	"esi-industry.read_character_jobs.v1",   // /industry/jobs/
	"esi-industry.read_character_mining.v1", // /mining/ (леджер добычи)

	// планетарка (manage_planets — единственный scope планет, GET-only)
	"esi-planets.manage_planets.v1",

	// почта, уведомления, лояльность
	"esi-mail.read_mail.v1",                // /mail/, /mail/labels/, /mail/lists/
	"esi-characters.read_notifications.v1", // /notifications/
	"esi-characters.read_loyalty.v1",       // /loyalty/points/

	// структуры: имена цитаделей и корпоративный список
	"esi-universe.read_structures.v1",     // /universe/structures/{id}/
	"esi-corporations.read_structures.v1", // /corporations/{id}/structures/
	// поиск цитадели по имени для прокладки маршрута: /universe/ids/ игрокских
	// структур не знает в принципе, их видит только авторизованный поиск (ACL).
	// ГРАБЛЯ: у старых токенов права нет — до перелогина маршрут ляжет только
	// до системы; хватает одного перелогиненного пилота среди выбранных.
	"esi-search.search_structures.v1", // /characters/{id}/search/?categories=structure

	// корпорация
	"esi-corporations.read_divisions.v1",      // /divisions/
	"esi-corporations.read_blueprints.v1",     // /corporations/{id}/blueprints/
	"esi-corporations.read_projects.v1",       // /corporations/{id}/projects
	"esi-assets.read_corporation_assets.v1",   // /corporations/{id}/assets/
	"esi-industry.read_corporation_jobs.v1",   // /corporations/{id}/industry/jobs/
	"esi-industry.read_corporation_mining.v1", // /corporation/{id}/mining/...
	"esi-wallet.read_corporation_wallets.v1",  // /corporations/{id}/wallets/...

	// флот: единственное место, где кабинет пишет в игру
	"esi-fleets.read_fleet.v1",
	"esi-fleets.write_fleet.v1",

	// учёт ТМЦ: контракты объясняют, почему имущество ушло из ангара
	// (курьерка, передача между альтами), килмейлы дают списание потерь.
	// ГРАБЛЯ: пока альт не перелогинится, старый токен этих прав не имеет,
	// и ESI отвечает 401 «Token is not valid for any required scope».
	"esi-contracts.read_character_contracts.v1",   // /characters/{id}/contracts/
	"esi-contracts.read_corporation_contracts.v1", // /corporations/{id}/contracts/
	"esi-killmails.read_killmails.v1",             // /characters/{id}/killmails/recent/
}

// DefaultScopes returns the full scope set the cabinet asks for when
// EVE_SCOPES is not set. Exported for tests that check the map
// "section → scope" against the real list (internal/web/scopes_test.go).
func DefaultScopes() []string { return slices.Clone(defaultScopes) }

// isOff reads the usual ways of writing "no" in an .env file.
func isOff(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "off", "0", "false", "no":
		return true
	}
	return false
}

func getEnv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// loadDotEnv sets variables from a KEY=VALUE file without overriding
// values already present in the environment.
func loadDotEnv(path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		v = strings.Trim(strings.TrimSpace(v), `"'`)
		if os.Getenv(k) == "" {
			os.Setenv(k, v)
		}
	}
}

// ── пресеты прав ─────────────────────────────────────────────────────
//
// Вход всегда запрашивает набор прав целиком (решение 4 плана кабинета),
// поэтому выбор пресета делается ДО редиректа на login.eveonline.com и
// определяет, что окажется в токене. Что реально пришло — видно из JWT
// (`claims.scp`) и лежит в `tokens.scopes`; пресет нигде не хранится.

// Ключи пресетов. Подписи рабочие: владелец их ещё переименует.
const (
	// PresetAlt — свой альт: полный набор прав кабинета.
	PresetAlt = "alt"
	// PresetIndustry — член корпорации: только навыки и производство.
	PresetIndustry = "industry"
)

// Preset — набор прав, который можно запросить на входе.
type Preset struct {
	Key    string
	Title  string
	Scopes []string
}

// industryScopes — ровно четыре права: публичная карточка, навыки,
// очередь обучения и производственные задания. Больше кабинету члена
// корпорации не нужно, и просить больше нельзя.
var industryScopes = []string{
	"publicData",
	"esi-skills.read_skills.v1",
	"esi-skills.read_skillqueue.v1",
	"esi-industry.read_character_jobs.v1",
}

// Presets перечисляет пресеты в порядке показа на странице входа.
// Полный набор берётся из c.Scopes — то есть уже с учётом EVE_SCOPES.
func (c *Config) Presets() []Preset {
	return []Preset{
		{Key: PresetAlt, Title: "Альт", Scopes: c.Scopes},
		{Key: PresetIndustry, Title: "Производство", Scopes: industryScopes},
	}
}

// PresetScopes возвращает права пресета. Неизвестный ключ — false:
// подставлять вместо него полный набор нельзя, иначе опечатка в ссылке
// молча попросит у человека все права.
func (c *Config) PresetScopes(key string) ([]string, bool) {
	for _, p := range c.Presets() {
		if p.Key == key {
			return p.Scopes, true
		}
	}
	return nil, false
}
