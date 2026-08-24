package web

import "net/http"

// Re-authorization page. A database carried over from the other copy arrives
// with tokens issued by the other EVE application: they decrypt (the key is
// shared) but cannot be refreshed — EVE answers invalid_grant, because a
// refresh token belongs to the pair "application + character". The only fix is
// logging the characters in again, and with 27 alts that has to be a loop
// rather than 27 round-trips through the character card.
//
// SSO cannot be told which character to return; it shows the account's own
// picker. So the page offers one button and a progress list, not a button per
// character — the order is EVE's to choose.

// reauthRow is one character with the state of its stored token.
type reauthRow struct {
	ID    int64
	Name  string
	State string // ok | foreign | unknown
}

// reauthGroup collects the rows of one account, in sidebar order.
type reauthGroup struct {
	Account string
	Rows    []reauthRow
	Todo    int
}

func (s *Server) handleReauth(w http.ResponseWriter, r *http.Request) {
	ec, stale := s.esiFor(r)
	data, _, err := s.shell(ec, 0, "reauth")
	if err != nil {
		httpError(w, "loading characters", err)
		return
	}
	chars, err := s.Store.Characters()
	if err != nil {
		httpError(w, "loading characters", err)
		return
	}
	clients, err := s.Store.TokenClients()
	if err != nil {
		httpError(w, "loading token owners", err)
		return
	}

	var groups []reauthGroup
	idx := map[string]int{}
	todo := 0
	for _, ch := range chars {
		row := reauthRow{ID: ch.ID, Name: ch.Name}
		switch client := clients[ch.ID]; {
		case client == s.SSO.ClientID:
			row.State = "ok"
		case client == "":
			row.State = "unknown"
		default:
			row.State = "foreign"
		}
		if row.State != "ok" {
			todo++
		}

		name := ch.Account
		if name == "" {
			name = "без аккаунта"
		}
		gi, ok := idx[name]
		if !ok {
			gi = len(groups)
			idx[name] = gi
			groups = append(groups, reauthGroup{Account: name})
		}
		groups[gi].Rows = append(groups[gi].Rows, row)
		if row.State != "ok" {
			groups[gi].Todo++
		}
	}

	data["Accounts"] = groups
	data["Todo"] = todo
	data["Total"] = len(chars)
	s.render(w, "reauth", data, stale)
}
