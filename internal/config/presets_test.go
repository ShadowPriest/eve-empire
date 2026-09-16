package config

import (
	"slices"
	"strings"
	"testing"
)

// full — конфиг с полным набором прав, каким его собирает Load без
// EVE_SCOPES в окружении.
func full() *Config { return &Config{Scopes: slices.Clone(defaultScopes)} }

// TestPresetAltIsTheConfiguredSet: «Альт» — ровно c.Scopes, то есть уже
// с учётом EVE_SCOPES, а не копия defaultScopes.
func TestPresetAltIsTheConfiguredSet(t *testing.T) {
	c := &Config{Scopes: []string{"publicData", "esi-skills.read_skills.v1"}}
	got, ok := c.PresetScopes(PresetAlt)
	if !ok {
		t.Fatal("пресета alt нет")
	}
	if !slices.Equal(got, c.Scopes) {
		t.Errorf("alt = %v, ждали c.Scopes = %v", got, c.Scopes)
	}
}

// TestPresetIndustryIsExactlyFour: узкий пресет — ровно четыре права, и
// каждое из них есть в полном наборе (иначе токен «производства» просил
// бы то, чего кабинет вообще не умеет).
func TestPresetIndustryIsExactlyFour(t *testing.T) {
	got, ok := full().PresetScopes(PresetIndustry)
	if !ok {
		t.Fatal("пресета industry нет")
	}
	want := []string{
		"publicData",
		"esi-skills.read_skills.v1",
		"esi-skills.read_skillqueue.v1",
		"esi-industry.read_character_jobs.v1",
	}
	if !slices.Equal(got, want) {
		t.Errorf("industry = %v, ждали %v", got, want)
	}
	for _, sc := range got {
		if !slices.Contains(defaultScopes, sc) {
			t.Errorf("право %q узкого пресета не входит в defaultScopes", sc)
		}
	}
}

// TestPresetScopesUnknownKey: неизвестный ключ не подставляет полный
// набор — опечатка в ссылке не должна молча просить все права.
func TestPresetScopesUnknownKey(t *testing.T) {
	for _, key := range []string{"", "ALT", "industry ", "прод"} {
		if got, ok := full().PresetScopes(key); ok {
			t.Errorf("PresetScopes(%q) = (%v, true), ждали false", key, got)
		}
	}
}

// TestPresetsHaveTitles: страница входа рисует подписи — пустых быть не
// должно, и ключи уникальны.
func TestPresetsHaveTitles(t *testing.T) {
	seen := map[string]bool{}
	for _, p := range full().Presets() {
		if strings.TrimSpace(p.Title) == "" {
			t.Errorf("у пресета %q нет подписи", p.Key)
		}
		if seen[p.Key] {
			t.Errorf("ключ %q повторяется", p.Key)
		}
		seen[p.Key] = true
	}
}
