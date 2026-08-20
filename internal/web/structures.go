package web

// Empire-wide structure report: per-corporation structure lists
// (fuel, vulnerability state, services — needs the Station Manager
// role) plus the structure notifications of every character, which are
// the only place ESI reports attacks (StructureUnderAttack & co).

import (
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"eve-empire/internal/esi"
)

// structView is one structure, decoded for display.
type structView struct {
	TypeID    int64
	TypeName  string
	Name      string
	System    string
	State     string // localized
	StateSev  string // ready CSS class: "", "sev-warn", "sev-err"
	StateEnd  *time.Time
	Fuel      *time.Time
	FuelLeft  string
	FuelSev   string // ready CSS class: pos / sev-warn / sev-err
	LowPower  bool
	Unanchors *time.Time
	Reinforce string // reinforce window, with the pending change if any
	Services  []structServiceView
}

type structServiceView struct {
	Name    string
	Online  bool
	Cleanup bool
}

type structCorpView struct {
	Corp       corpEntry
	Note       string
	Structures []structView
}

// structEventView is one structure notification, newest first.
type structEventView struct {
	Time      time.Time
	Label     string
	Sev       string // ready CSS class: "", "sev-warn", "sev-err", "pos"
	Structure string
	System    string
	Details   string
}

// structStates maps ESI vulnerability states to display. shield_vulnerable
// is the normal condition; the reinforce states mean the structure is
// under active attack.
var structStates = map[string]struct{ Label, Sev string }{
	"shield_vulnerable":    {"штатное (щит)", ""},
	"armor_vulnerable":     {"уязвима броня", "sev-warn"},
	"hull_vulnerable":      {"уязвим корпус", "sev-warn"},
	"armor_reinforce":      {"реинфорс: броня", "sev-err"},
	"hull_reinforce":       {"реинфорс: корпус", "sev-err"},
	"anchoring":            {"якорится", "sev-warn"},
	"anchor_vulnerable":    {"уязвима при якорении", "sev-warn"},
	"deploy_vulnerable":    {"уязвима при развёртывании", "sev-warn"},
	"fitting_invulnerable": {"неуязвима (фиттинг)", ""},
	"onlining_vulnerable":  {"включается (уязвима)", "sev-warn"},
	"unanchored":           {"снята с якоря", "sev-warn"},
	"unknown":              {"состояние неизвестно", ""},
}

// structNotifTypes maps notification types worth showing. Anything else
// starting with "Structure" still passes the filter and shows raw.
var structNotifTypes = map[string]struct{ Label, Sev string }{
	"StructureUnderAttack":     {"структура под атакой", "sev-err"},
	"StructureLostShields":     {"щит снят — реинфорс", "sev-err"},
	"StructureLostArmor":       {"броня снята — реинфорс", "sev-err"},
	"StructureDestroyed":       {"структура уничтожена", "sev-err"},
	"StructureFuelAlert":       {"топливо на исходе", "sev-warn"},
	"StructureWentLowPower":    {"перешла в low power", "sev-warn"},
	"StructureWentHighPower":   {"вернулась в high power", "pos"},
	"StructureOnline":          {"структура включена", "pos"},
	"StructureAnchoring":       {"структура якорится", ""},
	"StructureUnanchoring":     {"структура снимается с якоря", "sev-warn"},
	"StructureServicesOffline": {"сервисы отключены", "sev-warn"},
	"StructureImpendingAbandonmentAssetsAtRisk": {"скоро будет заброшена — имущество под угрозой", "sev-err"},
	"OwnershipTransferred":                      {"смена владельца структуры", "sev-warn"},
	"StructuresReinforcementChanged":            {"изменено окно реинфорса", ""},
	"StructuresJobsPaused":                      {"производственные работы поставлены на паузу", "sev-warn"},
	"StructuresJobsCancelled":                   {"производственные работы отменены", "sev-warn"},
	"StructureItemsMovedToSafety":               {"имущество перемещено в asset safety", "sev-warn"},
	"StructureItemsDelivered":                   {"имущество доставлено", ""},
}

// The notification text is CCP's YAML: "structureID: &id001 10354..." —
// the anchor between the key and the value is optional.
var (
	reNotifStructID = regexp.MustCompile(`(?m)^structureID:(?:\s+&\S+)?\s+(\d+)`)
	reNotifTypeID   = regexp.MustCompile(`(?m)^structureTypeID:\s+(\d+)`)
	reNotifSystem   = regexp.MustCompile(`(?m)^solarsystemID:\s+(\d+)`)
	reNotifAggrChar = regexp.MustCompile(`(?m)^charID:\s+(\d+)`)
	reNotifShield   = regexp.MustCompile(`(?m)^shieldPercentage:\s+([\d.]+)`)
	reNotifArmor    = regexp.MustCompile(`(?m)^armorPercentage:\s+([\d.]+)`)
	reNotifHull     = regexp.MustCompile(`(?m)^hullPercentage:\s+([\d.]+)`)
)

func notifInt(re *regexp.Regexp, text string) int64 {
	if m := re.FindStringSubmatch(text); m != nil {
		v, _ := strconv.ParseInt(m[1], 10, 64)
		return v
	}
	return 0
}

func notifPct(re *regexp.Regexp, text string) (float64, bool) {
	if m := re.FindStringSubmatch(text); m != nil {
		v, _ := strconv.ParseFloat(m[1], 64)
		return v, true
	}
	return 0, false
}

func (s *Server) handleEmpireStructures(w http.ResponseWriter, r *http.Request) {
	ec, stale := s.esiFor(r)
	data, _, err := s.shell(ec, 0, "")
	if err != nil {
		httpError(w, "loading characters", err)
		return
	}
	chars := empireChars(data)
	if len(chars) == 0 {
		s.render(w, "welcome", data, stale)
		return
	}
	corps, _ := data["Corporations"].([]corpEntry)
	var errs errList

	// Corp membership: the Station Manager role can live on any alt of
	// the corp, so every member is a candidate token.
	corpChars := map[int64][]int64{}
	{
		var mu sync.Mutex
		var wg sync.WaitGroup
		for _, ch := range chars {
			wg.Add(1)
			go func(id int64) {
				defer wg.Done()
				corpID, _, err := ec.CharacterPublic(id)
				if err != nil || corpID == 0 {
					return
				}
				mu.Lock()
				corpChars[corpID] = append(corpChars[corpID], id)
				mu.Unlock()
			}(ch.ID)
		}
		wg.Wait()
	}

	// ── structures per corporation ──
	views := make([]structCorpView, len(corps))
	rawByCorp := make([][]esi.CorpStructure, len(corps))
	var wg sync.WaitGroup
	for i, corp := range corps {
		wg.Add(1)
		go func(i int, corp corpEntry) {
			defer wg.Done()
			v := structCorpView{Corp: corp}
			candidates := corpChars[corp.ID]
			if len(candidates) == 0 {
				candidates = []int64{corp.ViaCharID}
			}
			var lastErr error
			for _, charID := range candidates {
				sts, err := ec.CorporationStructures(charID, corp.ID)
				if err == nil {
					rawByCorp[i] = sts
					lastErr = nil
					break
				}
				lastErr = err
				if !strings.Contains(err.Error(), "required role") {
					break
				}
			}
			if lastErr != nil {
				if strings.Contains(lastErr.Error(), "required role") {
					v.Note = "нет роли Station Manager ни у одного альта — список структур закрыт"
				} else {
					v.Note = lastErr.Error()
				}
			}
			views[i] = v
		}(i, corp)
	}

	// ── notifications of every character ──
	notifLists := make([][]esi.Notification, len(chars))
	for i, ch := range chars {
		wg.Add(1)
		go func(i int, ch sideChar) {
			defer wg.Done()
			ns, err := ec.Notifications(ch.ID)
			if err != nil {
				errs.add(ch.Name+": уведомления", err)
				return
			}
			notifLists[i] = ns
		}(i, ch)
	}
	wg.Wait()

	now := time.Now()
	structNames := map[int64]string{} // structure id -> name (for events)
	var systemIDs []int64
	for i := range corps {
		for _, st := range rawByCorp[i] {
			systemIDs = append(systemIDs, st.SystemID)
			structNames[st.StructureID] = st.Name
		}
	}

	// Events first: they may reference structures we no longer see.
	type rawEvent struct {
		n        esi.Notification
		charID   int64
		structID int64
		systemID int64
	}
	seen := map[string]bool{}
	var events []rawEvent
	for i, ch := range chars {
		for _, n := range notifLists[i] {
			_, known := structNotifTypes[n.Type]
			if !known && !strings.HasPrefix(n.Type, "Structure") {
				continue
			}
			structID := notifInt(reNotifStructID, n.Text)
			key := fmt.Sprintf("%s|%d|%d", n.Type, n.Timestamp.Unix(), structID)
			if seen[key] {
				continue // every alt of the corp receives the same event
			}
			seen[key] = true
			sysID := notifInt(reNotifSystem, n.Text)
			if sysID != 0 {
				systemIDs = append(systemIDs, sysID)
			}
			events = append(events, rawEvent{n: n, charID: ch.ID, structID: structID, systemID: sysID})
		}
	}
	sort.Slice(events, func(i, j int) bool { return events[i].n.Timestamp.After(events[j].n.Timestamp) })
	if len(events) > 80 {
		events = events[:80]
	}

	// Aggressor names ride the same batch as system names. charID means
	// "attacker" only in attack notifications — elsewhere it is whoever
	// delivered/moved the items.
	isAttack := func(typ string) bool {
		switch typ {
		case "StructureUnderAttack", "StructureLostShields", "StructureLostArmor", "StructureDestroyed":
			return true
		}
		return false
	}
	var aggrIDs []int64
	for _, ev := range events {
		if !isAttack(ev.n.Type) {
			continue
		}
		if id := notifInt(reNotifAggrChar, ev.n.Text); id != 0 {
			aggrIDs = append(aggrIDs, id)
		}
	}
	names := ec.Names(append(systemIDs, aggrIDs...))

	// ── decode structures ──
	totals := struct {
		Count, LowPower, Reinforced, FuelWeek int
		NextFuel                              *time.Time
		EventsErr                             int
	}{}
	for i := range corps {
		for _, st := range rawByCorp[i] {
			sv := structView{
				TypeID: st.TypeID, TypeName: st.TypeName, Name: st.Name,
				System: names[st.SystemID],
				Fuel:   st.FuelExpires, Unanchors: st.UnanchorsAt,
				StateEnd: st.StateTimerEnd,
			}
			if lbl, ok := structStates[st.State]; ok {
				sv.State, sv.StateSev = lbl.Label, lbl.Sev
			} else {
				sv.State = st.State
			}
			if sv.StateSev == "sev-err" {
				totals.Reinforced++
			}
			if st.FuelExpires == nil || st.FuelExpires.Before(now) {
				sv.LowPower = true
				totals.LowPower++
			} else {
				left := st.FuelExpires.Sub(now)
				sv.FuelLeft = humanDur(left)
				switch {
				case left < 48*time.Hour:
					sv.FuelSev = "sev-err"
				case left < 7*24*time.Hour:
					sv.FuelSev = "sev-warn"
				default:
					sv.FuelSev = "pos"
				}
				if left < 7*24*time.Hour {
					totals.FuelWeek++
				}
				if totals.NextFuel == nil || st.FuelExpires.Before(*totals.NextFuel) {
					totals.NextFuel = st.FuelExpires
				}
			}
			if st.ReinforceHour != nil {
				sv.Reinforce = fmt.Sprintf("%02d:00 ±2ч", *st.ReinforceHour)
				if st.NextReinforceHour != nil && st.NextReinforceApply != nil {
					sv.Reinforce += fmt.Sprintf(" → %02d:00 с %s",
						*st.NextReinforceHour, st.NextReinforceApply.Format("02.01"))
				}
			}
			for _, svc := range st.Services {
				sv.Services = append(sv.Services, structServiceView{
					Name: svc.Name, Online: svc.State == "online", Cleanup: svc.State == "cleanup",
				})
			}
			views[i].Structures = append(views[i].Structures, sv)
			totals.Count++
		}
		sort.Slice(views[i].Structures, func(a, b int) bool {
			return views[i].Structures[a].Name < views[i].Structures[b].Name
		})
	}

	// ── decode events ──
	weekAgo := now.Add(-7 * 24 * time.Hour)
	evViews := make([]structEventView, 0, len(events))
	for _, ev := range events {
		v := structEventView{Time: ev.n.Timestamp}
		if lbl, ok := structNotifTypes[ev.n.Type]; ok {
			v.Label, v.Sev = lbl.Label, lbl.Sev
		} else {
			v.Label = ev.n.Type
		}
		if v.Sev == "sev-err" && ev.n.Timestamp.After(weekAgo) {
			totals.EventsErr++
		}
		if ev.structID != 0 {
			if n := structNames[ev.structID]; n != "" {
				v.Structure = n
			} else {
				// Not in any list we can read (destroyed, other corp) —
				// try the paid lookup once, else show the id.
				ln := ec.LocationNames(ev.charID, []int64{ev.structID})
				v.Structure = ln[ev.structID]
				structNames[ev.structID] = v.Structure
			}
		}
		v.System = names[ev.systemID]
		var parts []string
		if sh, ok := notifPct(reNotifShield, ev.n.Text); ok {
			ar, _ := notifPct(reNotifArmor, ev.n.Text)
			hu, _ := notifPct(reNotifHull, ev.n.Text)
			parts = append(parts, fmt.Sprintf("щит %.1f%% · броня %.1f%% · корпус %.1f%%", sh, ar, hu))
		}
		if isAttack(ev.n.Type) {
			if id := notifInt(reNotifAggrChar, ev.n.Text); id != 0 && names[id] != "" {
				parts = append(parts, "атакует "+names[id])
			}
		}
		v.Details = strings.Join(parts, " · ")
		evViews = append(evViews, v)
	}

	data["Corps"] = views
	data["Events"] = evViews
	data["Totals"] = totals
	data["Errors"] = errs.list
	s.render(w, "empire_structures", data, stale)
}
