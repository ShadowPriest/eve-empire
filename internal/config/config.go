package config

import (
	"bufio"
	"encoding/hex"
	"fmt"
	"os"
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
}

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
	}

	if c.ClientID == "" || c.ClientSecret == "" {
		return nil, fmt.Errorf("EVE_CLIENT_ID and EVE_CLIENT_SECRET must be set")
	}

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
