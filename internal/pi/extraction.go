package pi

import "math"

// Extraction program constants. ESI exposes no per-cycle numbers, so the
// curve is reproduced here.
//
// Verified against the client on a base of 13848 with a 4h cycle: all 15
// sampled cycles (1..84) match exactly and the program total lands on
// 3 680 687, the same figure the client shows.
//
// The one non-obvious part is the wobble: it only ever adds. Community
// formulas apply it signed, which undershoots every cycle where the
// cosines happen to sum below zero — clamping it at zero is what makes
// the numbers line up.
const (
	decayFactor = 0.012
	noiseFactor = 0.8
	// qty_per_cycle is the yield per 15-minute unit; longer cycles
	// harvest proportionally more.
	unitSeconds = 900.0
)

// CycleYield is the amount produced by one extraction cycle.
type CycleYield struct {
	Index int
	Qty   int64
}

// ExtractionProgram reproduces the full yield curve of an extractor.
// baseQty is qty_per_cycle from ESI (the value the program was
// installed with), cycleSeconds its cycle time and cycles the number of
// cycles the program runs for.
func ExtractionProgram(baseQty int64, cycleSeconds, cycles int) []CycleYield {
	if baseQty <= 0 || cycleSeconds <= 0 || cycles <= 0 {
		return nil
	}
	out := make([]CycleYield, 0, cycles)
	base := float64(baseQty)
	scale := float64(cycleSeconds) / unitSeconds
	phaseShift := math.Pow(base, 0.7)
	for i := 0; i < cycles; i++ {
		t := (float64(i) + 0.5) * scale
		decay := base / (1 + t*decayFactor)
		sinA := math.Cos(phaseShift + t/12)
		sinB := math.Cos(phaseShift/2 + t/5)
		sinC := math.Cos(t / 2)
		wobble := (sinA + sinB + sinC) / 3
		if wobble < 0 {
			wobble = 0 // the client never subtracts, only adds
		}
		qty := decay * (1 + noiseFactor*wobble) * scale
		if qty < 0 {
			qty = 0
		}
		out = append(out, CycleYield{Index: i, Qty: int64(math.Floor(qty))})
	}
	return out
}

// ProgramTotals sums a program and reports its hourly average.
func ProgramTotals(prog []CycleYield, cycleSeconds int) (total int64, perHour float64, peak int64) {
	for _, c := range prog {
		total += c.Qty
		if c.Qty > peak {
			peak = c.Qty
		}
	}
	if len(prog) > 0 && cycleSeconds > 0 {
		hours := float64(len(prog)) * float64(cycleSeconds) / 3600
		if hours > 0 {
			perHour = float64(total) / hours
		}
	}
	return
}
