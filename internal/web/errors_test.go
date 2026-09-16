package web

import (
	"strings"
	"testing"
)

func TestGroupESIErrors(t *testing.T) {
	names := map[int64]string{1: "Alpha One", 2: "Beta Two"}
	msgs := []string{
		"работы: нет токена: персонаж 1 — токен отклонён SSO, нужен перелогин на /reauth",
		"навыки: нет токена: обновление токена персонажа 2: sso token endpoint: 400 Bad Request (invalid_grant: Invalid refresh token. Character grant missing/expired.)",
		"Alpha One: уведомления: нет токена: персонаж 1 — токен отклонён SSO, нужен перелогин на /reauth",
		"работы корпорации: esi https://esi.evetech.net/latest/corporations/98840146/industry/jobs/?language=ru: 401 Unauthorized: {\"error\":\"Unauthorized - No token provided\"}",
		"колонии: нет кэша (данные загружаются)",
		"колонии: нет кэша (данные загружаются)",
	}
	groups, reauth := groupESIErrors(msgs, names)
	if !reauth {
		t.Fatal("reauth: ожидалась ссылка на /reauth")
	}
	if len(groups) != 3 {
		t.Fatalf("групп: %d, ожидалось 3: %+v", len(groups), groups)
	}
	tok := groups[0]
	if tok.Msg != errTokenRejected || tok.N != 3 {
		t.Errorf("токены: %+v", tok)
	}
	if strings.Join(tok.Names, ",") != "Alpha One,Beta Two" {
		t.Errorf("имена: %v", tok.Names)
	}
	if strings.Join(tok.Kinds, ",") != "работы,навыки,уведомления" {
		t.Errorf("ручки: %v", tok.Kinds)
	}
	if want := "esi corporations/98840146/industry/jobs/: 401 Unauthorized: (Unauthorized - No token provided)"; groups[1].Msg != want {
		t.Errorf("url: %q", groups[1].Msg)
	}
	if groups[2].N != 2 || groups[2].Msg != "нет кэша (данные загружаются)" {
		t.Errorf("кэш: %+v", groups[2])
	}
	if isESIErr("реестр: database is locked") {
		t.Error("ошибка базы принята за ESI")
	}
}
