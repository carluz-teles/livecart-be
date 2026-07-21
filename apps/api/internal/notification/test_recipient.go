package notification

import (
	"context"
	"crypto/rand"
	"encoding/base32"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"go.uber.org/zap"

	"livecart/apps/api/db/sqlc"
	"livecart/apps/api/lib/logger"
)

// SetupCodeTTL is how long the lojista has to send the magic code from their
// personal IG to the business account before it expires.
const SetupCodeTTL = 10 * time.Minute

// TestRecipient describes the IG account that should receive test
// notifications for a store. PSID and Handle are filled once the lojista has
// completed the setup flow.
type TestRecipient struct {
	PSID         string
	Handle       string
	SetupCode    string
	SetupExpires time.Time
}

// Configured reports whether the store has captured a recipient yet.
func (r TestRecipient) Configured() bool {
	return r.PSID != "" && r.Handle != ""
}

// SetupActive reports whether there is a non-expired setup code waiting for an
// IG message to arrive and complete the flow.
func (r TestRecipient) SetupActive(now time.Time) bool {
	return r.SetupCode != "" && r.SetupExpires.After(now)
}

// GetTestRecipient returns the current test recipient state for a store.
func (s *Service) GetTestRecipient(ctx context.Context, storeID string) (*TestRecipient, error) {
	uid, err := parseUUID(storeID)
	if err != nil {
		return nil, err
	}

	row, err := s.queries.GetStoreTestRecipient(ctx, uid)
	if err != nil {
		return nil, fmt.Errorf("getting test recipient: %w", err)
	}

	out := &TestRecipient{}
	if row.NotificationTestRecipientPsid.Valid {
		out.PSID = row.NotificationTestRecipientPsid.String
	}
	if row.NotificationTestRecipientHandle.Valid {
		out.Handle = row.NotificationTestRecipientHandle.String
	}
	if row.NotificationTestSetupCode.Valid {
		out.SetupCode = row.NotificationTestSetupCode.String
	}
	if row.NotificationTestSetupExpiresAt.Valid {
		out.SetupExpires = row.NotificationTestSetupExpiresAt.Time
	}
	return out, nil
}

// StartTestRecipientSetup generates a fresh magic code and persists it with a
// short TTL. The lojista is expected to DM this code from their personal IG to
// the business account; the IG webhook handler will then call
// CompleteTestRecipientSetup to capture the sender.
func (s *Service) StartTestRecipientSetup(ctx context.Context, storeID string) (*TestRecipient, error) {
	uid, err := parseUUID(storeID)
	if err != nil {
		return nil, err
	}

	code, err := generateSetupCode()
	if err != nil {
		return nil, fmt.Errorf("generating setup code: %w", err)
	}

	expires := time.Now().Add(SetupCodeTTL)
	if err := s.queries.SetStoreTestSetupCode(ctx, sqlc.SetStoreTestSetupCodeParams{
		ID: uid,
		NotificationTestSetupCode: pgtype.Text{
			String: code,
			Valid:  true,
		},
		NotificationTestSetupExpiresAt: pgtype.Timestamptz{
			Time:  expires,
			Valid: true,
		},
	}); err != nil {
		return nil, fmt.Errorf("persisting setup code: %w", err)
	}

	return s.GetTestRecipient(ctx, storeID)
}

// CompleteTestRecipientSetup looks up the store that owns the given setup code
// and saves the sender as that store's test recipient. Returns the store ID on
// success so the caller can continue any follow-up logic. Returns an empty
// string if no active code matches — the caller should treat that as "this DM
// is a regular customer message, not a setup attempt".
func (s *Service) CompleteTestRecipientSetup(ctx context.Context, candidateCode, senderPSID, senderHandle string) (string, error) {
	code := normalizeSetupCode(candidateCode)
	if code == "" {
		return "", nil
	}

	storeUID, err := s.queries.FindStoreByActiveTestSetupCode(ctx, pgtype.Text{
		String: code,
		Valid:  true,
	})
	if err != nil {
		// Not found is the common case: the message text is not a setup code.
		return "", nil
	}

	if err := s.queries.SetStoreTestRecipient(ctx, sqlc.SetStoreTestRecipientParams{
		ID: storeUID,
		NotificationTestRecipientPsid: pgtype.Text{
			String: senderPSID,
			Valid:  senderPSID != "",
		},
		NotificationTestRecipientHandle: pgtype.Text{
			String: senderHandle,
			Valid:  senderHandle != "",
		},
	}); err != nil {
		return "", fmt.Errorf("storing test recipient: %w", err)
	}

	return storeUID.String(), nil
}

// SendTest renders the given template with sample variables and dispatches a
// real DM to the store's configured test recipient.
func (s *Service) SendTest(ctx context.Context, storeID string, notifType NotificationType, template string) error {
	recipient, err := s.GetTestRecipient(ctx, storeID)
	if err != nil {
		return err
	}
	if !recipient.Configured() {
		return ErrTestRecipientNotConfigured
	}

	if _, err := ValidateTemplate(template, SampleVariables()); err != nil {
		return err
	}

	message := RenderTemplate(template, SampleVariables())
	if len(message) > MaxMessageBytes {
		message = TruncateMessage(message, MaxMessageBytes)
	}

	if err := s.dmSender.SendInstagramDM(ctx, storeID, recipient.PSID, message); err != nil {
		logger.From(ctx, s.logger).Warn("test notification failed",
			zap.String("store_id", storeID),
			zap.String("type", string(notifType)),
			zap.Error(err),
		)
		return fmt.Errorf("sending test DM: %w", err)
	}
	return nil
}

// ErrTestRecipientNotConfigured is returned by SendTest when the store has not
// completed the setup flow yet. The HTTP layer maps this to a 412 with a clear
// message so the FE can prompt the lojista to configure first.
var ErrTestRecipientNotConfigured = fmt.Errorf("test recipient not configured")

// generateSetupCode returns a short, copy-paste-friendly code. The format is
// `LIVECART-XXXXXX` using a 30-bit base32 random value (no padding, no
// confusable characters from the standard base32 alphabet).
func generateSetupCode() (string, error) {
	var raw [4]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	enc := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(raw[:])
	if len(enc) > 6 {
		enc = enc[:6]
	}
	return "LIVECART-" + enc, nil
}

// normalizeSetupCode trims whitespace and uppercases so an IG message like
// "  livecart-abc123 " still matches the stored code.
func normalizeSetupCode(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	return strings.ToUpper(s)
}
