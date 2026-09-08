//go:build integration

package integration

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"livecart/apps/api/internal/integration/providers"
)

func blingOAuthTestCredentials(token string) *providers.Credentials {
	return &providers.Credentials{
		AccessToken: token, RefreshToken: "refresh-" + token,
		ExpiresAt: time.Now().Add(6 * time.Hour),
	}
}

func blingOAuthTestIdentity(account string) map[string]any {
	return map[string]any{
		providers.MetadataBlingCompanyID: account,
		"bling_company_name":             "Test company",
		"bling_company_document":         "test-document",
	}
}

func createBlingOAuthTestConnection(t *testing.T, svc *Service, storeID, account string) string {
	t.Helper()
	id, err := svc.upsertBlingIntegration(context.Background(), storeID,
		blingOAuthTestCredentials("original-token"), blingOAuthTestIdentity(account))
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestBlingOAuthStateIsProviderBoundAndSingleUse(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	storeID := seedStoreForReconnect(t)
	otherState := uuid.NewString()
	if err := testRepo.CreateOAuthState(ctx, otherState, storeID, "tiny", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := testRepo.consumeBlingOAuthState(ctx, otherState); err == nil {
		t.Fatal("Bling accepted a state belonging to Tiny")
	}
	if _, err := testRepo.GetOAuthState(ctx, otherState); err != nil {
		t.Fatalf("Bling consumed the other provider's state: %v", err)
	}
	state := uuid.NewString()
	if err := testRepo.CreateOAuthState(ctx, state, storeID, "bling", ""); err != nil {
		t.Fatal(err)
	}
	var accepted atomic.Int32
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			row, err := testRepo.consumeBlingOAuthState(ctx, state)
			if err == nil {
				accepted.Add(1)
				if row.Provider != "bling" || uuidToString(row.StoreID) != storeID {
					t.Error("consumed state has the wrong owner")
				}
			}
		}()
	}
	wg.Wait()
	if n := accepted.Load(); n != 1 {
		t.Fatalf("state was accepted %d times, expected once", n)
	}
}

func TestBlingOAuthReconnectPreservesSameAccountSettingsInEveryStatus(t *testing.T) {
	requireDB(t)
	for _, status := range []string{"active", "pending_auth", "error", "disconnected"} {
		t.Run(status, func(t *testing.T) {
			ctx := context.Background()
			svc := reconnectTestService(t)
			storeID := seedStoreForReconnect(t)
			account := uuid.NewString()
			id := createBlingOAuthTestConnection(t, svc, storeID, account)
			metadata := blingOAuthTestIdentity(account)
			metadata["reserva_capacidade_confirmada"] = true
			metadata["webhookLastPingAt"] = "2026-09-08T01:00:00Z"
			metadata["custom_setting"] = "current-database-value"
			if err := testRepo.UpdateMetadata(ctx, id, metadata); err != nil {
				t.Fatal(err)
			}
			if err := testRepo.UpdateStatus(ctx, id, status); err != nil {
				t.Fatal(err)
			}
			input := blingOAuthTestIdentity(account)
			input["custom_setting"] = "stale-snapshot"
			newID, err := svc.upsertBlingIntegration(ctx, storeID, blingOAuthTestCredentials("new-token"), input)
			if err != nil || newID != id {
				t.Fatalf("reconnect failed or replaced the row: same_id=%v err=%v", newID == id, err)
			}
			row, err := testRepo.GetByID(ctx, id, storeID)
			if err != nil {
				t.Fatal(err)
			}
			creds, err := svc.decryptCredentials(row.Credentials)
			if err != nil || creds.AccessToken != "new-token" || row.Status != "active" {
				t.Fatalf("reconnect did not update credentials/status: status=%s err=%v", row.Status, err)
			}
			if row.Metadata["custom_setting"] != "current-database-value" || row.Metadata["reserva_capacidade_confirmada"] != true || row.Metadata["webhookLastPingAt"] == nil {
				t.Fatal("reconnection replaced current settings or webhook history")
			}
			resolved, err := testRepo.GetActiveERPByAccount(ctx, "bling", account)
			if err != nil || resolved.ID != id {
				t.Fatalf("reconnected account cannot receive webhooks: %v", err)
			}
		})
	}
}

func TestBlingOAuthAccountConflictLeavesOriginalConnectionUntouched(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	svc := reconnectTestService(t)
	storeID := seedStoreForReconnect(t)
	originalAccount := uuid.NewString()
	id := createBlingOAuthTestConnection(t, svc, storeID, originalAccount)
	if err := testRepo.UpdateStatus(ctx, id, "error"); err != nil {
		t.Fatal(err)
	}
	claimedAccount := uuid.NewString()
	otherID := createBlingOAuthTestConnection(t, svc, seedStoreForReconnect(t), claimedAccount)
	// A broken integration still owns its account according to the index.
	if err := testRepo.UpdateStatus(ctx, otherID, "error"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.upsertBlingIntegration(ctx, storeID, blingOAuthTestCredentials("must-not-be-saved"), blingOAuthTestIdentity(claimedAccount)); err == nil {
		t.Fatal("accepted account owned by another store")
	}
	row, err := testRepo.GetByID(ctx, id, storeID)
	if err != nil {
		t.Fatal(err)
	}
	creds, err := svc.decryptCredentials(row.Credentials)
	if err != nil || creds.AccessToken != "original-token" || row.Status != "error" || row.Metadata[providers.MetadataBlingCompanyID] != originalAccount {
		t.Fatalf("failed callback partially modified original connection: status=%s err=%v", row.Status, err)
	}
	var savedAccount string
	if err := testPool.QueryRow(ctx, "SELECT erp_account_id FROM integrations WHERE id=$1", id).Scan(&savedAccount); err != nil || savedAccount != originalAccount {
		t.Fatalf("routing identity changed despite failed callback: %v", err)
	}
}

func TestBlingOAuthRejectsChangingAccountsWithExistingLinks(t *testing.T) {
	requireDB(t)
	for _, linked := range []string{"product", "order", "contact"} {
		t.Run(linked, func(t *testing.T) {
			ctx := context.Background()
			svc := reconnectTestService(t)
			storeID := seedStoreForReconnect(t)
			account := uuid.NewString()
			id := createBlingOAuthTestConnection(t, svc, storeID, account)
			var err error
			switch linked {
			case "product":
				_, err = testPool.Exec(ctx, `INSERT INTO products (store_id, name, external_source, external_id, keyword, price, stock)
					VALUES ($1, 'Test linked product', 'bling', '1234', '1234', 100, 1)`, storeID)
			case "order":
				_, err = testPool.Exec(ctx, `WITH event AS (
					INSERT INTO live_events (store_id, status, title, ends_at)
					VALUES ($1, 'ended', 'Test live', now()) RETURNING id)
					INSERT INTO carts (event_id, store_id, platform_user_id, platform_handle, token, short_id, external_order_id)
					SELECT id, $1, 'test-user', '@test-user', $2, 12345, '1234' FROM event`, storeID, uuid.NewString())
			case "contact":
				err = testRepo.UpsertERPContact(ctx, storeID, id, "test-user", "test-user", "1234")
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err := svc.upsertBlingIntegration(ctx, storeID, blingOAuthTestCredentials("wrong-account"), blingOAuthTestIdentity(uuid.NewString())); err == nil {
				t.Fatal("account changed with existing ERP identifiers")
			}
			// Same-account recovery must remain available even with live links.
			if got, err := svc.upsertBlingIntegration(ctx, storeID, blingOAuthTestCredentials("same-account"), blingOAuthTestIdentity(account)); err != nil || got != id {
				t.Fatalf("safe reconnection was blocked: %v", err)
			}
		})
	}
}

func TestBlingOAuthUnlinkedAccountChangeResetsOldAccountMetadata(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	svc := reconnectTestService(t)
	storeID := seedStoreForReconnect(t)
	account := uuid.NewString()
	id := createBlingOAuthTestConnection(t, svc, storeID, account)
	metadata := blingOAuthTestIdentity(account)
	metadata["reserva_capacidade_confirmada"] = true
	if err := testRepo.UpdateMetadata(ctx, id, metadata); err != nil {
		t.Fatal(err)
	}
	newAccount := uuid.NewString()
	if got, err := svc.upsertBlingIntegration(ctx, storeID, blingOAuthTestCredentials("new-account"), blingOAuthTestIdentity(newAccount)); err != nil || got != id {
		t.Fatalf("unlinked account change failed: %v", err)
	}
	row, err := testRepo.GetByID(ctx, id, storeID)
	if err != nil {
		t.Fatal(err)
	}
	if row.Metadata["reserva_capacidade_confirmada"] != nil || row.Metadata[providers.MetadataBlingCompanyID] != newAccount {
		t.Fatal("new account inherited old account reservation proof")
	}
}

func TestBlingOAuthConcurrentFirstConnectionsReuseOneIntegration(t *testing.T) {
	requireDB(t)
	svc := reconnectTestService(t)
	storeID := seedStoreForReconnect(t)
	account := uuid.NewString()
	const callers = 8
	results := make(chan string, callers)
	for i := range callers {
		go func() {
			id, err := svc.upsertBlingIntegration(context.Background(), storeID,
				blingOAuthTestCredentials(fmt.Sprintf("token-%d", i)), blingOAuthTestIdentity(account))
			if err != nil {
				t.Error(err)
			}
			results <- id
		}()
	}
	first := <-results
	for i := 1; i < callers; i++ {
		if got := <-results; got == "" || got != first {
			t.Fatal("concurrent callbacks did not reuse the same integration")
		}
	}
}

func TestBlingOAuthRejectsMissingIdentityAndPropagatesLookupFailure(t *testing.T) {
	requireDB(t)
	svc := reconnectTestService(t)
	storeID := seedStoreForReconnect(t)
	if _, err := svc.upsertBlingIntegration(context.Background(), storeID, blingOAuthTestCredentials("token"), nil); err == nil {
		t.Fatal("created active integration without company identity")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := svc.checkBlingERPConnection(ctx, storeID); !errors.Is(err, context.Canceled) {
		t.Fatalf("ERP lookup failure was swallowed: %v", err)
	}
	if _, err := testRepo.consumeBlingOAuthState(ctx, "any-state"); !errors.Is(err, context.Canceled) {
		t.Fatalf("OAuth storage failure was misreported as expired state: %v", err)
	}
}

func TestBlingOAuthDoesNotReplaceERPCreatedAfterPreflight(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	svc := reconnectTestService(t)
	storeID := seedStoreForReconnect(t)
	if err := svc.checkBlingERPConnection(ctx, storeID); err != nil {
		t.Fatal(err)
	}
	encrypted, err := svc.encryptor.EncryptJSON(blingOAuthTestCredentials("tiny-token"))
	if err != nil {
		t.Fatal(err)
	}
	// Another tab connects a different ERP while Bling consent is in progress.
	other, err := testRepo.Create(ctx, CreateIntegrationParams{
		StoreID: storeID, Type: "erp", Provider: "tiny", Status: "error", Credentials: encrypted,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.upsertBlingIntegration(ctx, storeID, blingOAuthTestCredentials("bling-token"), blingOAuthTestIdentity(uuid.NewString())); err == nil {
		t.Fatal("callback replaced another ERP created after preflight")
	}
	row, err := testRepo.GetByID(ctx, other.ID, storeID)
	if err != nil {
		t.Fatal(err)
	}
	creds, err := svc.decryptCredentials(row.Credentials)
	if err != nil || row.Provider != "tiny" || row.Status != "error" || creds.AccessToken != "tiny-token" {
		t.Fatalf("another ERP was modified by the rejected callback: %v", err)
	}
}
