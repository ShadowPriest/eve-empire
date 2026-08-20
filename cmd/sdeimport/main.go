// Command sdeimport builds a local static-data database (sde.db) from
// CCP's official JSONL Static Data Export plus 64px type icons from the
// image CDN. One-shot and resumable: re-running skips a finished SDE
// build and only fetches missing icons.
//
// Tables: meta, categories, groups, market_groups, types (localized
// en/ru), type_attributes (full dogma), type_skills (derived skill
// requirements), dogma_attributes, blueprints, bp_activities,
// bp_materials, bp_products, bp_skills, icons (png BLOB, NULL = no icon).
package main

import (
	"archive/zip"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

const (
	sdeBase  = "https://developers.eveonline.com/static-data/tranquility"
	imgBase  = "https://images.evetech.net"
	iconSize = 64
)

var workers = 48

// ua identifies the importer to CCP. ESI asks every client for a working
// contact, so put yours in ESI_USER_AGENT before a long run — the default
// is deliberately useless as a contact.
var ua = esiUserAgent()

func esiUserAgent() string {
	if v := os.Getenv("ESI_USER_AGENT"); v != "" {
		return v
	}
	return "eve-empire-sde-import (set ESI_USER_AGENT to your contact)"
}

// Skill-requirement dogma attribute pairs: requiredSkillN / its level.
var skillAttrPairs = [][2]int64{
	{182, 277}, {183, 278}, {184, 279}, {1285, 1286}, {1289, 1287}, {1290, 1288},
}

var httpc = &http.Client{Timeout: 10 * time.Minute}

func main() {
	dbPath := flag.String("db", "sde.db", "output database path")
	skipIcons := flag.Bool("no-icons", false, "skip icon download")
	redata := flag.Bool("redata", false, "re-import SDE data even if the build matches")
	flag.IntVar(&workers, "workers", workers, "parallel icon downloads")
	flag.Parse()
	log.SetFlags(log.Ltime)

	db, err := sql.Open("sqlite", *dbPath+"?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)")
	if err != nil {
		log.Fatalf("open db: %v", err)
	}
	defer db.Close()
	if err := migrate(db); err != nil {
		log.Fatalf("migrate: %v", err)
	}

	build, release := latestBuild()
	log.Printf("актуальный SDE: build %d (%s)", build, release)

	var have string
	_ = db.QueryRow(`SELECT value FROM meta WHERE key='build'`).Scan(&have)
	if have == fmt.Sprint(build) && !*redata {
		log.Printf("SDE build %d уже импортирован — пропускаю данные", build)
	} else {
		zipPath := filepath.Join(os.TempDir(), fmt.Sprintf("eve-sde-%d.zip", build))
		if err := download(fmt.Sprintf("%s/eve-online-static-data-%d-jsonl.zip", sdeBase, build), zipPath); err != nil {
			log.Fatalf("download sde: %v", err)
		}
		if err := importSDE(db, zipPath, build, release); err != nil {
			log.Fatalf("import sde: %v", err)
		}
		os.Remove(zipPath)
	}

	if !*skipIcons {
		if err := fetchIcons(db); err != nil {
			log.Fatalf("icons: %v", err)
		}
		if err := fetchBPCIcons(db); err != nil {
			log.Fatalf("bpc icons: %v", err)
		}
	}

	report(db, *dbPath)
}

func migrate(db *sql.DB) error {
	_, err := db.Exec(`
CREATE TABLE IF NOT EXISTS meta (key TEXT PRIMARY KEY, value TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS categories (
    category_id INTEGER PRIMARY KEY, name_en TEXT, name_ru TEXT, published INTEGER);
CREATE TABLE IF NOT EXISTS groups (
    group_id INTEGER PRIMARY KEY, category_id INTEGER, name_en TEXT, name_ru TEXT, published INTEGER);
CREATE TABLE IF NOT EXISTS market_groups (
    market_group_id INTEGER PRIMARY KEY, parent_id INTEGER, name_en TEXT, name_ru TEXT, icon_id INTEGER);
CREATE TABLE IF NOT EXISTS types (
    type_id INTEGER PRIMARY KEY, group_id INTEGER, market_group_id INTEGER,
    name_en TEXT, name_ru TEXT, description_en TEXT, description_ru TEXT,
    published INTEGER, volume REAL, mass REAL, capacity REAL,
    portion_size INTEGER, base_price REAL, icon_id INTEGER, meta_group_id INTEGER,
    variation_parent_id INTEGER);
CREATE INDEX IF NOT EXISTS idx_types_group ON types(group_id);
-- reprocessing output: what one portion_size batch of a type refines into
CREATE TABLE IF NOT EXISTS type_materials (
    type_id INTEGER, material_type_id INTEGER, quantity INTEGER,
    PRIMARY KEY (type_id, material_type_id)) WITHOUT ROWID;
-- erratic ores (Prismaticite & co) refine into a random amount instead
CREATE TABLE IF NOT EXISTS type_materials_rand (
    type_id INTEGER, material_type_id INTEGER, qty_min INTEGER, qty_max INTEGER,
    PRIMARY KEY (type_id, material_type_id)) WITHOUT ROWID;
-- raw type -> the type it compresses into
CREATE TABLE IF NOT EXISTS compressible (
    type_id INTEGER PRIMARY KEY, compressed_type_id INTEGER);
CREATE TABLE IF NOT EXISTS type_attributes (
    type_id INTEGER, attribute_id INTEGER, value REAL,
    PRIMARY KEY (type_id, attribute_id)) WITHOUT ROWID;
CREATE TABLE IF NOT EXISTS type_skills (
    type_id INTEGER, skill_type_id INTEGER, level INTEGER,
    PRIMARY KEY (type_id, skill_type_id)) WITHOUT ROWID;
CREATE TABLE IF NOT EXISTS dogma_attributes (
    attribute_id INTEGER PRIMARY KEY, name TEXT, display_name_en TEXT, display_name_ru TEXT,
    unit_id INTEGER, published INTEGER DEFAULT 0, icon_id INTEGER DEFAULT 0);
CREATE TABLE IF NOT EXISTS blueprints (
    blueprint_type_id INTEGER PRIMARY KEY, max_production_limit INTEGER);
CREATE TABLE IF NOT EXISTS bp_activities (
    blueprint_type_id INTEGER, activity TEXT, time INTEGER,
    PRIMARY KEY (blueprint_type_id, activity)) WITHOUT ROWID;
CREATE TABLE IF NOT EXISTS bp_materials (
    blueprint_type_id INTEGER, activity TEXT, material_type_id INTEGER, quantity INTEGER,
    PRIMARY KEY (blueprint_type_id, activity, material_type_id)) WITHOUT ROWID;
CREATE TABLE IF NOT EXISTS bp_products (
    blueprint_type_id INTEGER, activity TEXT, product_type_id INTEGER, quantity INTEGER, probability REAL,
    PRIMARY KEY (blueprint_type_id, activity, product_type_id)) WITHOUT ROWID;
CREATE TABLE IF NOT EXISTS bp_skills (
    blueprint_type_id INTEGER, activity TEXT, skill_type_id INTEGER, level INTEGER,
    PRIMARY KEY (blueprint_type_id, activity, skill_type_id)) WITHOUT ROWID;
CREATE TABLE IF NOT EXISTS pi_schematics (
    schematic_id INTEGER PRIMARY KEY, name_en TEXT, name_ru TEXT, cycle_time INTEGER);
CREATE TABLE IF NOT EXISTS pi_schematic_types (
    schematic_id INTEGER, type_id INTEGER, is_input INTEGER, quantity INTEGER,
    PRIMARY KEY (schematic_id, type_id, is_input)) WITHOUT ROWID;
CREATE INDEX IF NOT EXISTS idx_pi_types ON pi_schematic_types(type_id);
CREATE TABLE IF NOT EXISTS meta_groups (
    meta_group_id INTEGER PRIMARY KEY, name_en TEXT, name_ru TEXT);
CREATE TABLE IF NOT EXISTS icons (type_id INTEGER PRIMARY KEY, png BLOB);
-- blueprint copies have their own artwork on the CDN ("bpc" variant)
CREATE TABLE IF NOT EXISTS icons_bpc (type_id INTEGER PRIMARY KEY, png BLOB)`)
	if err != nil {
		return err
	}
	// Databases built before the ore table need the new types column;
	// "duplicate column" on a fresh build is the expected no-op.
	if _, err := db.Exec(`ALTER TABLE types ADD COLUMN variation_parent_id INTEGER`); err != nil &&
		!strings.Contains(err.Error(), "duplicate column") {
		return err
	}
	return nil
}

func latestBuild() (int64, string) {
	req, _ := http.NewRequest("GET", sdeBase+"/latest.jsonl", nil)
	req.Header.Set("User-Agent", ua)
	resp, err := httpc.Do(req)
	if err != nil {
		log.Fatalf("latest.jsonl: %v", err)
	}
	defer resp.Body.Close()
	var m struct {
		BuildNumber int64  `json:"buildNumber"`
		ReleaseDate string `json:"releaseDate"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil || m.BuildNumber == 0 {
		log.Fatalf("latest.jsonl: bad manifest (%v)", err)
	}
	return m.BuildNumber, m.ReleaseDate
}

func download(url, dst string) error {
	if st, err := os.Stat(dst); err == nil && st.Size() > 0 {
		log.Printf("архив уже скачан: %s (%.1f МБ)", dst, float64(st.Size())/1e6)
		return nil
	}
	log.Printf("качаю %s ...", url)
	req, _ := http.NewRequest("GET", url, nil)
	req.Header.Set("User-Agent", ua)
	resp, err := httpc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("%s: %s", url, resp.Status)
	}
	f, err := os.Create(dst + ".part")
	if err != nil {
		return err
	}
	n, err := io.Copy(f, resp.Body)
	f.Close()
	if err != nil {
		return err
	}
	log.Printf("скачано %.1f МБ", float64(n)/1e6)
	return os.Rename(dst+".part", dst)
}

// loc is the multi-language string in the new SDE format.
type loc map[string]string

func (l loc) en() string { return l["en"] }
func (l loc) ru() string {
	if v, ok := l["ru"]; ok {
		return v
	}
	return l["en"]
}

func importSDE(db *sql.DB, zipPath string, build int64, release string) error {
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return err
	}
	defer zr.Close()

	files := map[string]*zip.File{}
	for _, f := range zr.File {
		files[strings.ToLower(filepath.Base(f.Name))] = f
	}
	var names []string
	for n := range files {
		names = append(names, n)
	}
	log.Printf("в архиве %d файлов", len(files))

	// each() streams a JSONL file line-object by line-object.
	each := func(name string, fn func(raw json.RawMessage)) error {
		f, ok := files[name]
		if !ok {
			log.Printf("ВНИМАНИЕ: %s нет в архиве — пропускаю", name)
			return nil
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		defer rc.Close()
		dec := json.NewDecoder(rc)
		n := 0
		for {
			var raw json.RawMessage
			if err := dec.Decode(&raw); err == io.EOF {
				break
			} else if err != nil {
				return fmt.Errorf("%s: %w", name, err)
			}
			fn(raw)
			n++
		}
		log.Printf("%s: %d записей", name, n)
		return nil
	}

	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// ── categories / groups / market groups ──
	type keyed struct {
		Key        int64 `json:"_key"`
		CategoryID int64 `json:"categoryID"`
		ParentID   int64 `json:"parentGroupID"`
		IconID     int64 `json:"iconID"`
		Published  bool  `json:"published"`
		Name       loc   `json:"name"`
	}
	catStmt, _ := tx.Prepare(`INSERT OR REPLACE INTO categories VALUES (?,?,?,?)`)
	if err := each("categories.jsonl", func(raw json.RawMessage) {
		var v keyed
		if json.Unmarshal(raw, &v) == nil {
			catStmt.Exec(v.Key, v.Name.en(), v.Name.ru(), v.Published)
		}
	}); err != nil {
		return err
	}
	grpStmt, _ := tx.Prepare(`INSERT OR REPLACE INTO groups VALUES (?,?,?,?,?)`)
	if err := each("groups.jsonl", func(raw json.RawMessage) {
		var v keyed
		if json.Unmarshal(raw, &v) == nil {
			grpStmt.Exec(v.Key, v.CategoryID, v.Name.en(), v.Name.ru(), v.Published)
		}
	}); err != nil {
		return err
	}
	mgStmt, _ := tx.Prepare(`INSERT OR REPLACE INTO market_groups VALUES (?,?,?,?,?)`)
	if err := each("marketgroups.jsonl", func(raw json.RawMessage) {
		var v keyed
		if json.Unmarshal(raw, &v) == nil {
			mgStmt.Exec(v.Key, v.ParentID, v.Name.en(), v.Name.ru(), v.IconID)
		}
	}); err != nil {
		return err
	}

	mgrStmt, _ := tx.Prepare(`INSERT OR REPLACE INTO meta_groups VALUES (?,?,?)`)
	if err := each("metagroups.jsonl", func(raw json.RawMessage) {
		var v struct {
			Key  int64 `json:"_key"`
			Name loc   `json:"name"`
		}
		if json.Unmarshal(raw, &v) == nil {
			mgrStmt.Exec(v.Key, v.Name.en(), v.Name.ru())
		}
	}); err != nil {
		return err
	}

	// ── types ──
	type sdeType struct {
		Key           int64   `json:"_key"`
		GroupID       int64   `json:"groupID"`
		MarketGroupID int64   `json:"marketGroupID"`
		Name          loc     `json:"name"`
		Description   loc     `json:"description"`
		Published     bool    `json:"published"`
		Volume        float64 `json:"volume"`
		Mass          float64 `json:"mass"`
		Capacity      float64 `json:"capacity"`
		PortionSize   int64   `json:"portionSize"`
		BasePrice     float64 `json:"basePrice"`
		IconID        int64   `json:"iconID"`
		MetaGroupID   int64   `json:"metaGroupID"`
		VariationOf   int64   `json:"variationParentTypeID"`
	}
	typeStmt, _ := tx.Prepare(`INSERT OR REPLACE INTO types (
	    type_id, group_id, market_group_id, name_en, name_ru, description_en, description_ru,
	    published, volume, mass, capacity, portion_size, base_price, icon_id, meta_group_id,
	    variation_parent_id) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`)
	if err := each("types.jsonl", func(raw json.RawMessage) {
		var v sdeType
		if json.Unmarshal(raw, &v) == nil {
			typeStmt.Exec(v.Key, v.GroupID, v.MarketGroupID,
				v.Name.en(), v.Name.ru(), v.Description.en(), v.Description.ru(),
				v.Published, v.Volume, v.Mass, v.Capacity,
				v.PortionSize, v.BasePrice, v.IconID, v.MetaGroupID, v.VariationOf)
		}
	}); err != nil {
		return err
	}

	// ── reprocessing output and compression targets ──
	// The ore table lives on these two: type_materials says what a batch
	// of portion_size units refines into, compressible links raw ore to
	// its compressed twin.
	tmStmt, _ := tx.Prepare(`INSERT OR REPLACE INTO type_materials VALUES (?,?,?)`)
	trStmt, _ := tx.Prepare(`INSERT OR REPLACE INTO type_materials_rand VALUES (?,?,?,?)`)
	if err := each("typematerials.jsonl", func(raw json.RawMessage) {
		var v struct {
			Key       int64 `json:"_key"`
			Materials []struct {
				TypeID   int64 `json:"materialTypeID"`
				Quantity int64 `json:"quantity"`
			} `json:"materials"`
			Randomized []struct {
				TypeID int64 `json:"materialTypeID"`
				Min    int64 `json:"quantityMin"`
				Max    int64 `json:"quantityMax"`
			} `json:"randomizedMaterials"`
		}
		if json.Unmarshal(raw, &v) != nil {
			return
		}
		for _, m := range v.Materials {
			tmStmt.Exec(v.Key, m.TypeID, m.Quantity)
		}
		for _, m := range v.Randomized {
			trStmt.Exec(v.Key, m.TypeID, m.Min, m.Max)
		}
	}); err != nil {
		return err
	}
	cmpStmt, _ := tx.Prepare(`INSERT OR REPLACE INTO compressible VALUES (?,?)`)
	if err := each("compressibletypes.jsonl", func(raw json.RawMessage) {
		var v struct {
			Key        int64 `json:"_key"`
			Compressed int64 `json:"compressedTypeID"`
		}
		if json.Unmarshal(raw, &v) == nil {
			cmpStmt.Exec(v.Key, v.Compressed)
		}
	}); err != nil {
		return err
	}

	// ── dogma attribute names ──
	daStmt, _ := tx.Prepare(`INSERT OR REPLACE INTO dogma_attributes VALUES (?,?,?,?,?,?,?)`)
	if err := each("dogmaattributes.jsonl", func(raw json.RawMessage) {
		var v struct {
			Key         int64  `json:"_key"`
			Name        string `json:"name"`
			DisplayName loc    `json:"displayName"`
			UnitID      int64  `json:"unitID"`
			Published   bool   `json:"published"`
			IconID      int64  `json:"iconID"`
		}
		if json.Unmarshal(raw, &v) == nil {
			daStmt.Exec(v.Key, v.Name, v.DisplayName.en(), v.DisplayName.ru(), v.UnitID, v.Published, v.IconID)
		}
	}); err != nil {
		return err
	}

	// ── per-type dogma: full attributes + derived skill requirements ──
	taStmt, _ := tx.Prepare(`INSERT OR REPLACE INTO type_attributes VALUES (?,?,?)`)
	tsStmt, _ := tx.Prepare(`INSERT OR REPLACE INTO type_skills VALUES (?,?,?)`)
	if err := each("typedogma.jsonl", func(raw json.RawMessage) {
		var v struct {
			Key   int64 `json:"_key"`
			Attrs []struct {
				AttributeID int64   `json:"attributeID"`
				Value       float64 `json:"value"`
			} `json:"dogmaAttributes"`
		}
		if json.Unmarshal(raw, &v) != nil {
			return
		}
		byID := map[int64]float64{}
		for _, a := range v.Attrs {
			taStmt.Exec(v.Key, a.AttributeID, a.Value)
			byID[a.AttributeID] = a.Value
		}
		for _, p := range skillAttrPairs {
			if sk, ok := byID[p[0]]; ok && sk > 0 {
				lvl := byID[p[1]]
				tsStmt.Exec(v.Key, int64(sk), int64(lvl))
			}
		}
	}); err != nil {
		return err
	}

	// ── blueprints ──
	type bpItem struct {
		TypeID      int64   `json:"typeID"`
		Quantity    int64   `json:"quantity"`
		Level       int64   `json:"level"`
		Probability float64 `json:"probability"`
	}
	type bpActivity struct {
		Time      int64    `json:"time"`
		Materials []bpItem `json:"materials"`
		Products  []bpItem `json:"products"`
		Skills    []bpItem `json:"skills"`
	}
	bpStmt, _ := tx.Prepare(`INSERT OR REPLACE INTO blueprints VALUES (?,?)`)
	baStmt, _ := tx.Prepare(`INSERT OR REPLACE INTO bp_activities VALUES (?,?,?)`)
	bmStmt, _ := tx.Prepare(`INSERT OR REPLACE INTO bp_materials VALUES (?,?,?,?)`)
	bpdStmt, _ := tx.Prepare(`INSERT OR REPLACE INTO bp_products VALUES (?,?,?,?,?)`)
	bsStmt, _ := tx.Prepare(`INSERT OR REPLACE INTO bp_skills VALUES (?,?,?,?)`)
	if err := each("blueprints.jsonl", func(raw json.RawMessage) {
		var v struct {
			Key                int64                 `json:"_key"`
			BlueprintTypeID    int64                 `json:"blueprintTypeID"`
			MaxProductionLimit int64                 `json:"maxProductionLimit"`
			Activities         map[string]bpActivity `json:"activities"`
		}
		if json.Unmarshal(raw, &v) != nil {
			return
		}
		id := v.Key
		if id == 0 {
			id = v.BlueprintTypeID
		}
		bpStmt.Exec(id, v.MaxProductionLimit)
		for act, a := range v.Activities {
			baStmt.Exec(id, act, a.Time)
			for _, m := range a.Materials {
				bmStmt.Exec(id, act, m.TypeID, m.Quantity)
			}
			for _, p := range a.Products {
				bpdStmt.Exec(id, act, p.TypeID, p.Quantity, p.Probability)
			}
			for _, s := range a.Skills {
				lvl := s.Level
				if lvl == 0 {
					lvl = s.Quantity // некоторые дампы кладут уровень в quantity
				}
				bsStmt.Exec(id, act, s.TypeID, lvl)
			}
		}
	}); err != nil {
		return err
	}

	// ── planetary industry schematics ──
	psStmt, _ := tx.Prepare(`INSERT OR REPLACE INTO pi_schematics VALUES (?,?,?,?)`)
	ptStmt, _ := tx.Prepare(`INSERT OR REPLACE INTO pi_schematic_types VALUES (?,?,?,?)`)
	if err := each("planetschematics.jsonl", func(raw json.RawMessage) {
		var v struct {
			Key       int64 `json:"_key"`
			CycleTime int64 `json:"cycleTime"`
			Name      loc   `json:"name"`
			Types     []struct {
				TypeID   int64 `json:"_key"`
				IsInput  bool  `json:"isInput"`
				Quantity int64 `json:"quantity"`
			} `json:"types"`
		}
		if json.Unmarshal(raw, &v) != nil {
			return
		}
		psStmt.Exec(v.Key, v.Name.en(), v.Name.ru(), v.CycleTime)
		for _, t := range v.Types {
			ptStmt.Exec(v.Key, t.TypeID, t.IsInput, t.Quantity)
		}
	}); err != nil {
		return err
	}

	tx.Exec(`INSERT OR REPLACE INTO meta VALUES ('build', ?), ('release_date', ?), ('imported_at', ?)`,
		fmt.Sprint(build), release, time.Now().Format(time.RFC3339))
	return tx.Commit()
}

// fetchIcons downloads 64px icons for every published type that has no
// icons row yet. A 404 is stored as NULL so it is not retried.
func fetchIcons(db *sql.DB) error {
	rows, err := db.Query(`SELECT t.type_id FROM types t
		LEFT JOIN icons i ON i.type_id = t.type_id
		WHERE t.published = 1 AND i.type_id IS NULL ORDER BY t.type_id`)
	if err != nil {
		return err
	}
	var todo []int64
	for rows.Next() {
		var id int64
		rows.Scan(&id)
		todo = append(todo, id)
	}
	rows.Close()
	if len(todo) == 0 {
		log.Printf("иконки: всё уже скачано")
		return nil
	}
	log.Printf("иконки: осталось скачать %d", len(todo))

	client := &http.Client{Timeout: 20 * time.Second}
	type result struct {
		id  int64
		png []byte // nil = 404
	}
	done := 0
	const batch = 2000
	for start := 0; start < len(todo); start += batch {
		end := start + batch
		if end > len(todo) {
			end = len(todo)
		}
		batch := todo[start:end]
		results := make([]result, len(batch))
		var wg sync.WaitGroup
		sem := make(chan struct{}, workers)
		for i, id := range batch {
			wg.Add(1)
			go func(i int, id int64) {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				// Blueprints have no "icon" variant — they serve "bp"
				// (and "bpc" for copies); render is the ship-model image.
				for _, variant := range []string{"icon", "bp", "render"} {
					url := fmt.Sprintf("%s/types/%d/%s?size=%d", imgBase, id, variant, iconSize)
					fetched := false
					for attempt := 0; attempt < 3; attempt++ {
						req, _ := http.NewRequest("GET", url, nil)
						req.Header.Set("User-Agent", ua)
						resp, err := client.Do(req)
						if err != nil {
							time.Sleep(time.Second << attempt)
							continue
						}
						body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
						resp.Body.Close()
						switch {
						case resp.StatusCode == 200:
							results[i] = result{id, body}
							return
						case resp.StatusCode == 400 || resp.StatusCode == 404:
							fetched = true // variant absent — try the next one
						default:
							time.Sleep(time.Second << attempt)
							continue
						}
						break
					}
					_ = fetched
				}
				results[i] = result{id, nil}
			}(i, id)
		}
		wg.Wait()

		tx, err := db.Begin()
		if err != nil {
			return err
		}
		st, _ := tx.Prepare(`INSERT OR REPLACE INTO icons VALUES (?,?)`)
		for _, r := range results {
			st.Exec(r.id, r.png)
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		done += len(batch)
		log.Printf("иконки: %d/%d", done, len(todo))
	}
	return nil
}

// fetchBPCIcons downloads the "blueprint copy" artwork for every
// blueprint type, so the UI can tell an original from a copy.
func fetchBPCIcons(db *sql.DB) error {
	rows, err := db.Query(`SELECT b.blueprint_type_id FROM blueprints b
		LEFT JOIN icons_bpc i ON i.type_id = b.blueprint_type_id
		WHERE i.type_id IS NULL ORDER BY b.blueprint_type_id`)
	if err != nil {
		return err
	}
	var todo []int64
	for rows.Next() {
		var id int64
		rows.Scan(&id)
		todo = append(todo, id)
	}
	rows.Close()
	if len(todo) == 0 {
		log.Printf("иконки копий: всё уже скачано")
		return nil
	}
	log.Printf("иконки копий (bpc): осталось скачать %d", len(todo))

	client := &http.Client{Timeout: 20 * time.Second}
	type result struct {
		id  int64
		png []byte
	}
	done := 0
	const batch = 2000
	for start := 0; start < len(todo); start += batch {
		end := start + batch
		if end > len(todo) {
			end = len(todo)
		}
		chunk := todo[start:end]
		results := make([]result, len(chunk))
		var wg sync.WaitGroup
		sem := make(chan struct{}, workers)
		for i, id := range chunk {
			wg.Add(1)
			go func(i int, id int64) {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				url := fmt.Sprintf("%s/types/%d/bpc?size=%d", imgBase, id, iconSize)
				for attempt := 0; attempt < 3; attempt++ {
					req, _ := http.NewRequest("GET", url, nil)
					req.Header.Set("User-Agent", ua)
					resp, err := client.Do(req)
					if err != nil {
						time.Sleep(time.Second << attempt)
						continue
					}
					body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
					resp.Body.Close()
					if resp.StatusCode == 200 {
						results[i] = result{id, body}
						return
					}
					if resp.StatusCode == 400 || resp.StatusCode == 404 {
						break
					}
					time.Sleep(time.Second << attempt)
				}
				results[i] = result{id, nil}
			}(i, id)
		}
		wg.Wait()

		tx, err := db.Begin()
		if err != nil {
			return err
		}
		st, _ := tx.Prepare(`INSERT OR REPLACE INTO icons_bpc VALUES (?,?)`)
		for _, r := range results {
			st.Exec(r.id, r.png)
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		done += len(chunk)
		log.Printf("иконки копий: %d/%d", done, len(todo))
	}
	return nil
}

func report(db *sql.DB, dbPath string) {
	count := func(q string) (n int64) { db.QueryRow(q).Scan(&n); return }
	db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`)
	var size int64
	if st, err := os.Stat(dbPath); err == nil {
		size = st.Size()
	}
	log.Printf("── ИТОГ ──────────────────────────────")
	log.Printf("типов:            %d (published %d)", count(`SELECT COUNT(*) FROM types`), count(`SELECT COUNT(*) FROM types WHERE published=1`))
	log.Printf("групп/категорий:  %d / %d", count(`SELECT COUNT(*) FROM groups`), count(`SELECT COUNT(*) FROM categories`))
	log.Printf("dogma-атрибутов:  %d строк", count(`SELECT COUNT(*) FROM type_attributes`))
	log.Printf("требований навыков: %d строк", count(`SELECT COUNT(*) FROM type_skills`))
	log.Printf("чертежей:         %d (материалов %d)", count(`SELECT COUNT(*) FROM blueprints`), count(`SELECT COUNT(*) FROM bp_materials`))
	log.Printf("переработка:      %d строк (%d типов, случайных %d)",
		count(`SELECT COUNT(*) FROM type_materials`),
		count(`SELECT COUNT(DISTINCT type_id) FROM type_materials`),
		count(`SELECT COUNT(*) FROM type_materials_rand`))
	log.Printf("сжимаемых типов:  %d", count(`SELECT COUNT(*) FROM compressible`))
	log.Printf("иконок:           %d (с картинкой %d)", count(`SELECT COUNT(*) FROM icons`), count(`SELECT COUNT(*) FROM icons WHERE png IS NOT NULL`))
	log.Printf("иконок копий bpc: %d", count(`SELECT COUNT(*) FROM icons_bpc WHERE png IS NOT NULL`))
	log.Printf("размер БД:        %.1f МБ (%s)", float64(size)/1e6, dbPath)
}
