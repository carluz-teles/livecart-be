package social

import "fmt"

// Only an explicit authorization rejection disconnects a credential shared by
// multiple stores. Network, quota and server failures must remain retryable.
type instagramRefreshError struct {
	status    int
	code      int
	permanent bool
	expired   bool
}

func (e *instagramRefreshError) Permanent() bool { return e.permanent }

func (e *instagramRefreshError) Error() string {
	if e.expired {
		return "instagram token expired; reconnect the account"
	}
	return fmt.Sprintf("instagram token refresh failed: status %d, code %d", e.status, e.code)
}
