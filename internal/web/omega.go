package web

import (
	"errors"
	"fmt"
	"math"
	"net/http"
	"strings"
	"time"

	"eve-empire/internal/store"
)

// Омега и учебные места аккаунта. ESI статус подписки не отдаёт ничем,
// поэтому даты вводятся руками со страницы настроек — как их показывает
// окно «Учётная запись» в клиенте: омега с точностью до минуты, доп.
// учебные места датой. Всё в EVE-времени (UTC).

// Пороги подсветки: меньше двух недель — жёлтый, меньше недели — красный.
const (
	omegaWarn = 14 * 24 * time.Hour
	omegaErr  = 7 * 24 * time.Hour
)

// omegaSlot is one subscription date prepared for the templates.
type omegaSlot struct {
	Raw      string    // stored value ('YYYY-MM-DD[ HH:MM]'), '' = not set
	Disp     string    // game-style 'YYYY.MM.DD[ HH:MM]' for display
	Deadline time.Time // effective deadline; date-only entries expire at day's end
	Days     int       // full days remaining, rounded up; 0 when expired
	Expired  bool
	Class    string // '' | 'warn' | 'err' — the sev-* palette
}

// omegaView is everything the sidebar header and the summary card need
// for one account.
type omegaView struct {
	Omega omegaSlot
	MCT1  omegaSlot
	MCT2  omegaSlot
	Any   bool   // at least one date entered
	Label string // sidebar badge, e.g. 'Ω 12д'
	Class string // sidebar badge severity
	Title string // sidebar badge tooltip: all entered dates
}

// parseOmegaDate normalizes hand-typed input to the stored form.
// Accepted: '2027.03.01 05:20', '2027-03-01 05:20', '2027.03.01',
// '2027-03-01'; empty stays empty.
func parseOmegaDate(v string) (string, error) {
	v = strings.Join(strings.Fields(strings.ReplaceAll(v, ".", "-")), " ")
	if v == "" {
		return "", nil
	}
	for _, layout := range []string{"2006-01-02 15:04", "2006-01-02"} {
		if t, err := time.Parse(layout, v); err == nil {
			return t.Format(layout), nil
		}
	}
	return "", fmt.Errorf("дата %q: нужен формат ГГГГ.ММ.ДД или ГГГГ.ММ.ДД ЧЧ:ММ", v)
}

// omegaDeadline turns a stored date into the moment it expires: an entry
// without a time keeps working through that whole day.
func omegaDeadline(raw string) time.Time {
	if t, err := time.Parse("2006-01-02 15:04", raw); err == nil {
		return t
	}
	if t, err := time.Parse("2006-01-02", raw); err == nil {
		return t.Add(24 * time.Hour)
	}
	return time.Time{}
}

func newOmegaSlot(raw string, now time.Time) omegaSlot {
	sl := omegaSlot{Raw: raw}
	if raw == "" {
		return sl
	}
	sl.Disp = strings.ReplaceAll(raw, "-", ".")
	sl.Deadline = omegaDeadline(raw)
	left := sl.Deadline.Sub(now)
	switch {
	case left <= 0:
		sl.Expired, sl.Class = true, "err"
	case left <= omegaErr:
		sl.Class = "err"
	case left <= omegaWarn:
		sl.Class = "warn"
	}
	if left > 0 {
		sl.Days = int(math.Ceil(left.Hours() / 24))
	}
	return sl
}

func newOmegaView(o store.AccountOmega, now time.Time) omegaView {
	v := omegaView{
		Omega: newOmegaSlot(o.OmegaUntil, now),
		MCT1:  newOmegaSlot(o.MCT1Until, now),
		MCT2:  newOmegaSlot(o.MCT2Until, now),
	}

	// The badge counts down to the nearest deadline still ahead; an
	// already-expired slot only paints it red. The tooltip has the detail.
	var nearest *omegaSlot
	var title []string
	expired := false
	for _, s := range []struct {
		name string
		sl   *omegaSlot
	}{{"Омега", &v.Omega}, {"Уч. место 1", &v.MCT1}, {"Уч. место 2", &v.MCT2}} {
		if s.sl.Raw == "" {
			continue
		}
		v.Any = true
		if s.sl.Expired {
			expired = true
			title = append(title, s.name+": "+s.sl.Disp+" (истёк)")
			continue
		}
		title = append(title, fmt.Sprintf("%s: %s (%dд)", s.name, s.sl.Disp, s.sl.Days))
		if nearest == nil || s.sl.Deadline.Before(nearest.Deadline) {
			nearest = s.sl
		}
	}
	v.Title = strings.Join(title, "\n")
	switch {
	case nearest != nil:
		v.Label = fmt.Sprintf("Ω %dд", nearest.Days)
		v.Class = nearest.Class
	case v.Any: // everything entered has run out
		v.Label = "Ω истекла"
		v.Class = "err"
	}
	if expired {
		v.Class = "err"
	}
	return v
}

// handleRenameAccount renames an account: the label on every character,
// the sidebar order and the omega dates move together.
func (s *Server) handleRenameAccount(w http.ResponseWriter, r *http.Request) {
	oldName := strings.TrimSpace(r.FormValue("account"))
	newName := strings.TrimSpace(r.FormValue("name"))
	if oldName == "" || newName == "" {
		http.Error(w, "пустое имя аккаунта", http.StatusBadRequest)
		return
	}
	if newName != oldName {
		switch err := s.Store.RenameAccount(oldName, newName); {
		case errors.Is(err, store.ErrAccountExists):
			http.Error(w, "аккаунт "+newName+" уже существует", http.StatusBadRequest)
			return
		case err != nil:
			httpError(w, "renaming account", err)
			return
		}
	}
	http.Redirect(w, r, "/settings", http.StatusFound)
}

// handleSetAccountOmega saves the subscription dates of one account
// (settings page form).
func (s *Server) handleSetAccountOmega(w http.ResponseWriter, r *http.Request) {
	account := strings.TrimSpace(r.FormValue("account"))
	if account == "" {
		http.Error(w, "no account", http.StatusBadRequest)
		return
	}
	o := store.AccountOmega{Account: account}
	for _, f := range []struct {
		field string
		dst   *string
	}{{"omega", &o.OmegaUntil}, {"mct1", &o.MCT1Until}, {"mct2", &o.MCT2Until}} {
		v, err := parseOmegaDate(r.FormValue(f.field))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		*f.dst = v
	}
	if err := s.Store.SetAccountOmega(o); err != nil {
		httpError(w, "saving omega dates", err)
		return
	}
	http.Redirect(w, r, "/settings", http.StatusFound)
}
