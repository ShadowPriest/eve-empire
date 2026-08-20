package pi

import "testing"

// Reference values read out of the client's extraction program window
// for a Noble Metals extractor: qty_per_cycle 13848, 4h cycle, 84 cycles.
func TestExtractionProgramMatchesClient(t *testing.T) {
	const (
		base   = 13848
		cycle  = 14400 // 4 hours
		cycles = 84
	)
	want := map[int]int64{
		1: 202160, 2: 188863, 3: 149708, 4: 141433, 5: 123781, 6: 148109,
		7: 98562, 9: 96815, 13: 67239, 17: 53159, 27: 36394, 28: 62326,
		55: 19327, 56: 19008, 84: 13008,
	}

	prog := ExtractionProgram(base, cycle, cycles)
	if len(prog) != cycles {
		t.Fatalf("got %d cycles, want %d", len(prog), cycles)
	}
	for c, w := range want {
		if got := prog[c-1].Qty; got != w {
			t.Errorf("cycle %d: got %d, want %d", c, got, w)
		}
	}

	total, perHour, _ := ProgramTotals(prog, cycle)
	if want := int64(3680687); total != want {
		t.Errorf("program total: got %d, want %d", total, want)
	}
	// 84 cycles x 4h = 336 hours
	if perHour < 10953 || perHour > 10955 {
		t.Errorf("per hour: got %.0f, want ~10954", perHour)
	}
}
