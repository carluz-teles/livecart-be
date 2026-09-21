// Package cartpricing projects the exact prices accepted for each addition.
package cartpricing

import "encoding/json"

type Lot struct {
	Quantity           int   `json:"quantity"`
	WaitlistedQuantity int   `json:"waitlistedQuantity"`
	UnitPrice          int64 `json:"unitPrice"`
	TotalPrice         int64 `json:"totalPrice"`
}

func Decode(raw []byte) ([]Lot, error) {
	var lots []Lot
	err := json.Unmarshal(raw, &lots)
	return lots, err
}

// Available uses the legacy line only when no ledger was supplied, for callers
// and read models that predate price lots. Every database line has a ledger.
func Available(lots []Lot, quantity, waiting int, price int64) int64 {
	if len(lots) == 0 {
		return int64(max(0, quantity-waiting)) * price
	}
	var total int64
	for _, lot := range lots {
		total += int64(lot.Quantity-lot.WaitlistedQuantity) * lot.UnitPrice
	}
	return total
}

// Reserved combines equal prices, keeping different prices as separate lines.
func Reserved(lots []Lot, quantity, waiting int, price int64) []Lot {
	if len(lots) == 0 {
		lots = []Lot{{Quantity: quantity, WaitlistedQuantity: waiting, UnitPrice: price}}
	}
	result := make([]Lot, 0, len(lots))
	byPrice := make(map[int64]int)
	for _, lot := range lots {
		qty := lot.Quantity - lot.WaitlistedQuantity
		if qty <= 0 {
			continue
		}
		if idx, ok := byPrice[lot.UnitPrice]; ok {
			result[idx].Quantity += qty
			result[idx].TotalPrice += int64(qty) * lot.UnitPrice
			continue
		}
		byPrice[lot.UnitPrice] = len(result)
		result = append(result, Lot{Quantity: qty, UnitPrice: lot.UnitPrice, TotalPrice: int64(qty) * lot.UnitPrice})
	}
	return result
}

// Total includes pending demand for order-list consistency; Available is the
// amount that may enter a checkout quote.
func Total(lots []Lot, quantity int, price int64) int64 {
	if len(lots) == 0 {
		return int64(quantity) * price
	}
	var total int64
	for _, lot := range lots {
		total += int64(lot.Quantity) * lot.UnitPrice
	}
	return total
}

// Covered values paid quantities in their original addition order. Pending
// units were never charged and cannot consume the coverage count.
func Covered(lots []Lot, quantity, waiting int, price int64, paid int) int64 {
	if len(lots) == 0 {
		return int64(min(paid, max(0, quantity-waiting))) * price
	}
	var total int64
	for _, lot := range lots {
		qty := min(paid, lot.Quantity-lot.WaitlistedQuantity)
		total += int64(qty) * lot.UnitPrice
		paid -= qty
	}
	return total
}
