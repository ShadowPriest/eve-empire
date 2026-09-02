package sde

// Blueprint recipes for the build-cost calculator: what a print makes and
// what it eats. The search index is held in memory — there are under 5000
// manufacturing and reaction recipes in the whole game, and SQLite's LIKE
// is ASCII-only, so a Russian name would never match case-insensitively
// through the database.

import (
	"database/sql"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// Recipe is one blueprint activity that produces something.
type Recipe struct {
	BlueprintID   int64
	BlueprintName string
	ProductID     int64
	ProductName   string
	ProductQty    int64  // units produced per run
	Activity      string // manufacturing | reaction
	Time          int64  // seconds per run
	MaxRuns       int64
}

// MaterialQty is how much of one material a job really consumes.
//
// ПОРЯДОК ВАЖЕН, сверено с клиентом (ARCHITECTURE.md, «Формулы EVE»):
// бонус корпуса идёт ОТДЕЛЬНЫМ множителем после ME, округление одно и в
// самом конце, а пол «не ниже числа прогонов» — самым последним. Именно
// поэтому игра просит 6 618 Robotics там, где арифметика даёт 5 897.
//
// У реакций ME чертежа не действует вовсе и бонуса корпуса на материалы
// нет — расход двигают только риги структуры, которые здесь не учтены.
func MaterialQty(base, runs int64, me int, reaction bool, matMul float64) int64 {
	if reaction {
		return base * runs
	}
	if matMul <= 0 {
		matMul = 1
	}
	v := math.Ceil(float64(base)*float64(runs)*(1-float64(me)/100)*matMul - 1e-9)
	if v < float64(runs) {
		v = float64(runs)
	}
	return int64(v)
}

// IsReaction reports whether material efficiency applies. Reactions
// ignore the blueprint's ME entirely — only structure rigs move their
// material use, so the calculator greys the ME field out for them.
func (r Recipe) IsReaction() bool { return r.Activity == "reaction" }

type recipeIndex struct {
	once      sync.Once
	all       []Recipe
	hay       []string // lowercased "bp-ru bp-en product-ru product-en", parallel to all
	byBP      map[int64]int
	byProduct map[int64]int
}

var recipes recipeIndex

// Recipes returns every manufacturing/reaction recipe, sorted by product
// name. Loaded once; an unavailable database yields nothing.
func (d *DB) Recipes() []Recipe {
	d.loadRecipes()
	return recipes.all
}

// RecipeOf returns the recipe of one blueprint.
func (d *DB) RecipeOf(blueprintID int64) (Recipe, bool) {
	d.loadRecipes()
	i, ok := recipes.byBP[blueprintID]
	if !ok {
		return Recipe{}, false
	}
	return recipes.all[i], true
}

// RecipeForProduct returns the recipe that MAKES the given type — the
// question the "build or buy" walk asks about every material.
func (d *DB) RecipeForProduct(productID int64) (Recipe, bool) {
	d.loadRecipes()
	i, ok := recipes.byProduct[productID]
	if !ok {
		return Recipe{}, false
	}
	return recipes.all[i], true
}

// RecipeMaterials lists what one run of the recipe consumes, in the same
// grouping the item modal uses.
func (d *DB) RecipeMaterials(r Recipe) []BPItem {
	if !d.Available() {
		return nil
	}
	rows, err := d.db.Query(`SELECT material_type_id, quantity FROM bp_materials
		WHERE blueprint_type_id = ? AND activity = ? ORDER BY quantity DESC`,
		r.BlueprintID, r.Activity)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []BPItem
	for rows.Next() {
		var it BPItem
		if rows.Scan(&it.TypeID, &it.Quantity) != nil {
			continue
		}
		it.Name = d.typeName(it.TypeID)
		it.Group = d.materialGroup(it.TypeID)
		out = append(out, it)
	}
	return out
}

// Dogma attributes carrying the industry bonuses of a structure hull.
// All four are MULTIPLIERS, not percentages: Azbel's 0.99 material and
// 0.8 time are the −1 % / −20 % the client shows.
const (
	attrStrEngMat      = 2600 // material multiplier
	attrStrEngCost     = 2601 // job-cost multiplier
	attrStrEngTime     = 2602 // manufacturing time multiplier
	attrStrReactionTim = 2721 // reaction time multiplier (Tatara)
)

// IndustryStructure is one structure hull with its industry bonuses.
// The NPC station is the zero value under key "npc": no bonuses at all.
type IndustryStructure struct {
	Key   string // "npc" or the type id as text
	Name  string
	Mat   float64 // material multiplier
	Time  float64 // manufacturing time multiplier
	Cost  float64 // job-cost multiplier — see the note in the web layer
	RTime float64 // reaction time multiplier
}

// IndustryStructures lists the structures worth offering, NPC station
// first. Everything comes from the SDE: the hull carries the bonuses,
// so nothing here is written by hand.
func (d *DB) IndustryStructures() []IndustryStructure {
	out := []IndustryStructure{{Key: "npc", Name: "НПС-станция / без бонусов", Mat: 1, Time: 1, Cost: 1, RTime: 1}}
	if !d.Available() {
		return out
	}
	rows, err := d.db.Query(`SELECT t.type_id, COALESCE(NULLIF(t.name_ru,''), t.name_en),
			MAX(CASE WHEN a.attribute_id = ? THEN a.value END),
			MAX(CASE WHEN a.attribute_id = ? THEN a.value END),
			MAX(CASE WHEN a.attribute_id = ? THEN a.value END),
			MAX(CASE WHEN a.attribute_id = ? THEN a.value END)
		FROM types t JOIN type_attributes a ON a.type_id = t.type_id
		WHERE a.attribute_id IN (?,?,?,?) AND t.published = 1
		GROUP BY t.type_id ORDER BY t.type_id`,
		attrStrEngMat, attrStrEngCost, attrStrEngTime, attrStrReactionTim,
		attrStrEngMat, attrStrEngCost, attrStrEngTime, attrStrReactionTim)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var (
			id                   int64
			name                 string
			mat, cost, tm, rtime sql.NullFloat64
		)
		if rows.Scan(&id, &name, &mat, &cost, &tm, &rtime) != nil {
			continue
		}
		s := IndustryStructure{
			Key: strconv.FormatInt(id, 10), Name: name,
			Mat: one(mat), Time: one(tm), Cost: one(cost), RTime: one(rtime),
		}
		out = append(out, s)
	}
	return out
}

// one reads a multiplier attribute, defaulting to "no effect".
func one(v sql.NullFloat64) float64 {
	if !v.Valid || v.Float64 <= 0 {
		return 1
	}
	return v.Float64
}

// shipCategory is the SDE category hulls live in. Their types.volume is
// the ASSEMBLED volume; the packaged one a hauler actually moves is not
// in the SDE at all, so the calculator refuses to sum it.
const shipCategory = 6

// ShipTypes reports which of the given types are ship hulls.
func (d *DB) ShipTypes(ids []int64) map[int64]bool {
	out := map[int64]bool{}
	if !d.Available() || len(ids) == 0 {
		return out
	}
	rows, err := d.db.Query(`SELECT t.type_id FROM types t
		JOIN groups g ON g.group_id = t.group_id
		WHERE g.category_id = ? AND t.type_id IN (`+placeholders(len(ids))+`)`,
		append([]any{int64(shipCategory)}, toArgs(ids)...)...)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		if rows.Scan(&id) == nil {
			out[id] = true
		}
	}
	return out
}

// SearchRecipes finds recipes by blueprint OR product name, in Russian or
// English. Ranking puts an exact product hit first — typing "Rifter" must
// land on the Rifter, not on "Rifter Blueprint" variants of other prints.
func (d *DB) SearchRecipes(q string, limit int) []Recipe {
	d.loadRecipes()
	q = strings.ToLower(strings.TrimSpace(q))
	if q == "" {
		return nil
	}

	type scored struct {
		rec   Recipe
		score int
	}
	var hits []scored
	for i, hay := range recipes.hay {
		if !strings.Contains(hay, q) {
			continue
		}
		r := recipes.all[i]
		score := 5
		switch {
		case strings.EqualFold(r.ProductName, q):
			score = 0
		case strings.EqualFold(r.BlueprintName, q):
			score = 1
		case strings.HasPrefix(strings.ToLower(r.ProductName), q):
			score = 2
		case strings.HasPrefix(strings.ToLower(r.BlueprintName), q):
			score = 3
		case strings.Contains(strings.ToLower(r.ProductName), q):
			score = 4
		}
		hits = append(hits, scored{r, score})
	}
	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].score != hits[j].score {
			return hits[i].score < hits[j].score
		}
		if len(hits[i].rec.ProductName) != len(hits[j].rec.ProductName) {
			return len(hits[i].rec.ProductName) < len(hits[j].rec.ProductName)
		}
		return hits[i].rec.ProductName < hits[j].rec.ProductName
	})
	if limit > 0 && len(hits) > limit {
		hits = hits[:limit]
	}
	out := make([]Recipe, len(hits))
	for i, h := range hits {
		out[i] = h.rec
	}
	return out
}

func (d *DB) loadRecipes() {
	recipes.once.Do(func() {
		recipes.byBP = map[int64]int{}
		recipes.byProduct = map[int64]int{}
		if !d.Available() {
			return
		}
		rows, err := d.db.Query(`SELECT p.blueprint_type_id, p.product_type_id, p.quantity, p.activity,
				COALESCE(a.time, 0), COALESCE(b.max_production_limit, 0),
				bt.name_en, bt.name_ru, pt.name_en, pt.name_ru
			FROM bp_products p
			JOIN types bt ON bt.type_id = p.blueprint_type_id
			JOIN types pt ON pt.type_id = p.product_type_id
			LEFT JOIN bp_activities a ON a.blueprint_type_id = p.blueprint_type_id AND a.activity = p.activity
			LEFT JOIN blueprints b ON b.blueprint_type_id = p.blueprint_type_id
			WHERE p.activity IN ('manufacturing','reaction') AND bt.published = 1`)
		if err != nil {
			return
		}
		defer rows.Close()
		// Recipe and haystack are sorted together: the search walks the
		// haystack by index, so the two slices must never drift apart.
		type entry struct {
			rec Recipe
			hay string
		}
		var list []entry
		for rows.Next() {
			var (
				r                    Recipe
				bpEn, bpRu, pEn, pRu sql.NullString
			)
			if rows.Scan(&r.BlueprintID, &r.ProductID, &r.ProductQty, &r.Activity,
				&r.Time, &r.MaxRuns, &bpEn, &bpRu, &pEn, &pRu) != nil {
				continue
			}
			r.BlueprintName = pick(bpRu, bpEn)
			r.ProductName = pick(pRu, pEn)
			// Both languages go into the haystack: the owner types either.
			list = append(list, entry{r, strings.ToLower(
				bpEn.String + " " + bpRu.String + " " + pEn.String + " " + pRu.String)})
		}
		sort.Slice(list, func(i, j int) bool {
			return list[i].rec.ProductName < list[j].rec.ProductName
		})
		recipes.all = make([]Recipe, len(list))
		recipes.hay = make([]string, len(list))
		for i, e := range list {
			recipes.all[i] = e.rec
			recipes.hay[i] = e.hay
			recipes.byBP[e.rec.BlueprintID] = i
			// A few types are made by more than one print (a reaction and
			// a manufacturing job for the same composite). First wins —
			// the list is sorted, so the choice at least stays stable.
			if _, seen := recipes.byProduct[e.rec.ProductID]; !seen {
				recipes.byProduct[e.rec.ProductID] = i
			}
		}
	})
}

func pick(ru, en sql.NullString) string {
	if ru.Valid && ru.String != "" {
		return ru.String
	}
	return en.String
}
