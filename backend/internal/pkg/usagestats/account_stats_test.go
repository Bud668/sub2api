package usagestats

import (
	"math"
	"testing"
)

func TestEstimateWindowTotalCost(t *testing.T) {
	for _, row := range [][3]float64{{720, 40, 1800}, {1615.59, 74, 2183.2297297297296}, {93.18, 25.6, 363.984375}} {
		got := EstimateWindowTotalCost(row[0], row[1])
		if got == nil || math.Abs(*got-row[2]) > 1e-8 {
			t.Fatalf("%v: estimate=%v", row, got)
		}
	}
	for _, row := range [][2]float64{{0, 40}, {720, 0}, {720, 101}, {-1, 40}, {720, -1}, {math.NaN(), 40}, {720, math.NaN()}, {math.Inf(1), 40}, {720, math.Inf(1)}, {math.MaxFloat64, .01}} {
		if got := EstimateWindowTotalCost(row[0], row[1]); got != nil {
			t.Fatalf("%v invented estimate %v", row, *got)
		}
	}
}
