package providers

import (
	"encoding/json"
	"strings"
)

const PaymentMethodERPManual = "erp_manual"

// IsERPRecordedPaymentSnapshot identifies our internal acknowledgment of money
// confirmed by the merchant in the ERP. It must not run checkout writes back
// into that same sale.
func IsERPRecordedPaymentSnapshot(raw []byte) bool {
	var status PaymentStatus
	return json.Unmarshal(raw, &status) == nil && status.PaymentMethod == PaymentMethodERPManual && strings.HasPrefix(status.PaymentID, "erp-") && status.Amount > 0
}
