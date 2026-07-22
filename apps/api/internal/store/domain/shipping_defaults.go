package domain

// ShippingDefaults holds the store-level defaults used when an ERP-imported
// product only carries weight. The optional dimensions are all-or-nothing.
type ShippingDefaults struct {
	packageWeightGrams int
	packageFormat      string
	heightCm           *int
	widthCm            *int
	lengthCm           *int
}

// ReconstructShippingDefaults rebuilds ShippingDefaults from persistence data.
func ReconstructShippingDefaults(
	packageWeightGrams int,
	packageFormat string,
	heightCm, widthCm, lengthCm *int,
) ShippingDefaults {
	return ShippingDefaults{
		packageWeightGrams: packageWeightGrams,
		packageFormat:      packageFormat,
		heightCm:           heightCm,
		widthCm:            widthCm,
		lengthCm:           lengthCm,
	}
}

func (s ShippingDefaults) PackageWeightGrams() int { return s.packageWeightGrams }
func (s ShippingDefaults) PackageFormat() string   { return s.packageFormat }
func (s ShippingDefaults) HeightCm() *int          { return s.heightCm }
func (s ShippingDefaults) WidthCm() *int           { return s.widthCm }
func (s ShippingDefaults) LengthCm() *int          { return s.lengthCm }
