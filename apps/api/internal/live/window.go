package live

import (
	"fmt"
	"time"
)

// brtLocation is Brazil's current civil time (UTC-03:00). Brazil no longer
// observes DST, so a fixed offset is correct and avoids a tzdata dependency.
// Timestamps are stored in UTC; only customer-facing messages are formatted here.
var brtLocation = time.FixedZone("BRT", -3*60*60)

// EffectiveStatus derives the status a merchant/customer should see from the
// stored status and the scheduled window, without any background job:
//   - a manually ended event stays "ended"
//   - past its end time → "ended"
//   - before its start time → "scheduled"
//   - otherwise → "active"
func EffectiveStatus(stored string, scheduledAt, endsAt *time.Time) string {
	if stored == "ended" {
		return "ended"
	}
	now := time.Now()
	if endsAt != nil && now.After(*endsAt) {
		return "ended"
	}
	if scheduledAt != nil && now.Before(*scheduledAt) {
		return "scheduled"
	}
	if stored == "scheduled" {
		// No window says otherwise but it was created as scheduled.
		return "scheduled"
	}
	return "active"
}

// FormatBRT renders a UTC time in Brazil time (UTC-3) for customer messages,
// e.g. "04/06 às 18h30".
func FormatBRT(t time.Time) string {
	local := t.In(brtLocation)
	return fmt.Sprintf("%02d/%02d às %02dh%02d",
		local.Day(), int(local.Month()), local.Hour(), local.Minute())
}
