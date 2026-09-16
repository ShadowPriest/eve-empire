package web

// Ошибки в шапке страницы. Ошибки опроса ESI (мёртвые токены, отказы
// ESI, пустой кэш) живут за красной точкой индикатора, сгруппированные по
// сути: один персонаж с отклонённым токеном даёт по строке на каждую
// ручку, а читать надо одну. Остальные ошибки страницы (база, разбор
// данных) — за жёлтым «!» слева от точки. В тело страниц ни те ни
// другие больше не попадают.

import (
	"regexp"
	"strconv"
	"strings"
)

// esiErrGroup — одна строка списка за красной точкой.
type esiErrGroup struct {
	Msg   string   // суть
	Names []string // кого задело (когда суть общая на нескольких персонажей)
	Kinds []string // какие ручки задело: работы, навыки, колонии…
	N     int      // сколько сырых строк свернулось в эту
}

// esiErrMarks — по чему строка ошибки признаётся ошибкой опроса ESI.
// Строки приходят готовым текстом (из view и из errList страниц), так
// что классифицируем по содержимому.
var esiErrMarks = []string{"нет токена", "нет кэша", "esi http", "esi.evetech", "sso token", "ESI"}

func isESIErr(msg string) bool {
	for _, m := range esiErrMarks {
		if strings.Contains(msg, m) {
			return true
		}
	}
	return false
}

var (
	reErrCharID = regexp.MustCompile(`персонаж(а)? (\d+)`)
	// «esi https://esi.evetech.net/latest/corporations/1/industry/jobs/?language=ru»
	// → «esi corporations/1/industry/jobs/»
	reErrURL = regexp.MustCompile(`esi https?://esi\.evetech\.net/[^/\s]+/([^\s?]+)(\?[^\s:]*)?`)
	// ответ ESI в фигурных скобках — оставить только текст error
	reErrJSON = regexp.MustCompile(`\{"error":"([^"]*)"[^}]*\}`)
)

const errTokenRejected = "токен отклонён SSO, нужен перелогин на /reauth"

// esiErrCore отделяет суть ошибки от префикса «где случилось».
func esiErrCore(msg string) (kind, core string) {
	for _, mark := range []string{"нет токена", "нет кэша", "esi http"} {
		if i := strings.Index(msg, mark); i >= 0 {
			return strings.TrimSuffix(strings.TrimSpace(msg[:i]), ":"), msg[i:]
		}
	}
	if i := strings.Index(msg, ": "); i >= 0 {
		return msg[:i], msg[i+2:]
	}
	return "", msg
}

// groupESIErrors сворачивает сырые строки в читаемый список: имена
// вместо id, одинаковая суть — одной строкой с перечнем персонажей и
// ручек. reauth — есть ли среди ошибок отклонённые токены (тогда внизу
// ссылка на /reauth).
func groupESIErrors(msgs []string, names map[int64]string) (groups []esiErrGroup, reauth bool) {
	idx := map[string]int{}
	for _, m := range msgs {
		kind, core := esiErrCore(m)

		// Мёртвый токен звучит по-разному (отклонён при запросе, отклонён
		// при обновлении с invalid_grant), а действие одно — перелогин.
		var who string
		if sub := reErrCharID.FindStringSubmatch(core); sub != nil {
			id, _ := strconv.ParseInt(sub[2], 10, 64)
			who = names[id]
			if who == "" {
				who = "персонаж " + sub[2]
			}
		}
		if strings.Contains(core, "нет токена") && (strings.Contains(core, "invalid_grant") || strings.Contains(core, "токен отклонён SSO")) {
			core = errTokenRejected
			reauth = true
		} else {
			core = reErrCharID.ReplaceAllStringFunc(core, func(s string) string {
				sub := reErrCharID.FindStringSubmatch(s)
				if n := names[mustInt(sub[2])]; n != "" {
					return "персонаж" + sub[1] + " " + n
				}
				return s
			})
			who = ""
			if strings.Contains(core, "/reauth") {
				reauth = true
			}
		}
		core = reErrURL.ReplaceAllString(core, "esi $1")
		core = reErrJSON.ReplaceAllString(core, "($1)")

		// Страница подписывает свои ошибки именем персонажа; имя уже в
		// тексте — из префикса его убираем.
		for _, n := range names {
			if strings.HasPrefix(kind, n+": ") {
				kind = strings.TrimPrefix(kind, n+": ")
				break
			}
		}

		i, ok := idx[core]
		if !ok {
			i = len(groups)
			idx[core] = i
			groups = append(groups, esiErrGroup{Msg: core})
		}
		g := &groups[i]
		g.N++
		if who != "" && !containsStr(g.Names, who) {
			g.Names = append(g.Names, who)
		}
		if kind != "" && !containsStr(g.Kinds, kind) {
			g.Kinds = append(g.Kinds, kind)
		}
	}
	return groups, reauth
}

func mustInt(s string) int64 {
	v, _ := strconv.ParseInt(s, 10, 64)
	return v
}

func containsStr(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
