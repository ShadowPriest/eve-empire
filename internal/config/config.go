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

var defaultScopes = []string{
	"publicData",
	"esi-location.read_location.v1",
	"esi-location.read_ship_type.v1",
	"esi-location.read_online.v1",
	"esi-wallet.read_character_wallet.v1",
	"esi-skills.read_skills.v1",
	"esi-skills.read_skillqueue.v1",
	"esi-assets.read_assets.v1",
	"esi-industry.read_character_jobs.v1",
	"esi-markets.read_character_orders.v1",
	"esi-corporations.read_projects.v1",
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
