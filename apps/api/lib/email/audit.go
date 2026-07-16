package email

import "context"

// Status values reported to the audit hook after each send attempt.
const (
	AuditStatusSent    = "sent"
	AuditStatusFailed  = "failed"
	AuditStatusSkipped = "skipped"
)

// AuditEntry describes one email send attempt for the unified notification
// audit trail (notification_logs, channel='email'). StoreID/CartID/EventID
// are optional context propagated from the caller — empty when the call site
// doesn't have them.
type AuditEntry struct {
	StoreID           string
	CartID            string
	EventID           string
	Kind              string // e.g. "order_paid", "invitation", "test_email"
	ToEmail           string
	Subject           string
	Status            string // AuditStatusSent | AuditStatusFailed | AuditStatusSkipped
	ProviderMessageID string // Resend message id, when the API returned one
	ErrorMessage      string
}

// AuditHook receives one AuditEntry per send attempt (sent/failed/skipped).
// Implementations must be non-blocking-ish and never panic: the hook runs
// inline on the send path, which is always best-effort.
type AuditHook func(ctx context.Context, e AuditEntry)

// SetAuditHook wires the audit sink. Optional — when unset, sends are not
// recorded. lib/email stays persistence-agnostic: the concrete hook (insert
// into notification_logs) is wired by the HTTP server at boot.
func (c *Client) SetAuditHook(hook AuditHook) {
	c.auditHook = hook
}

// audit invokes the hook when configured.
func (c *Client) audit(ctx context.Context, e AuditEntry) {
	if c.auditHook == nil {
		return
	}
	c.auditHook(ctx, e)
}

// auditSkipped records an attempt short-circuited before reaching the Resend
// API (missing RESEND_API_KEY).
func (c *Client) auditSkipped(ctx context.Context, e AuditEntry) {
	e.Status = AuditStatusSkipped
	c.audit(ctx, e)
}

// auditFailed records an attempt that failed before the API call could
// succeed (template render, request build, transport or API error).
func (c *Client) auditFailed(ctx context.Context, e AuditEntry, err error) {
	e.Status = AuditStatusFailed
	if err != nil {
		e.ErrorMessage = err.Error()
	}
	c.audit(ctx, e)
}
