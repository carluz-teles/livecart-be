package product

import "livecart/apps/api/internal/product/domain"

// OptionValueRef is the denormalized representation of one option/value pair
// attached to a variant (e.g. {Option: "Color", Value: "Red"}).
// Exposed as part of ProductResponse and reused by the productgroup package.
type OptionValueRef struct {
	Option string `json:"option"`
	Value  string `json:"value"`
}

// ShippingDTOToDomain converts a ShippingProfileDTO into the domain ShippingProfile.
// Exported so other packages (productgroup, integration) can reuse the same mapping.
func ShippingDTOToDomain(dto ShippingProfileDTO) (domain.ShippingProfile, error) {
	return shippingDTOToDomain(dto)
}
