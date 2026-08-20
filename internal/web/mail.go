package web

// EVE mail tab of a character, mirroring the in-game window: the
// «Письма» pane (folder menu, 50-a-page list, letters open in the item
// modal via /api/mail/) and the «Уведомления» pane — the client's second
// mail tab, which is the notifications feed, not mail at all.
//
// Folder state is plain GET (?l= label, ?ml= mailing list, ?before=
// paging) so the stale/revalidate machinery works unchanged. Mailing
// lists cannot be filtered server-side in ESI — the handler scans the
// unfiltered feed and matches recipients.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"sync"
	"time"

	"eve-empire/internal/esi"
	"eve-empire/internal/sde"
)

// mailFolderView is one entry of the folder menu.
type mailFolderView struct {
	Query  string // href query: "?l=1" or "?ml=145..."
	Name   string
	Unread int64
	Active bool
	List   bool // mailing list, not a label
}

// mailRowView is one line of the list.
type mailRowView struct {
	ID      int64
	Subject string
	Corr    string // sender — or the recipients when the character wrote it
	Sent    bool
	Time    time.Time
	Unread  bool
}

// notifRowView is one line of the notifications feed.
type notifRowView struct {
	Time   time.Time
	Label  string
	Sev    string // ready CSS class
	Sender string
	Text   string // raw YAML-ish blob, shown in the modal
}

// builtinMailFolders localizes the fixed labels (ESI names them in English).
var builtinMailFolders = map[int64]string{
	1: "Входящие", 2: "Отправленные", 4: "Корпорация", 8: "Альянс",
}

// notifTypesRu localizes the non-structure notification types worth a
// friendly name; structure ones reuse structNotifTypes. Unknown types
// fall back to the raw ESI name — honest, if ugly.
var notifTypesRu = map[string]string{
	"InsurancePayoutMsg":            "страховая выплата",
	"InsuranceIssuedMsg":            "страховка оформлена",
	"InsuranceExpirationMsg":        "страховка истекла",
	"InsuranceInvalidatedMsg":       "страховка аннулирована",
	"InsuranceFirstShipMsg":         "страховка первого корабля",
	"KillReportVictim":              "ваш корабль уничтожен",
	"KillReportFinalBlow":           "решающий удар (килмейл)",
	"KillRightAvailable":            "доступно право на убийство",
	"CharAppAcceptMsg":              "заявка в корпорацию принята",
	"CharAppRejectMsg":              "заявка в корпорацию отклонена",
	"CharAppWithdrawMsg":            "заявка в корпорацию отозвана",
	"CharLeftCorpMsg":               "персонаж покинул корпорацию",
	"CorpAppNewMsg":                 "новая заявка в корпорацию",
	"CorpAppInvitedMsg":             "приглашение в корпорацию",
	"CorpAppRejectCustomMsg":        "заявка отклонена",
	"CorpNewCEOMsg":                 "новый CEO корпорации",
	"CorpCEOChangeReqMsg":           "запрос смены CEO",
	"CorpTaxChangeMsg":              "изменён налог корпорации",
	"CorpDividendMsg":               "дивиденды корпорации",
	"CorpVoteMsg":                   "голосование в корпорации",
	"CorpVoteCEORevokedMsg":         "голосование отменено",
	"WarDeclared":                   "объявлена война",
	"CorpWarDeclaredMsg":            "корпорации объявлена война",
	"AllWarDeclaredMsg":             "альянсу объявлена война",
	"CorpWarSurrenderMsg":           "капитуляция в войне",
	"WarInvalid":                    "война завершена",
	"WarRetracted":                  "война отозвана",
	"WarRetractedByConcord":         "война отменена CONCORD",
	"WarConcordInvalidates":         "CONCORD аннулировал войну",
	"WarHQRemovedFromSpace":         "штаб войны уничтожен",
	"MoonminingExtractionStarted":   "начата экстракция луны",
	"MoonminingExtractionFinished":  "лунная порода готова",
	"MoonminingExtractionCancelled": "экстракция луны отменена",
	"MoonminingLaserFired":          "лунный чанк расколот",
	"MoonminingAutomaticFracture":   "лунный чанк раскололся сам",
	"TowerAlertMsg":                 "POS под атакой",
	"TowerResourceAlertMsg":         "у POS кончается топливо",
	"GameTimeReceived":              "получено игровое время",
	"GameTimeSent":                  "передано игровое время",
	"BillPaidCorpAllMsg":            "счёт корпорации оплачен",
	"CorpAllBillMsg":                "счёт корпорации",
	"BountyClaimMsg":                "выплата за голову",
	"ContactAdd":                    "добавлен в контакты",
	"ContactEdit":                   "изменён контакт",
	"IndustryOperationFinished":     "производственная работа завершена",
	"IndustryJobPausedFacilityOffline": "работа на паузе — структура офлайн",
	"OfferedSurrenderMsg":           "предложена капитуляция",
	"AcceptedSurrenderMsg":          "капитуляция принята",
	"MercOfferedNegotiationMsg":     "предложение наёмников",
	"FacWarCorpJoinRequestMsg":      "заявка корпорации в ФВ",
	"FacWarCorpLeaveRequestMsg":     "корпорация покидает ФВ",
	"CloneActivationMsg":            "активация клона",
	"JumpCloneDeletedMsg1":          "джамп-клон уничтожен",
	"JumpCloneDeletedMsg2":          "джамп-клон уничтожен",
	"CorpBecameWarEligible":         "корпорация может воевать",
	"CorpNoLongerWarEligible":       "корпорация вне войн",
	"MutualWarInviteSent":           "приглашение во взаимную войну",
	"ESSMainBankLink":               "ESS: доступ к банку",
	"DailyItemRewardAutoClaimed":    "ежедневная награда получена",
	"SeasonalChallengeCompleted":    "сезонное задание выполнено",
	"RaffleCreated":                 "лотерея HyperNet создана",
	"RaffleFinished":                "лотерея HyperNet завершена",
	"RaffleExpired":                 "лотерея HyperNet истекла",
	"NPCStandingsLost":              "упало отношение NPC",
	"NPCStandingsGained":            "выросло отношение NPC",
	"FreelanceProjectCreated":       "создан фриланс-проект",
	"FreelanceProjectClosed":        "фриланс-проект закрыт",
	"FreelanceProjectCompleted":     "фриланс-проект выполнен",
	"FreelanceProjectExpired":       "фриланс-проект истёк",
	"ExpertSystemExpired":           "экспертная система истекла",
	"GameTimeAdded":                 "добавлено игровое время",
	"SovAllClaimAquiredMsg":         "получен суверенитет",
	"SovAllClaimLostMsg":            "потерян суверенитет",
	"SovStructureDestroyed":         "сов-структура уничтожена",
	"SovStructureSelfDestructRequested": "сов-структура: запрошено самоуничтожение",
	"SovStructureSelfDestructCancel":    "сов-структура: самоуничтожение отменено",
	"SovStructureSelfDestructFinished":  "сов-структура самоуничтожилась",
	"AllianceCapitalChanged":        "сменилась столица альянса",
	"AllAnchoringMsg":               "якорение в системе альянса",
}

// notifLabel resolves a notification type to (label, severity class).
func notifLabel(typ string) (string, string) {
	if v, ok := structNotifTypes[typ]; ok {
		return v.Label, v.Sev
	}
	if v, ok := notifTypesRu[typ]; ok {
		return v, ""
	}
	return typ, ""
}

func (s *Server) handleMail(w http.ResponseWriter, r *http.Request) {
	ec, stale := s.esiFor(r)
	data, selected, ok := s.shellFor(w, r, ec, "mail")
	if !ok {
		return
	}
	var errs errList

	q := r.URL.Query()
	listID, _ := strconv.ParseInt(q.Get("ml"), 10, 64)
	labelID := int64(1) // inbox
	if raw := q.Get("l"); raw != "" {
		labelID, _ = strconv.ParseInt(raw, 10, 64)
	}
	before, _ := strconv.ParseInt(q.Get("before"), 10, 64)

	var (
		labels     *esi.MailLabels
		lists      []esi.MailingList
		heads      []esi.MailHeader
		notifs     []esi.Notification
		olderAfter int64 // cursor for the "older" link, 0 = feed exhausted
		wg         sync.WaitGroup
	)
	wg.Add(3)
	go func() {
		defer wg.Done()
		var err error
		if labels, err = ec.MailLabelList(selected.ID); err != nil {
			errs.add("метки", err)
		}
	}()
	go func() {
		defer wg.Done()
		lists, _ = ec.MailingLists(selected.ID)
	}()
	go func() {
		defer wg.Done()
		notifs, _ = ec.Notifications(selected.ID)
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		var err error
		if listID != 0 {
			// ESI cannot filter by mailing list — scan the unfiltered
			// feed and match recipients. Capped so one click cannot
			// spin through a decade of mail.
			heads, olderAfter, err = scanMailingList(ec, selected.ID, listID, before)
		} else {
			heads, err = ec.MailHeaders(selected.ID, labelID, before)
			if len(heads) == 50 {
				olderAfter = heads[len(heads)-1].MailID
			}
		}
		if err != nil {
			errs.add("письма", err)
		}
	}()
	wg.Wait()

	// Mailing-list ids must not go into the /universe/names/ batch —
	// they would fail the whole request. Their names live in lists.
	listNames := map[int64]string{}
	for _, l := range lists {
		listNames[l.MailingListID] = l.Name
	}
	var nameIDs []int64
	for _, h := range heads {
		nameIDs = append(nameIDs, h.From)
		for _, rc := range h.Recipients {
			if rc.RecipientType != "mailing_list" {
				nameIDs = append(nameIDs, rc.RecipientID)
			}
		}
	}
	for _, n := range notifs {
		if n.SenderID != 0 {
			nameIDs = append(nameIDs, n.SenderID)
		}
	}
	names := ec.Names(nameIDs)
	nameOf := func(id int64, typ string) string {
		if typ == "mailing_list" {
			if n := listNames[id]; n != "" {
				return "рассылка «" + n + "»"
			}
			return "рассылка " + strconv.FormatInt(id, 10)
		}
		if n := names[id]; n != "" {
			return n
		}
		return strconv.FormatInt(id, 10)
	}

	// ── folder menu ──
	folders := []mailFolderView{{Query: "?l=0", Name: "Все письма", Active: labelID == 0 && listID == 0}}
	if labels != nil {
		folders[0].Unread = labels.TotalUnread
		for _, l := range labels.Labels {
			name := builtinMailFolders[l.LabelID]
			if name == "" {
				name = l.Name
			}
			folders = append(folders, mailFolderView{
				Query: fmt.Sprintf("?l=%d", l.LabelID), Name: name,
				Unread: l.UnreadCount, Active: listID == 0 && l.LabelID == labelID,
			})
		}
	}
	for _, l := range lists {
		folders = append(folders, mailFolderView{
			Query: fmt.Sprintf("?ml=%d", l.MailingListID), Name: l.Name,
			Active: listID == l.MailingListID, List: true,
		})
	}

	// ── list ──
	rows := make([]mailRowView, 0, len(heads))
	for _, h := range heads {
		row := mailRowView{
			ID: h.MailID, Subject: h.Subject, Time: h.Timestamp, Unread: !h.IsRead,
		}
		if row.Subject == "" {
			row.Subject = "(без темы)"
		}
		if h.From == selected.ID {
			row.Sent = true
			var to []string
			for _, rc := range h.Recipients {
				to = append(to, nameOf(rc.RecipientID, rc.RecipientType))
			}
			row.Corr = joinList(to)
		} else {
			row.Corr = nameOf(h.From, "")
		}
		rows = append(rows, row)
	}

	// ── notifications feed ──
	sort.Slice(notifs, func(i, j int) bool { return notifs[i].Timestamp.After(notifs[j].Timestamp) })
	if len(notifs) > 200 {
		notifs = notifs[:200]
	}
	nrows := make([]notifRowView, 0, len(notifs))
	for _, n := range notifs {
		nv := notifRowView{Time: n.Timestamp, Text: n.Text}
		nv.Label, nv.Sev = notifLabel(n.Type)
		if n.SenderID != 0 {
			nv.Sender = names[n.SenderID]
		}
		nrows = append(nrows, nv)
	}

	base := fmt.Sprintf("?l=%d", labelID)
	if listID != 0 {
		base = fmt.Sprintf("?ml=%d", listID)
	}
	data["Folders"] = folders
	data["Base"] = base
	data["Rows"] = rows
	data["Notifs"] = nrows
	data["OlderBefore"] = olderAfter
	data["Paged"] = before != 0
	data["Errors"] = errs.list
	s.render(w, "mail", data, stale)
}

// scanMailingList walks the unfiltered feed from the cursor and keeps
// mails addressed to the list. Returns the next cursor (0 when the feed
// ran dry within the scan window).
func scanMailingList(ec *esi.Client, charID, listID, cursor int64) ([]esi.MailHeader, int64, error) {
	const maxPages = 20 // 1000 headers per click is plenty
	var out []esi.MailHeader
	for page := 0; page < maxPages && len(out) < 50; page++ {
		chunk, err := ec.MailHeaders(charID, 0, cursor)
		if err != nil {
			return out, 0, err
		}
		if len(chunk) == 0 {
			return out, 0, nil
		}
		for _, h := range chunk {
			for _, rc := range h.Recipients {
				if rc.RecipientType == "mailing_list" && rc.RecipientID == listID {
					out = append(out, h)
					break
				}
			}
		}
		cursor = chunk[len(chunk)-1].MailID
		if len(chunk) < 50 {
			return out, 0, nil // reached the very first mail
		}
	}
	return out, cursor, nil
}

// handleMailJSON serves one letter for the modal: resolved names and a
// sanitized body. Strict client on purpose — the modal wants the letter
// now, and bodies cache for 30 days after the first fetch.
func (s *Server) handleMailJSON(w http.ResponseWriter, r *http.Request) {
	charID, err1 := strconv.ParseInt(r.PathValue("id"), 10, 64)
	mailID, err2 := strconv.ParseInt(r.PathValue("mail"), 10, 64)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if err1 != nil || err2 != nil {
		json.NewEncoder(w).Encode(map[string]string{"error": "bad id"})
		return
	}
	letter, err := s.ESI.MailBody(charID, mailID)
	if err != nil {
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	listNames := map[int64]string{}
	if lists, err := s.ESI.MailingLists(charID); err == nil {
		for _, l := range lists {
			listNames[l.MailingListID] = l.Name
		}
	}
	var nameIDs []int64
	if letter.From != 0 {
		nameIDs = append(nameIDs, letter.From)
	}
	for _, rc := range letter.Recipients {
		if rc.RecipientType != "mailing_list" {
			nameIDs = append(nameIDs, rc.RecipientID)
		}
	}
	names := s.ESI.Names(nameIDs)
	var to []string
	for _, rc := range letter.Recipients {
		if rc.RecipientType == "mailing_list" {
			if n := listNames[rc.RecipientID]; n != "" {
				to = append(to, "рассылка «"+n+"»")
			} else {
				to = append(to, "рассылка "+strconv.FormatInt(rc.RecipientID, 10))
			}
		} else if n := names[rc.RecipientID]; n != "" {
			to = append(to, n)
		} else {
			to = append(to, strconv.FormatInt(rc.RecipientID, 10))
		}
	}
	subject := letter.Subject
	if subject == "" {
		subject = "(без темы)"
	}
	from := names[letter.From]
	if from == "" {
		from = strconv.FormatInt(letter.From, 10)
	}
	json.NewEncoder(w).Encode(map[string]string{
		"subject": subject,
		"from":    from,
		"to":      joinList(to),
		"time":    letter.Timestamp.Format("02.01.2006 15:04"),
		"body":    sde.DescriptionHTML(letter.Body),
	})
}

// joinList joins names, compressing long recipient lists.
func joinList(names []string) string {
	const max = 3
	out := ""
	for i, n := range names {
		if i == max {
			return out + " и ещё " + strconv.Itoa(len(names)-max)
		}
		if i > 0 {
			out += ", "
		}
		out += n
	}
	return out
}
