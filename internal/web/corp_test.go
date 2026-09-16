package web

// Раздел «Корпорация» (этап 5). Живого ESI в тестах нет, поэтому
// проверяем то, что от него не зависит: страница отвечает 200, пустое
// состояние честное, а чужое согласие её не роняет. Персонажи и
// корпорации выдуманы: репозиторий публичный.

import (
	"net/http"
	"strings"
	"testing"

	"eve-empire/internal/store"
)

func TestCorpPageWithoutCEOCorps(t *testing.T) {
	srv, st := testServer(t)
	h := srv.Routes()
	a := newCabinet(t, st, true, map[int64]string{9001: "Alpha One"})

	rec := a.do(h, "GET", "/corp", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("/corp: %d, ждали 200", rec.Code)
	}
	if body := rec.Body.String(); !strings.Contains(body, "Вы не CEO ни одной корпорации") {
		t.Errorf("/corp без своих корпораций не объясняет пустоту:\n%s", body)
	}
}

// Чужое согласие на странице не ломает рендер: без ESI членство
// подтвердить нельзя, и строка просто не показывается.
func TestCorpPageWithForeignShare(t *testing.T) {
	srv, st := testServer(t)
	h := srv.Routes()
	a := newCabinet(t, st, true, map[int64]string{9001: "Alpha One"})
	b := newCabinet(t, st, false, map[int64]string{9002: "Beta Two"})

	if err := st.SetCharShare(b.CharIDs[0], 7001,
		[]string{store.ShareSkills, store.ShareLines}); err != nil {
		t.Fatalf("SetCharShare: %v", err)
	}
	rec := a.do(h, "GET", "/corp", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("/corp: %d, ждали 200", rec.Code)
	}
	if body := rec.Body.String(); strings.Contains(body, "Beta Two") {
		t.Error("/corp показал персонажа, чьё членство не подтверждено публичной карточкой")
	}
}

// Согласием управляет только владелец персонажа: чужой id — 404.
func TestShareToggleOwnership(t *testing.T) {
	srv, st := testServer(t)
	h := srv.Routes()
	a := newCabinet(t, st, true, map[int64]string{9001: "Alpha One"})
	b := newCabinet(t, st, false, map[int64]string{9002: "Beta Two"})

	if err := st.SetCharShare(b.CharIDs[0], 7001, []string{store.ShareSkills}); err != nil {
		t.Fatalf("SetCharShare: %v", err)
	}
	if rec := a.do(h, "POST", "/characters/9002/share", "on=0"); rec.Code != http.StatusNotFound {
		t.Errorf("отзыв чужого согласия: %d, ждали 404", rec.Code)
	}
	if sh, err := st.CharShare(9002); err != nil || sh == nil {
		t.Errorf("чужое согласие снято: (%v, %v)", sh, err)
	}

	// Своё согласие снимается (включение требует ESI и здесь не проверяется).
	if err := st.SetCharShare(9001, 7001, []string{store.ShareSkills}); err != nil {
		t.Fatalf("SetCharShare: %v", err)
	}
	rec := a.do(h, "POST", "/characters/9001/share", "on=0")
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("отзыв своего согласия: %d, ждали 303", rec.Code)
	}
	if sh, err := st.CharShare(9001); err != nil || sh != nil {
		t.Errorf("своё согласие не снято: (%v, %v)", sh, err)
	}
}

// Настройки показывают галочку согласия у каждого персонажа кабинета.
func TestSettingsShowsShareToggle(t *testing.T) {
	srv, st := testServer(t)
	h := srv.Routes()
	a := newCabinet(t, st, true, map[int64]string{9001: "Alpha One"})

	rec := a.do(h, "GET", "/settings", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("/settings: %d, ждали 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "/characters/9001/share") ||
		!strings.Contains(body, "делиться с руководством корпорации") {
		t.Errorf("/settings без переключателя согласия:\n%s", body)
	}
}
