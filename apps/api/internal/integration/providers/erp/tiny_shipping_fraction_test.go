package erp

import (
	"math"
	"testing"
)

func TestTinyShippingPositiveFractionsRemainValid(t *testing.T) {
	for _, tc := range []struct {
		name                          string
		weight, height, width, length float64
		want                          ERPShippingProfile
	}{
		{"reported product", 4.25, 46, 0.4, 50, ERPShippingProfile{WeightGrams: 4250, HeightCm: 46, WidthCm: 1, LengthCm: 50, PackageFormat: "box"}},
		{"fractions", 0.0004, 0.1, 1.2, 2.5, ERPShippingProfile{WeightGrams: 1, HeightCm: 1, WidthCm: 2, LengthCm: 3, PackageFormat: "box"}},
		{"whole units", 1, 10, 20, 30, ERPShippingProfile{WeightGrams: 1000, HeightCm: 10, WidthCm: 20, LengthCm: 30, PackageFormat: "box"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			flat := flatDimensionsToShipping(tc.weight, tc.height, tc.width, tc.length)
			parent := dimensoesToShipping(&tinyDimensoes{PesoBruto: tc.weight, Altura: tc.height, Largura: tc.width, Comprimento: tc.length})
			for _, got := range []*ERPShippingProfile{flat, parent} {
				if got == nil || *got != tc.want {
					t.Fatalf("shipping = %+v, want %+v", got, tc.want)
				}
			}
		})
	}
}

func TestTinyShippingRejectsInvalidMeasures(t *testing.T) {
	for _, invalid := range []float64{0, -1, math.NaN(), math.Inf(1), math.Inf(-1), float64(math.MaxInt)} {
		for field := range 4 {
			values := []float64{1, 10, 20, 30}
			values[field] = invalid
			if got := flatDimensionsToShipping(values[0], values[1], values[2], values[3]); got != nil {
				t.Fatalf("invalid field %d (%v) produced %+v", field, invalid, got)
			}
		}
	}
}
