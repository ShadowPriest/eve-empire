package ledger

import (
	"path/filepath"
	"testing"
	"time"

	"eve-empire/internal/sde"
	"eve-empire/internal/store"
)

// Производственная проводка целиком, на настоящем рецепте из SDE:
// материалы считаются по формуле, списываются с полки и всей своей
// себестоимостью оказываются в продукте.
//
// Тест пропускается без sde.db — база в git не лежит (250 МБ).
func TestJobConsumesAndProduces(t *testing.T) {
	sdeDB := sde.Open("../../sde.db")
	defer sdeDB.Close()
	if !sdeDB.Available() {
		t.Skip("нет sde.db — рецепты недоступны")
	}

	const (
		owner    = int64(1001)
		facility = int64(60003760)
		bpType   = int64(881) // чертёж, с которого идут живые работы
		bpItem   = int64(9001)
		jobID    = int64(555001)
		runs     = 4
		fee      = 138.0
	)
	recipe, ok := sdeDB.RecipeOf(bpType)
	if !ok {
		t.Skipf("в SDE нет рецепта %d", bpType)
	}
	mats := sdeDB.RecipeMaterials(recipe)
	if len(mats) == 0 {
		t.Skip("рецепт без материалов")
	}

	key := make([]byte, 32)
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"), key)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	opened := time.Now().Add(-2 * time.Hour)
	hangar := store.PlaceKey{OwnerID: owner, LocationID: facility, Flag: "Hangar"}

	// Открываем книги и кладём на полку с запасом, по 10 ISK за единицу,
	// чтобы ожидаемую себестоимость можно было посчитать в уме.
	const unit = 10.0
	var lines []store.Line
	var expect float64
	for _, m := range mats {
		need := sde.MaterialQty(m.Quantity, runs, 0, recipe.IsReaction(), 1)
		lines = append(lines, store.Line{
			Place: hangar, TypeID: m.TypeID, Qty: need * 2,
			CostTotal: float64(need*2) * unit,
		})
		expect += float64(need) * unit
	}
	if _, err := st.PostDoc(store.Doc{
		Kind: "opening", OwnerID: owner, At: opened, Src: "opening",
		SrcID: "1001",
	}, lines, nil); err != nil {
		t.Fatal(err)
	}

	if _, err := st.SaveBlueprints([]store.BlueprintRow{{
		OwnerID: owner, ItemID: bpItem, TypeID: bpType, Quantity: -1, Runs: -1,
	}}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := st.SaveJobs([]store.JobRow{{
		OwnerID: owner, JobID: jobID, ActivityID: 1, BlueprintID: bpItem,
		BlueprintTypeID: bpType, ProductTypeID: recipe.ProductID, Runs: runs,
		Cost: fee, Status: "active", FacilityID: facility,
		StartDate: opened.Add(time.Hour),
	}}, time.Now()); err != nil {
		t.Fatal(err)
	}

	res, err := New(st, nil).Jobs(sdeDB)
	if err != nil {
		t.Fatalf("проводка работ: %v", err)
	}
	if res.Documents != 1 {
		t.Fatalf("документов %d, ожидался 1 (%s)", res.Documents, res.Note)
	}
	if res.Shortfall != 0 {
		t.Errorf("нехватка %d, а материалов клали вдвое больше нужного", res.Shortfall)
	}

	got, err := st.Production(opened.Add(-time.Hour), time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("в отчёте производства %d строк, ожидалась 1", len(got))
	}
	p := got[0]
	if p.TypeID != recipe.ProductID {
		t.Errorf("продукт %d, ожидался %d", p.TypeID, recipe.ProductID)
	}
	if want := recipe.ProductQty * runs; p.Quantity != want {
		t.Errorf("выпущено %d, ожидалось %d", p.Quantity, want)
	}
	if !p.InWIP {
		t.Error("работа не выдана, продукт обязан лежать в НЗП")
	}
	// Себестоимость продукта = материалы по факту + сбор за установку.
	if d := p.Cost - (expect + fee); d > 0.01 || d < -0.01 {
		t.Errorf("себестоимость продукта %.2f, ожидалось %.2f", p.Cost, expect+fee)
	}
}
