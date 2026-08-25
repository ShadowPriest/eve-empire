package store

import (
	"math"
	"path/filepath"
	"testing"
	"time"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	key := make([]byte, 32)
	s, err := Open(filepath.Join(t.TempDir(), "t.db"), key)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

const owner = int64(1001)
const station = int64(60003760)
const strontium = int64(16275)

func hangar() PlaceKey {
	return PlaceKey{OwnerID: owner, LocationID: station, Flag: "Hangar"}
}

// buy posts one purchase lot n days before "now".
func buy(t *testing.T, s *Store, id string, qty int64, unit float64, daysAgo int) {
	t.Helper()
	at := time.Now().AddDate(0, 0, -daysAgo)
	_, err := s.PostDoc(
		Doc{Kind: "purchase", OwnerID: owner, At: at, Src: "test", SrcID: id},
		[]Line{{Place: hangar(), TypeID: strontium, Qty: qty, CostTotal: float64(qty) * unit}},
		nil)
	if err != nil {
		t.Fatalf("покупка %s: %v", id, err)
	}
}

func near(a, b float64) bool { return math.Abs(a-b) < 0.01 }

// TestFIFOWorkedExample is the example from ACCOUNTING.md §3 checked
// against the code: three strontium lots, 600 000 units into a job.
func TestFIFOWorkedExample(t *testing.T) {
	s := testStore(t)
	buy(t, s, "A", 300_000, 2100, 9) // 630 млн
	buy(t, s, "B", 200_000, 3400, 6) // 680 млн
	buy(t, s, "C", 400_000, 2400, 3) // 960 млн

	res, err := s.PostDoc(
		Doc{Kind: "manufacture", OwnerID: owner, At: time.Now(), Src: "test", SrcID: "job1"},
		[]Line{{Place: hangar(), TypeID: strontium, Qty: -600_000}},
		nil)
	if err != nil {
		t.Fatalf("работа: %v", err)
	}
	if res.Shortfall != 0 {
		t.Fatalf("нехватка %d, а партий хватало", res.Shortfall)
	}

	var cogs float64
	if err := s.db.QueryRow(`SELECT -SUM(cost) FROM acc_move WHERE doc_id = ?`,
		res.DocID).Scan(&cogs); err != nil {
		t.Fatal(err)
	}
	if !near(cogs, 1_550_000_000) {
		t.Errorf("себестоимость запуска = %.2f, ожидалось 1 550 000 000", cogs)
	}

	var left float64
	var qty int64
	if err := s.db.QueryRow(`SELECT COALESCE(SUM(cost_left),0), COALESCE(SUM(qty_left),0)
		FROM acc_lot`).Scan(&left, &qty); err != nil {
		t.Fatal(err)
	}
	if !near(left, 720_000_000) || qty != 300_000 {
		t.Errorf("остаток = %d шт на %.2f, ожидалось 300 000 шт на 720 000 000", qty, left)
	}
	// The whole point: FIFO only splits what was already spent.
	if !near(cogs+left, 2_270_000_000) {
		t.Errorf("списано + осталось = %.2f, а потрачено было 2 270 000 000", cogs+left)
	}
}

// TestNoCostDrift consumes lots in awkward slices; the last issue of a lot
// must take exactly what is left, so nothing can end at "0 units, 4.7 ISK".
func TestNoCostDrift(t *testing.T) {
	s := testStore(t)
	buy(t, s, "A", 7, 1.0/3.0, 5) // цена, которая не делится нацело
	buy(t, s, "B", 5, 1.0/7.0, 4)

	for i, q := range []int64{1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1} {
		if _, err := s.PostDoc(
			Doc{Kind: "sale", OwnerID: owner, At: time.Now(), Src: "test",
				SrcID: "s" + string(rune('a'+i))},
			[]Line{{Place: hangar(), TypeID: strontium, Qty: -q}}, nil); err != nil {
			t.Fatalf("продажа %d: %v", i, err)
		}
	}
	var qty int64
	var left float64
	if err := s.db.QueryRow(`SELECT COALESCE(SUM(qty_left),0), COALESCE(SUM(cost_left),0)
		FROM acc_lot`).Scan(&qty, &left); err != nil {
		t.Fatal(err)
	}
	if qty != 0 {
		t.Fatalf("осталось %d шт, ожидалось 0", qty)
	}
	if left != 0 {
		t.Errorf("склад пуст, а себестоимости осталось %v — дрейф округления", left)
	}
	// Everything that came in must have gone out, to the ISK.
	var in, out float64
	s.db.QueryRow(`SELECT COALESCE(SUM(cost),0) FROM acc_move WHERE qty > 0`).Scan(&in)
	s.db.QueryRow(`SELECT COALESCE(-SUM(cost),0) FROM acc_move WHERE qty < 0`).Scan(&out)
	if !near(in, out) {
		t.Errorf("пришло %.6f, ушло %.6f", in, out)
	}
}

// TestSpecificIdentification is the owner's actual requirement: send THIS
// purchase into production, not the oldest one.
func TestSpecificIdentification(t *testing.T) {
	s := testStore(t)
	buy(t, s, "A", 100, 10, 5) // старая и дешёвая
	buy(t, s, "B", 100, 90, 1) // свежая и дорогая

	var newest int64
	if err := s.db.QueryRow(`SELECT id FROM acc_lot ORDER BY at DESC LIMIT 1`).Scan(&newest); err != nil {
		t.Fatal(err)
	}
	res, err := s.PostDoc(
		Doc{Kind: "manufacture", OwnerID: owner, At: time.Now(), Src: "test", SrcID: "job2"},
		[]Line{{Place: hangar(), TypeID: strontium, Qty: -100,
			Alloc: []Alloc{{LotID: newest, Qty: 100}}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var cogs float64
	s.db.QueryRow(`SELECT -SUM(cost) FROM acc_move WHERE doc_id = ?`, res.DocID).Scan(&cogs)
	if !near(cogs, 9000) {
		t.Errorf("списано %.2f, а указана была дорогая партия — ожидалось 9000", cogs)
	}
}

// TestShortfall: issuing what the ledger has never seen must not fail and
// must not be silently free — it becomes a flagged estimate lot.
func TestShortfall(t *testing.T) {
	s := testStore(t)
	res, err := s.PostDoc(
		Doc{Kind: "sale", OwnerID: owner, At: time.Now(), Src: "test", SrcID: "s1"},
		[]Line{{Place: hangar(), TypeID: strontium, Qty: -50, ShortfallUnitCost: 3}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Shortfall != 50 {
		t.Errorf("нехватка %d, ожидалось 50", res.Shortfall)
	}
	var kind string
	var cost float64
	s.db.QueryRow(`SELECT cost_kind, cost_total FROM acc_lot`).Scan(&kind, &cost)
	if kind != "estimate" {
		t.Errorf("партия-затычка помечена %q, должна быть estimate", kind)
	}
	if !near(cost, 150) {
		t.Errorf("оценка %.2f, ожидалось 150", cost)
	}
}

// TestIdempotent: re-posting the same ESI object changes nothing, which is
// what lets the ledger be rebuilt by simply running the builders again.
func TestIdempotent(t *testing.T) {
	s := testStore(t)
	buy(t, s, "A", 100, 10, 1)
	buy(t, s, "A", 100, 10, 1) // тот же src_id

	var lots, docs int
	s.db.QueryRow(`SELECT COUNT(*) FROM acc_lot`).Scan(&lots)
	s.db.QueryRow(`SELECT COUNT(*) FROM acc_doc`).Scan(&docs)
	if lots != 1 || docs != 1 {
		t.Errorf("после повторной проводки партий %d, документов %d — ожидалось по 1", lots, docs)
	}
}

// TestScopeWidening: goods sold off a market order are not in the hangar
// place any more, so an issue must be able to look wider than one place.
func TestScopeWidening(t *testing.T) {
	s := testStore(t)
	container := PlaceKey{OwnerID: owner, LocationID: station, HolderID: 999,
		Flag: "Hangar", Name: "Закуп 14.08"}
	if _, err := s.PostDoc(
		Doc{Kind: "purchase", OwnerID: owner, At: time.Now().AddDate(0, 0, -1),
			Src: "test", SrcID: "inbox"},
		[]Line{{Place: container, TypeID: strontium, Qty: 100, CostTotal: 1000}},
		nil); err != nil {
		t.Fatal(err)
	}
	res, err := s.PostDoc(
		Doc{Kind: "sale", OwnerID: owner, At: time.Now(), Src: "test", SrcID: "sale1"},
		[]Line{{Place: hangar(), TypeID: strontium, Qty: -100}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Shortfall != 0 {
		t.Errorf("нехватка %d: расход не нашёл партию в контейнере той же станции", res.Shortfall)
	}
}

// TestTransferCarriesCost: moving goods must not create or destroy value.
// A transfer priced at market instead of at cost would let profit be
// manufactured by shuffling stock between alts.
func TestTransferCarriesCost(t *testing.T) {
	s := testStore(t)
	buy(t, s, "A", 100, 7, 3)

	const other = int64(60003761)
	to := PlaceKey{OwnerID: owner, LocationID: other, Flag: "Hangar"}
	if _, err := s.PostDoc(
		Doc{Kind: "transfer", OwnerID: owner, At: time.Now(), Src: "test", SrcID: "mv1"},
		[]Line{
			{Place: hangar(), TypeID: strontium, Qty: -100, Scope: "location"},
			{Place: to, TypeID: strontium, Qty: 100, CostFrom: "issue"},
		}, nil); err != nil {
		t.Fatalf("перемещение: %v", err)
	}

	var qty int64
	var cost float64
	if err := s.db.QueryRow(`SELECT COALESCE(SUM(qty_left),0), COALESCE(SUM(cost_left),0)
		FROM acc_lot WHERE qty_left > 0`).Scan(&qty, &cost); err != nil {
		t.Fatal(err)
	}
	if qty != 100 || !near(cost, 700) {
		t.Errorf("после переезда %d шт на %.2f, ожидалось 100 шт на 700", qty, cost)
	}
	// And it must actually be at the destination now.
	var atDest int64
	s.db.QueryRow(`SELECT COALESCE(SUM(l.qty_left),0) FROM acc_lot l
		JOIN acc_place p ON p.id = l.place_id
		WHERE p.location_id = ?`, other).Scan(&atDest)
	if atDest != 100 {
		t.Errorf("в точке назначения %d шт, ожидалось 100", atDest)
	}
}

// TestEstimateSurvivesTransfer: a guessed price must not become a fact by
// moving the goods. Otherwise a report could not say how much of the
// profit still rests on guesses — the flag would launder itself away.
func TestEstimateSurvivesTransfer(t *testing.T) {
	s := testStore(t)
	if _, err := s.PostDoc(
		Doc{Kind: "opening", OwnerID: owner, At: time.Now().AddDate(0, 0, -2),
			Src: "test", SrcID: "open"},
		[]Line{{Place: hangar(), TypeID: strontium, Qty: 100,
			CostTotal: 500, CostKind: "estimate"}}, nil); err != nil {
		t.Fatal(err)
	}
	to := PlaceKey{OwnerID: owner, LocationID: 60003761, Flag: "Hangar"}
	if _, err := s.PostDoc(
		Doc{Kind: "transfer", OwnerID: owner, At: time.Now(), Src: "test", SrcID: "mv2"},
		[]Line{
			{Place: hangar(), TypeID: strontium, Qty: -100, Scope: "location"},
			{Place: to, TypeID: strontium, Qty: 100, CostFrom: "issue"},
		}, nil); err != nil {
		t.Fatal(err)
	}
	var kind string
	var cost float64
	if err := s.db.QueryRow(`SELECT cost_kind, cost_left FROM acc_lot
		WHERE qty_left > 0`).Scan(&kind, &cost); err != nil {
		t.Fatal(err)
	}
	if kind != "estimate" {
		t.Errorf("после переезда партия помечена %q — оценка отмылась в факт", kind)
	}
	if !near(cost, 500) {
		t.Errorf("себестоимость после переезда %.2f, ожидалось 500", cost)
	}
}
