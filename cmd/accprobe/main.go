// Command accprobe is a throwaway probe for the container experiment
// behind ACCOUNTING.md §4.3: does a container keep its ESI item_id when
// it changes hands (personal hangar → corp hangar → another alt)?
//
//	go run ./cmd/accprobe -char "Allya Erquilenne" -label t1
//	go run ./cmd/accprobe -diff t1,t2
//
// It takes a snapshot of every asset row with its parent chain and the
// names of assembled items, writes it to JSON, and diffs two snapshots.
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"eve-empire/internal/collect"
	"eve-empire/internal/config"
	"eve-empire/internal/esi"
	"eve-empire/internal/sched"
	"eve-empire/internal/sso"
	"eve-empire/internal/store"
)

type row struct {
	ItemID       int64  `json:"item_id"`
	TypeID       int64  `json:"type_id"`
	TypeName     string `json:"type_name"`
	Name         string `json:"name,omitempty"` // only assembled items have one
	Quantity     int64  `json:"quantity"`
	Singleton    bool   `json:"singleton"`
	Flag         string `json:"flag"`
	LocationID   int64  `json:"location_id"`
	LocationType string `json:"location_type"`
	ParentItemID int64  `json:"parent_item_id"` // 0 = sits directly at a location
	RootID       int64  `json:"root_id"`
	RootName     string `json:"root_name"`
}

type ownerSnap struct {
	OwnerID   int64  `json:"owner_id"`
	OwnerName string `json:"owner_name"`
	Kind      string `json:"kind"` // char|corp
	Rows      []row  `json:"rows"`
}

type snapshot struct {
	Label  string      `json:"label"`
	At     time.Time   `json:"at"`
	Owners []ownerSnap `json:"owners"`
}

func snapDir() string {
	dir := os.Getenv("ACCPROBE_DIR")
	if dir == "" {
		dir = filepath.Join(os.TempDir(), "accprobe")
	}
	os.MkdirAll(dir, 0o755)
	return dir
}

func main() {
	chars := flag.String("char", "", "имена или id персонажей через запятую")
	withCorp := flag.Bool("corp", false, "добавить корпоративные ассеты (нужна роль Director)")
	label := flag.String("label", "", "имя снимка")
	diff := flag.String("diff", "", "сравнить два снимка: старый,новый")
	contracts := flag.Bool("contracts", false, "показать контракты персонажей вместо снимка")
	collectOne := flag.String("collect", "", "прогнать одну задачу сборщика: contracts|wallet|orders|jobs|assets")
	buildLedger := flag.String("build", "", "собрать реестр из истории: average|adjusted")
	recon := flag.Bool("recon", false, "показать расхождения реестра с действительностью")
	showReport := flag.Bool("report", false, "показать отчёты реестра")
	noCache := flag.Bool("nocache", true, "выбросить кэш ассетов перед запросом")
	flag.Parse()

	if *diff != "" {
		parts := strings.SplitN(*diff, ",", 2)
		if len(parts) != 2 {
			log.Fatal("-diff старый,новый")
		}
		compare(load(parts[0]), load(parts[1]))
		return
	}
	if *collectOne != "" {
		runCollector(*collectOne)
		return
	}
	if *recon {
		runRecon()
		return
	}
	if *buildLedger != "" || *showReport {
		runLedger(*buildLedger, *showReport)
		return
	}
	if *chars != "" && *contracts {
		dumpContracts(*chars)
		return
	}
	if *chars == "" || *label == "" {
		log.Fatal("нужны -char и -label (или -diff)")
	}

	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	if *noCache {
		purgeAssetCache(cfg.DBPath)
	}
	st, err := store.Open(cfg.DBPath, cfg.EncryptionKey)
	if err != nil {
		log.Fatalf("store: %v", err)
	}
	defer st.Close()
	ec := esi.New(sso.New(cfg.ClientID, cfg.ClientSecret, cfg.CallbackURL, cfg.Scopes, cfg.UserAgent), st, cfg.UserAgent)
	ec.SetLanguage(st.Setting("language"))

	known, err := st.Characters()
	if err != nil {
		log.Fatalf("characters: %v", err)
	}

	snap := snapshot{Label: *label, At: time.Now().UTC()}
	// Alts share corporations: read each corp's assets once.
	seenCorp := map[int64]bool{}
	for _, want := range strings.Split(*chars, ",") {
		want = strings.TrimSpace(want)
		if want == "" {
			continue
		}
		id, name := resolve(known, want)
		if id == 0 {
			log.Fatalf("персонаж %q не найден в базе", want)
		}
		rows, err := ec.CharacterAssetRows(id)
		if err != nil {
			log.Fatalf("ассеты %s: %v", name, err)
		}
		snap.Owners = append(snap.Owners, build(ec, id, name, "char", rows,
			func(ids []int64) (map[int64]string, error) { return ec.AssetNames(id, ids) }))

		if *withCorp {
			corpID, corpName, err := ec.CharacterPublic(id)
			if err != nil {
				log.Printf("корпорация %s: %v", name, err)
				continue
			}
			if seenCorp[corpID] {
				continue
			}
			seenCorp[corpID] = true
			rows, err := ec.CorporationAssetRows(id, corpID)
			if err != nil {
				log.Printf("корп-ассеты %s (%s): %v — нужна роль Director", corpName, name, err)
				continue
			}
			snap.Owners = append(snap.Owners, build(ec, id, corpName, "corp", rows,
				func(ids []int64) (map[int64]string, error) {
					return ec.CorporationAssetNames(id, corpID, ids)
				}))
		}
	}

	save(snap)
	report(snap)
}

// runCollector runs a single collector task right now instead of waiting
// for its slot in the scheduler. The tasks are idempotent, so running one
// by hand is safe and is the quickest way to see what it actually writes.
func runCollector(name string) {
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	st, err := store.Open(cfg.DBPath, cfg.EncryptionKey)
	if err != nil {
		log.Fatalf("store: %v", err)
	}
	defer st.Close()
	ec := esi.New(sso.New(cfg.ClientID, cfg.ClientSecret, cfg.CallbackURL, cfg.Scopes, cfg.UserAgent), st, cfg.UserAgent)
	ec.SetLanguage(st.Setting("language"))

	var task *sched.Task
	for _, t := range collect.New(ec, st, cfg.ClientID).Tasks() {
		if t.Name == name {
			t := t
			task = &t
		}
	}
	if task == nil {
		log.Fatalf("нет такой задачи: %s", name)
	}
	started := time.Now()
	if err := task.Run(context.Background()); err != nil {
		log.Printf("задача %s: %v", name, err)
	}
	fmt.Println("задача", name, "отработала за", time.Since(started).Round(time.Millisecond))
	for _, cs := range mustStatuses(st) {
		if cs.Task == name {
			fmt.Println("  ", cs.Note)
		}
	}
}

func mustStatuses(st *store.Store) []store.CollectorStatus {
	out, err := st.CollectorStatuses()
	if err != nil {
		return nil
	}
	return out
}

// dumpContracts prints each character's contracts newest first, with the
// cargo of the recent ones — the movement side of the container experiment.
func dumpContracts(chars string) {
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	st, err := store.Open(cfg.DBPath, cfg.EncryptionKey)
	if err != nil {
		log.Fatalf("store: %v", err)
	}
	defer st.Close()
	ec := esi.New(sso.New(cfg.ClientID, cfg.ClientSecret, cfg.CallbackURL, cfg.Scopes, cfg.UserAgent), st, cfg.UserAgent)
	ec.SetLanguage(st.Setting("language"))
	known, err := st.Characters()
	if err != nil {
		log.Fatalf("characters: %v", err)
	}

	for _, want := range strings.Split(chars, ",") {
		id, name := resolve(known, strings.TrimSpace(want))
		if id == 0 {
			continue
		}
		list, err := ec.CharacterContracts(id)
		if err != nil {
			log.Printf("контракты %s: %v", name, err)
			continue
		}
		sort.Slice(list, func(i, j int) bool { return list[i].DateIssued.After(list[j].DateIssued) })
		fmt.Printf("══ %s: %d контрактов\n", name, len(list))
		for i, k := range list {
			if i >= 3 {
				fmt.Printf("   … ещё %d\n", len(list)-3)
				break
			}
			var who []int64
			for _, p := range []int64{k.IssuerID, k.AssigneeID, k.AcceptorID} {
				if p != 0 {
					who = append(who, p)
				}
			}
			n := ec.Names(append(who, k.StartLocationID, k.EndLocationID))
			fmt.Printf("\n   #%d  %s / %s   выдан %s\n", k.ContractID, k.Type, k.Status,
				k.DateIssued.Format("2006-01-02 15:04"))
			fmt.Printf("      %s → %s, вёз %s\n", n[k.IssuerID], n[k.AssigneeID], n[k.AcceptorID])
			fmt.Printf("      маршрут: %s → %s\n", locLabel(ec, id, k.StartLocationID, n), locLabel(ec, id, k.EndLocationID, n))
			fmt.Printf("      награда %.2f · залог %.2f · цена %.2f · объём %.1f м³ · принят %s · завершён %s\n",
				k.Reward, k.Collateral, k.Price, k.Volume,
				stamp(k.DateAccepted), stamp(k.DateCompleted))
			items, err := ec.ContractItems(id, k.ContractID)
			if err != nil {
				fmt.Printf("      состав: %v\n", err)
				continue
			}
			var tids []int64
			for _, it := range items {
				tids = append(tids, it.TypeID)
			}
			tn := ec.Names(tids)
			for _, it := range items {
				fmt.Printf("      груз: %-36s %8d  record_id=%d singleton=%v\n",
					tn[it.TypeID], it.Quantity, it.RecordID, it.IsSingleton)
			}
		}
		fmt.Println()
	}
}

func stamp(t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	return t.Format("2006-01-02 15:04")
}

func locLabel(ec *esi.Client, viaChar, id int64, known map[int64]string) string {
	if n := known[id]; n != "" {
		return n
	}
	return locationNames(ec, viaChar, []int64{id})[id]
}

// resolve maps a name or id from the command line onto a stored character.
func resolve(known []store.Character, want string) (int64, string) {
	if id, err := strconv.ParseInt(want, 10, 64); err == nil {
		for _, c := range known {
			if c.ID == id {
				return c.ID, c.Name
			}
		}
		return 0, ""
	}
	for _, c := range known {
		if strings.EqualFold(c.Name, want) {
			return c.ID, c.Name
		}
	}
	// Fall back to a prefix match: the owner types names from memory.
	for _, c := range known {
		if strings.HasPrefix(strings.ToLower(c.Name), strings.ToLower(want)) {
			return c.ID, c.Name
		}
	}
	return 0, ""
}

// build turns raw ESI rows into snapshot rows: resolves the parent chain
// up to a real location, fetches type names, and asks for the names of
// assembled items (only those can carry one).
func build(ec *esi.Client, viaChar int64, ownerName, kind string, raw []esi.AssetRow,
	names func([]int64) (map[int64]string, error)) ownerSnap {

	byItem := map[int64]esi.AssetRow{}
	for _, a := range raw {
		byItem[a.ItemID] = a
	}
	rootOf := func(a esi.AssetRow) (root, parent int64) {
		loc := a.LocationID
		if _, nested := byItem[loc]; nested {
			parent = loc
		}
		for depth := 0; depth < 32; depth++ {
			p, ok := byItem[loc]
			if !ok {
				return loc, parent
			}
			loc = p.LocationID
		}
		return loc, parent
	}

	var typeIDs, nameIDs, locIDs []int64
	for _, a := range raw {
		typeIDs = append(typeIDs, a.TypeID)
		if a.IsSingleton {
			nameIDs = append(nameIDs, a.ItemID)
		}
	}
	typeNames := ec.Names(typeIDs)

	itemNames, err := names(nameIDs)
	if err != nil {
		log.Printf("имена ассетов %s: %v", ownerName, err)
	}

	out := ownerSnap{OwnerID: viaChar, OwnerName: ownerName, Kind: kind}
	for _, a := range raw {
		root, parent := rootOf(a)
		locIDs = append(locIDs, root)
		qty := a.Quantity
		if qty == 0 {
			qty = 1
		}
		out.Rows = append(out.Rows, row{
			ItemID: a.ItemID, TypeID: a.TypeID, TypeName: typeNames[a.TypeID],
			Name: itemNames[a.ItemID], Quantity: qty, Singleton: a.IsSingleton,
			Flag: a.LocationFlag, LocationID: a.LocationID, LocationType: a.LocationType,
			ParentItemID: parent, RootID: root,
		})
	}

	locNames := locationNames(ec, viaChar, locIDs)
	for i := range out.Rows {
		out.Rows[i].RootName = locNames[out.Rows[i].RootID]
	}
	return out
}

// locationNames resolves stations via /universe/names/ and structures via
// the authed endpoint — the batch route rejects ids >= 1e12.
func locationNames(ec *esi.Client, viaChar int64, ids []int64) map[int64]string {
	seen := map[int64]bool{}
	var plain []int64
	out := map[int64]string{}
	for _, id := range ids {
		if seen[id] {
			continue
		}
		seen[id] = true
		if id >= 1_000_000_000_000 {
			var st struct {
				Name string `json:"name"`
			}
			if b, err := ec.RawJSON(viaChar, fmt.Sprintf("/universe/structures/%d/", id)); err == nil {
				json.Unmarshal(b, &st)
			}
			if st.Name == "" {
				st.Name = fmt.Sprintf("Структура %d", id)
			}
			out[id] = st.Name
		} else {
			plain = append(plain, id)
		}
	}
	for id, n := range ec.Names(plain) {
		out[id] = n
	}
	for _, id := range plain {
		if out[id] == "" {
			out[id] = fmt.Sprintf("Локация %d", id)
		}
	}
	return out
}

// report prints the container tree: named assembled items first, with
// whatever sits inside them.
func report(s snapshot) {
	fmt.Printf("СНИМОК %s  %s\n\n", s.Label, s.At.Format("2006-01-02 15:04:05 UTC"))
	for _, o := range s.Owners {
		fmt.Printf("══ %s (%s, %d строк)\n", o.OwnerName, o.Kind, len(o.Rows))
		kids := map[int64][]row{}
		for _, r := range o.Rows {
			kids[r.ParentItemID] = append(kids[r.ParentItemID], r)
		}
		var carriers []row
		for _, r := range o.Rows {
			if r.Singleton && len(kids[r.ItemID]) > 0 {
				carriers = append(carriers, r)
			}
		}
		sort.Slice(carriers, func(i, j int) bool { return carriers[i].ItemID < carriers[j].ItemID })
		if len(carriers) == 0 {
			fmt.Println("   (ни одного непустого собранного контейнера)")
		}
		for _, c := range carriers {
			name := c.Name
			if name == "" {
				name = "БЕЗ ИМЕНИ"
			}
			fmt.Printf("\n   ┌ item_id %d  «%s»  [%s]\n", c.ItemID, name, c.TypeName)
			fmt.Printf("   │  где: %s · flag=%s · родитель=%d\n", c.RootName, c.Flag, c.ParentItemID)
			inside := kids[c.ItemID]
			sort.Slice(inside, func(i, j int) bool { return inside[i].Quantity > inside[j].Quantity })
			for _, k := range inside {
				fmt.Printf("   │    %-40s %12d  item_id %d flag=%s\n",
					k.TypeName, k.Quantity, k.ItemID, k.Flag)
			}
			fmt.Println("   └")
		}
		fmt.Println()
	}
}

func compare(oldS, newS snapshot) {
	fmt.Printf("ДИФФ  %s (%s)  →  %s (%s)\n\n",
		oldS.Label, oldS.At.Format("15:04:05"), newS.Label, newS.At.Format("15:04:05"))

	index := func(s snapshot) (map[int64]row, map[string]row) {
		byID := map[int64]row{}
		byName := map[string]row{}
		for _, o := range s.Owners {
			for _, r := range o.Rows {
				r.RootName = o.OwnerName + " · " + r.RootName
				byID[r.ItemID] = r
				if r.Name != "" {
					byName[r.Name] = r
				}
			}
		}
		return byID, byName
	}
	oldByID, oldByName := index(oldS)
	newByID, newByName := index(newS)

	// The question the experiment exists to answer.
	fmt.Println("── ИМЕНОВАННЫЕ КОНТЕЙНЕРЫ ──")
	seen := map[string]bool{}
	var allNames []string
	for n := range oldByName {
		allNames = append(allNames, n)
		seen[n] = true
	}
	for n := range newByName {
		if !seen[n] {
			allNames = append(allNames, n)
		}
	}
	sort.Strings(allNames)
	for _, n := range allNames {
		o, hadOld := oldByName[n]
		w, hadNew := newByName[n]
		switch {
		case hadOld && !hadNew:
			fmt.Printf("  «%s» ИСЧЕЗ (был item_id %d у %s)\n", n, o.ItemID, o.RootName)
		case !hadOld && hadNew:
			fmt.Printf("  «%s» ПОЯВИЛСЯ (item_id %d у %s)\n", n, w.ItemID, w.RootName)
		case o.ItemID != w.ItemID:
			fmt.Printf("  «%s» item_id СМЕНИЛСЯ %d → %d   %s → %s\n",
				n, o.ItemID, w.ItemID, o.RootName, w.RootName)
		case o.RootID != w.RootID || o.Flag != w.Flag || o.ParentItemID != w.ParentItemID:
			fmt.Printf("  «%s» item_id СОХРАНЁН (%d), переехал: %s flag=%s → %s flag=%s\n",
				n, o.ItemID, o.RootName, o.Flag, w.RootName, w.Flag)
		default:
			fmt.Printf("  «%s» без изменений (item_id %d)\n", n, o.ItemID)
		}
	}

	fmt.Println("\n── СТРОКИ ──")
	var appeared, vanished, moved, requantified int
	for id, w := range newByID {
		o, ok := oldByID[id]
		if !ok {
			appeared++
			fmt.Printf("  + %-36s %10d  item_id %d  %s flag=%s\n", w.TypeName, w.Quantity, id, w.RootName, w.Flag)
			continue
		}
		if o.RootID != w.RootID || o.ParentItemID != w.ParentItemID || o.Flag != w.Flag {
			moved++
			fmt.Printf("  → %-36s %10d  item_id %d  %s/%s → %s/%s\n",
				w.TypeName, w.Quantity, id, o.RootName, o.Flag, w.RootName, w.Flag)
		}
		if o.Quantity != w.Quantity {
			requantified++
			fmt.Printf("  ≠ %-36s %10d → %-10d item_id %d\n", w.TypeName, o.Quantity, w.Quantity, id)
		}
	}
	for id, o := range oldByID {
		if _, ok := newByID[id]; !ok {
			vanished++
			fmt.Printf("  − %-36s %10d  item_id %d  %s flag=%s\n", o.TypeName, o.Quantity, id, o.RootName, o.Flag)
		}
	}
	fmt.Printf("\nитого: появилось %d, исчезло %d, переехало %d, изменилось количество у %d\n",
		appeared, vanished, moved, requantified)
}

func save(s snapshot) {
	path := filepath.Join(snapDir(), s.Label+".json")
	b, _ := json.MarshalIndent(s, "", "  ")
	if err := os.WriteFile(path, b, 0o644); err != nil {
		log.Fatalf("снимок: %v", err)
	}
	fmt.Printf("снимок сохранён: %s\n\n", path)
}

func load(label string) snapshot {
	b, err := os.ReadFile(filepath.Join(snapDir(), label+".json"))
	if err != nil {
		log.Fatalf("снимок %s: %v", label, err)
	}
	var s snapshot
	if err := json.Unmarshal(b, &s); err != nil {
		log.Fatalf("снимок %s: %v", label, err)
	}
	return s
}

// purgeAssetCache drops cached asset pages so the probe always talks to
// ESI. CCP's own hour-long cache still applies — that is the point of
// printing timestamps.
func purgeAssetCache(dbPath string) {
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		log.Printf("кэш: %v", err)
		return
	}
	defer db.Close()
	res, err := db.Exec(`DELETE FROM esi_cache WHERE url LIKE '%/assets/%'`)
	if err != nil {
		log.Printf("кэш: %v", err)
		return
	}
	n, _ := res.RowsAffected()
	fmt.Printf("кэш ассетов очищен: %d строк\n", n)
}
