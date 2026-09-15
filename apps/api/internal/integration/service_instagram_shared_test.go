//go:build integration

package integration

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
	"livecart/apps/api/db/sqlc"
	"livecart/apps/api/internal/notification"
)

func seedSharedInstagram(t *testing.T, storeID, accountID, altID, status string) string {
	t.Helper()
	row, err := testRepo.Create(t.Context(), CreateIntegrationParams{
		StoreID: storeID, Type: "social", Provider: "instagram", Status: status,
		Credentials: []byte("test-only"), Metadata: map[string]any{"instagram_user_id": accountID, "instagram_app_scoped_id": altID},
	})
	if err != nil {
		t.Fatal(err)
	}
	return row.ID
}

func TestInstagramSharedAccountMessages(t *testing.T) {
	requireDB(t)
	ctx := t.Context()
	a, b, other := seedStoreForReconnect(t), seedStoreForReconnect(t), seedStoreForReconnect(t)
	account, alt := "account-"+a, "scoped-"+a
	seedSharedInstagram(t, a, account, alt, "active")
	second := seedSharedInstagram(t, b, account, alt, "active")
	seedSharedInstagram(t, other, account, alt, "error")
	// Duplicate rows in one store must not create a false multiple-store match.
	seedSharedInstagram(t, a, account, alt, "active")
	for _, id := range []string{account, alt} {
		stores, err := testRepo.ListInstagramStoreIDs(ctx, id)
		if err != nil || len(stores) != 2 || !containsID(stores, a) || !containsID(stores, b) {
			t.Fatalf("stores=%v err=%v", stores, err)
		}
	}
	for _, id := range []string{"", "unknown-" + a} {
		stores, err := testRepo.ListInstagramStoreIDs(ctx, id)
		if err != nil || len(stores) != 0 {
			t.Fatalf("unexpected stores=%v err=%v", stores, err)
		}
	}
	core, logs := observer.New(zap.InfoLevel)
	svc := &Service{repo: testRepo, logger: zap.New(core), notificationService: notification.NewService(sqlc.New(testPool), nil, zap.NewNop())}
	message := ProcessInstagramMessageInput{AccountID: account, SenderID: "buyer", MessageID: "shared-message-" + a, Text: "Olá", RawPayload: []byte(`{"message":"hello"}`), SignatureValid: true}
	if err := svc.HandleMessageReceived(ctx, message); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := testPool.QueryRow(ctx, `SELECT count(*) FROM webhook_events WHERE event_id=$1`, message.MessageID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("generic DM assigned to a store: %d %v", count, err)
	}
	if logs.FilterMessage("instagram message ignored: shared account without store context").Len() != 1 {
		t.Fatal("missing shared-account diagnostic")
	}
	for _, store := range []string{b, other} {
		if _, err := testPool.Exec(ctx, `UPDATE stores SET notification_test_setup_code=$2,notification_test_setup_expires_at=$3 WHERE id=$1`, store, strings.ToUpper("LIVECART-"+store), time.Now().Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	message.Text = "LIVECART-" + other
	if err := svc.HandleMessageReceived(ctx, message); err != nil {
		t.Fatal(err)
	}
	var psid *string
	if err := testPool.QueryRow(ctx, `SELECT notification_test_recipient_psid FROM stores WHERE id=$1`, other).Scan(&psid); err != nil || psid != nil {
		t.Fatalf("unconnected store recipient changed: %v %v", psid, err)
	}
	message.Text = "LIVECART-" + b
	if err := svc.HandleMessageReceived(ctx, message); err != nil {
		t.Fatal(err)
	}
	var integrationID string
	if err := testPool.QueryRow(ctx, `SELECT integration_id::text FROM webhook_events WHERE event_id=$1`, message.MessageID).Scan(&integrationID); err != nil || integrationID != second {
		t.Fatalf("setup DM audit owner=%s want=%s err=%v", integrationID, second, err)
	}
	if err := testPool.QueryRow(ctx, `SELECT notification_test_recipient_psid FROM stores WHERE id=$1`, b).Scan(&psid); err != nil || psid == nil || *psid != "buyer" {
		t.Fatalf("recipient not configured: %v %v", psid, err)
	}

	// With a single connected store, ordinary DMs retain their existing audit.
	unique := fmt.Sprintf("unique-%s", other)
	seedSharedInstagram(t, other, unique, "", "active")
	message.AccountID, message.MessageID, message.Text = unique, "unique-message-"+other, "hello"
	if err := svc.HandleMessageReceived(ctx, message); err != nil {
		t.Fatal(err)
	}
	if err := testPool.QueryRow(ctx, `SELECT count(*) FROM webhook_events WHERE event_id=$1`, message.MessageID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("single-store audit missing: %d %v", count, err)
	}
}

func containsID(ids []string, want string) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}
