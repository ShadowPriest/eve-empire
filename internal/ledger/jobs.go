package ledger

import (
	"fmt"
	"strconv"
	"time"

	"eve-empire/internal/sde"
	"eve-empire/internal/store"
)

// Производство (ACCOUNTING.md §9.4, этап 3).
//
// ESI не говорит, ЧТО работа съела: у задания есть чертёж, число прогонов
// и фабрика, а список материалов приходится воспроизводить самим — по
// рецепту из SDE, исследованному ME чертежа и бонусу корпуса структуры.
//
// Списание в работу — НЕ затрата, а перенос: себестоимость материалов
// целиком переезжает в продукт и станет затратой только при его продаже.
// Поэтому у документа приход и расход в одной проводке, а `CostFrom="doc"`
// переносит всё списанное на выход.

// wipFlag marks the place a job holds its output in until delivery.
// Materials are already gone from the hangar while the job runs, and the
// product does not exist yet: without such a place the ledger would show
// a shortage for the whole duration (§2, НЗП).
const wipFlag = "WIP"

// esiPrice keeps only what the ledger needs, so a nil ESI client (tests)
// costs nothing.
type esiPrice struct{ Average float64 }

// Jobs posts manufacturing and reaction jobs.
func (b *Builder) Jobs(sdeDB *sde.DB) (Result, error) {
	var res Result
	if sdeDB == nil || !sdeDB.Available() {
		res.Note = "нет статической базы — состав работ не восстановить"
		return res, nil
	}
	jobs, err := b.Store.Jobs()
	if err != nil {
		return res, err
	}
	if len(jobs) == 0 {
		res.Note = "работ пока не собрано"
		return res, nil
	}
	done, err := b.Store.PostedSrcIDs("esi:job")
	if err != nil {
		return res, err
	}
	delivered, err := b.Store.PostedSrcIDs("esi:job-deliver")
	if err != nil {
		return res, err
	}
	meOf, err := b.Store.BlueprintME()
	if err != nil {
		return res, err
	}
	openedAt, err := b.Store.OpeningTimes()
	if err != nil {
		return res, err
	}
	var opened time.Time
	for _, t := range openedAt {
		if opened.IsZero() || t.Before(opened) {
			opened = t
		}
	}

	// Рыночная оценка продукта нужна разрезу «вклад по переделам»: без неё
	// у производства вход оценён, а выход нет, и передел покажет
	// отрицательный вклад на ровном месте.
	var prices map[int64]esiPrice
	if b.ESI != nil {
		if p, err := b.ESI.MarketPrices(); err == nil {
			prices = make(map[int64]esiPrice, len(p))
			for id, v := range p {
				prices[id] = esiPrice{Average: v.Average}
			}
		}
	}

	muls := map[int64]float64{}
	before, noRecipe := 0, 0

	for _, j := range jobs {
		if j.ActivityID != 1 && j.ActivityID != 9 {
			continue // копирование, исследование и инвент — не этот этап
		}
		key := strconv.FormatInt(j.JobID, 10)

		if !done[key] {
			// Работа, начатая до открытия книг, уже учтена: её материалы
			// в ангаре отсутствовали, когда снимали остатки.
			o, ok := openedAt[j.OwnerID]
			if !ok {
				o = opened
			}
			if !o.IsZero() && j.StartDate.Before(o) {
				before++
				continue
			}
			recipe, ok := sdeDB.RecipeOf(j.BlueprintTypeID)
			if !ok {
				noRecipe++
				continue
			}
			mul, seen := muls[j.FacilityID]
			if !seen {
				mul = b.facilityMul(sdeDB, j.OwnerID, j.FacilityID)
				muls[j.FacilityID] = mul
			}
			r, err := b.postJobStart(j, recipe, sdeDB, meOf[j.BlueprintID], mul, key,
				prices[recipe.ProductID].Average)
			if err != nil {
				return res, fmt.Errorf("работа %d: %w", j.JobID, err)
			}
			res.Documents++
			res.Shortfall += r.Shortfall
		}

		if j.Status == "delivered" && !delivered[key] {
			if err := b.postJobDelivery(j, sdeDB, key); err != nil {
				return res, fmt.Errorf("выдача работы %d: %w", j.JobID, err)
			}
			res.Documents++
		}
	}

	notes := ""
	if before > 0 {
		notes = fmt.Sprintf("%d работ старше инвентаризации пропущено; ", before)
	}
	if noRecipe > 0 {
		notes += fmt.Sprintf("%d без рецепта в SDE; ", noRecipe)
	}
	res.Note = notes + fmt.Sprintf("проведено документов %d", res.Documents)
	return res, nil
}

// postJobStart consumes the materials and parks the output in WIP.
func (b *Builder) postJobStart(j store.JobRow, recipe sde.Recipe, sdeDB *sde.DB,
	me int, matMul float64, key string, unitMkt float64) (store.PostResult, error) {

	at := j.StartDate
	facility := store.PlaceKey{OwnerID: j.OwnerID, LocationID: j.FacilityID, Flag: "Hangar"}
	wip := store.PlaceKey{
		OwnerID: j.OwnerID, LocationID: j.FacilityID, HolderID: j.JobID,
		Flag: wipFlag, Name: "работа " + key,
	}

	var lines []store.Line
	for _, m := range sdeDB.RecipeMaterials(recipe) {
		qty := sde.MaterialQty(m.Quantity, int64(j.Runs), me, recipe.IsReaction(), matMul)
		if qty <= 0 {
			continue
		}
		lines = append(lines, store.Line{
			Place: facility, TypeID: m.TypeID, Qty: -qty, Scope: "location",
		})
	}
	if len(lines) == 0 {
		return store.PostResult{}, fmt.Errorf("рецепт без материалов")
	}
	// Сбор за установку капитализируется в продукт (§8): без него работа
	// выглядела бы бесплатной, а сбор — убытком из ниоткуда.
	out := recipe.ProductQty * int64(j.Runs)
	lines = append(lines, store.Line{
		Place: wip, TypeID: recipe.ProductID,
		Qty:       out,
		CostFrom:  "doc",
		CostExtra: j.Cost,
		MktTotal:  unitMkt * float64(out),
	})

	return b.Store.PostDoc(store.Doc{
		Kind: kindOf(j.ActivityID), OwnerID: j.OwnerID, At: at,
		Src: "esi:job", SrcID: key,
		Note: fmt.Sprintf("%s ×%d, ME %d, бонус корпуса %.2f",
			recipe.ProductName, j.Runs, me, matMul),
	}, lines, nil)
}

// postJobDelivery moves the finished goods out of WIP into the hangar.
func (b *Builder) postJobDelivery(j store.JobRow, sdeDB *sde.DB, key string) error {
	recipe, ok := sdeDB.RecipeOf(j.BlueprintTypeID)
	if !ok {
		return nil
	}
	runs := int64(j.SuccessfulRuns)
	if runs == 0 {
		runs = int64(j.Runs)
	}
	wip := store.PlaceKey{
		OwnerID: j.OwnerID, LocationID: j.FacilityID, HolderID: j.JobID, Flag: wipFlag,
	}
	hangar := store.PlaceKey{OwnerID: j.OwnerID, LocationID: j.FacilityID, Flag: "Hangar"}
	at := j.CompletedDate
	if at.IsZero() {
		at = j.EndDate
	}
	_, err := b.Store.PostDoc(store.Doc{
		Kind: "delivery", OwnerID: j.OwnerID, At: at,
		Src: "esi:job-deliver", SrcID: key,
		Note: fmt.Sprintf("выдача: %s ×%d", recipe.ProductName, runs),
	}, []store.Line{
		{Place: wip, TypeID: recipe.ProductID, Qty: -recipe.ProductQty * runs},
		{Place: hangar, TypeID: recipe.ProductID, Qty: recipe.ProductQty * runs,
			CostFrom: "issue"},
	}, nil)
	return err
}

func kindOf(activity int) string {
	if activity == 9 {
		return "reaction"
	}
	return "manufacture"
}

// facilityMul is the structure's material multiplier from the SDE hull
// attributes. NPC stations give no bonus, and a structure we cannot read
// is treated as giving none — an honest 1.0 rather than a guess.
func (b *Builder) facilityMul(sdeDB *sde.DB, viaChar, facilityID int64) float64 {
	if facilityID < 1_000_000_000_000 {
		return 1
	}
	typeID, err := b.ESI.StructureType(viaChar, facilityID)
	if err != nil || typeID == 0 {
		return 1
	}
	key := strconv.FormatInt(typeID, 10)
	for _, st := range sdeDB.IndustryStructures() {
		if st.Key == key {
			if st.Mat > 0 {
				return st.Mat
			}
			return 1
		}
	}
	return 1
}
