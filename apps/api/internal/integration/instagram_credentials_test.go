//go:build integration

package integration

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"
	"livecart/apps/api/internal/integration/providers"
)

type instagramRefreshTestProvider struct {
	providers.SocialProvider
	refresh func(context.Context) (*providers.Credentials, error)
}

func (p instagramRefreshTestProvider) RefreshToken(ctx context.Context) (*providers.Credentials, error) {
	return p.refresh(ctx)
}

// Execute the actual operator script's DO block, with transaction-local inputs.
func linkInstagramFixture(t *testing.T, sourceID, targetID, accountID string) error {
	t.Helper()
	script, err := os.ReadFile("../../../../docs/operations/link-shared-instagram.sql")
	if err != nil {
		t.Fatal(err)
	}
	text := string(script)
	block := text[strings.Index(text, "DO $$") : strings.Index(text, "END $$;")+len("END $$;")]
	ctx := t.Context()
	tx, err := testPool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	_, err = tx.Exec(ctx, `SELECT set_config('livecart.link_source_id',$1,true), set_config('livecart.link_target_store_id',$2,true), set_config('livecart.link_expected_account_id',$3,true)`, sourceID, targetID, accountID)
	if err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, block); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func seedInstagramCredentials(t *testing.T) (*Service, *IntegrationRow, *IntegrationRow, *providers.Credentials) {
	t.Helper()
	requireDB(t)
	svc := reconnectTestService(t)
	a, b := seedStoreForReconnect(t), seedStoreForReconnect(t)
	creds := &providers.Credentials{AccessToken: "local-test-token", ExpiresAt: time.Now().Add(10 * time.Minute), Extra: map[string]any{"instagram_user_id": "scoped-" + a}}
	encrypted, err := svc.encryptor.EncryptJSON(creds)
	if err != nil {
		t.Fatal(err)
	}
	owner, err := testRepo.Create(t.Context(), CreateIntegrationParams{StoreID: a, Type: "social", Provider: "instagram", Status: "active", Credentials: encrypted, TokenExpiresAt: &creds.ExpiresAt, Metadata: map[string]any{"instagram_user_id": a, "instagram_app_scoped_id": "scoped-" + a, "username": "demo"}})
	if err != nil {
		t.Fatal(err)
	}
	// Script requires at least 30 minutes of validity when initially linking.
	if _, err := testPool.Exec(t.Context(), `UPDATE integrations SET token_expires_at=now()+interval '1 hour' WHERE id=$1`, owner.ID); err != nil {
		t.Fatal(err)
	}
	if err := linkInstagramFixture(t, owner.ID, b, a); err != nil {
		t.Fatal(err)
	}
	alias, err := testRepo.GetAnyByType(t.Context(), b, "social")
	if err != nil {
		t.Fatal(err)
	}
	return svc, owner, alias, creds
}

func TestInstagramSharedCredentialsLifecycle(t *testing.T) {
	svc, owner, alias, old := seedInstagramCredentials(t)
	ctx := t.Context()
	if alias.ID == owner.ID || alias.StoreID == owner.StoreID || alias.InstagramCredentialsSourceID != owner.ID || alias.Metadata["username"] != "demo" {
		t.Fatal("alias lost local identity or source identity")
	}
	var copies int
	if err := testPool.QueryRow(ctx, `SELECT count(*) FROM integrations WHERE id IN ($1,$2) AND credentials IS NOT NULL`, owner.ID, alias.ID).Scan(&copies); err != nil || copies != 1 {
		t.Fatalf("credential copies=%d err=%v", copies, err)
	}
	if err := linkInstagramFixture(t, owner.ID, alias.StoreID, owner.StoreID); err != nil {
		t.Fatalf("idempotent link: %v", err)
	}
	if err := linkInstagramFixture(t, owner.ID, seedStoreForReconnect(t), "wrong-account"); err == nil {
		t.Fatal("accepted wrong Instagram")
	}
	for _, id := range []string{owner.StoreID, "scoped-" + owner.StoreID} {
		stores, err := testRepo.ListInstagramStoreIDs(ctx, id)
		if err != nil || len(stores) != 2 {
			t.Fatalf("owners=%v err=%v", stores, err)
		}
	}
	if err := testRepo.Delete(ctx, owner.ID, owner.StoreID); err == nil || !strings.Contains(err.Error(), "outras lojas") {
		t.Fatalf("root deletion must explain dependency: %v", err)
	}
	if _, err := svc.GetOAuthURL(ctx, GetOAuthURLInput{StoreID: alias.StoreID, Provider: "instagram"}); err == nil || !strings.Contains(err.Error(), "compartilhado") {
		t.Fatalf("alias OAuth was not blocked: %v", err)
	}
	if _, err := svc.Create(ctx, CreateIntegrationInput{StoreID: alias.StoreID, Type: "social", Provider: "instagram"}); err == nil {
		t.Fatal("created independent credentials in alias store")
	}
	workers, err := testRepo.ListWithExpiringTokens(ctx, time.Now().Add(2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	var ownerJobs, aliasJobs int
	for _, row := range workers {
		if row.ID == owner.ID {
			ownerJobs++
		}
		if row.ID == alias.ID {
			aliasJobs++
		}
	}
	if ownerJobs != 1 || aliasJobs != 0 {
		t.Fatalf("worker scheduled owner=%d alias=%d", ownerJobs, aliasJobs)
	}
	if err := testRepo.UpdateStatus(ctx, owner.ID, "error"); err != nil {
		t.Fatal(err)
	}
	resolved, err := testRepo.GetByID(ctx, alias.ID, alias.StoreID)
	if err != nil || resolved.Status != "error" {
		t.Fatalf("source failure not visible: %v", err)
	}
	if _, err := svc.createProviderFromRow(ctx, alias); err == nil {
		t.Fatal("stale alias used disconnected source")
	}
	stores, err := testRepo.ListInstagramStoreIDs(ctx, owner.StoreID)
	if err != nil || len(stores) != 0 {
		t.Fatalf("disconnected source still receives DMs: %v %v", stores, err)
	}
	newCreds := *old
	newCreds.AccessToken = "reauthorized-token"
	newCreds.ExpiresAt = time.Now().Add(60 * 24 * time.Hour)
	encrypted, err := svc.encryptor.EncryptJSON(newCreds)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.saveInstagramAuthorization(ctx, owner, encrypted, newCreds.ExpiresAt, map[string]any{"instagram_user_id": "another-account"}); err == nil {
		t.Fatal("shared account switched silently")
	}
	if err := svc.saveInstagramAuthorization(ctx, owner, encrypted, newCreds.ExpiresAt, map[string]any{"instagram_user_id": owner.StoreID, "username": "renamed-demo"}); err != nil {
		t.Fatal(err)
	}
	resolved, err = testRepo.GetByID(ctx, alias.ID, alias.StoreID)
	if err != nil {
		t.Fatal(err)
	}
	saved, err := svc.decryptCredentials(resolved.Credentials)
	if err != nil || saved.AccessToken != newCreds.AccessToken || resolved.Status != "active" || resolved.Metadata["username"] != "renamed-demo" {
		t.Fatalf("reconnect not shared: %v", err)
	}
	if err := testRepo.Delete(ctx, alias.ID, alias.StoreID); err != nil {
		t.Fatal(err)
	}
	if _, err := testRepo.GetByID(ctx, owner.ID, owner.StoreID); err != nil {
		t.Fatal("disconnecting alias affected root", err)
	}
}

func TestInstagramSharedRefreshSerializesInstances(t *testing.T) {
	svc, owner, alias, old := seedInstagramCredentials(t)
	var calls atomic.Int32
	factory := providers.NewFactory(providers.FactoryConfig{Logger: zap.NewNop(), InstagramConstructor: func(cfg providers.InstagramConfig) (providers.SocialProvider, error) {
		if cfg.IntegrationID != owner.ID || cfg.StoreID != owner.StoreID {
			t.Error("refresh not using canonical owner")
		}
		return instagramRefreshTestProvider{refresh: func(context.Context) (*providers.Credentials, error) {
			if calls.Add(1) != 1 {
				return nil, errors.New("duplicate refresh")
			}
			return &providers.Credentials{AccessToken: "shared-refreshed-token", ExpiresAt: time.Now().Add(60 * 24 * time.Hour), Extra: old.Extra}, nil
		}}, nil
	}})
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	const n = 8
	results := make(chan error, n)
	for i := range n {
		row := owner
		if i%2 == 1 {
			row = alias
		}
		other := &Service{repo: NewRepository(testRepo.queries, testPool), factory: factory, encryptor: svc.encryptor, logger: zap.NewNop()}
		go func() {
			creds, err := other.refreshToken(ctx, row, old)
			if err == nil && (creds == nil || creds.AccessToken != "shared-refreshed-token") {
				err = errors.New("caller received stale token")
			}
			results <- err
		}()
	}
	for range n {
		if err := <-results; err != nil {
			t.Error(err)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("refresh calls=%d", calls.Load())
	}
	// Normal operations still use the selling store's IDs, with the shared token.
	svc.factory = providers.NewFactory(providers.FactoryConfig{Logger: zap.NewNop(), InstagramConstructor: func(cfg providers.InstagramConfig) (providers.SocialProvider, error) {
		if cfg.IntegrationID != alias.ID || cfg.StoreID != alias.StoreID || cfg.Credentials.AccessToken != "shared-refreshed-token" {
			t.Error("provider attribution or token incorrect")
		}
		return instagramRefreshTestProvider{}, nil
	}})
	if _, err := svc.createProviderFromRow(ctx, alias); err != nil {
		t.Fatal(err)
	}
}

func TestInstagramRefreshFailureClassification(t *testing.T) {
	for _, tc := range []struct {
		name    string
		failure error
		status  string
	}{
		{"network", errors.New("network failure"), "active"},
		{"quota", &refreshTestError{}, "active"},
		{"cancelled", context.Canceled, "active"},
		{"revoked", &refreshTestError{permanent: true}, "error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, owner, alias, old := seedInstagramCredentials(t)
			svc.factory = providers.NewFactory(providers.FactoryConfig{Logger: zap.NewNop(), InstagramConstructor: func(providers.InstagramConfig) (providers.SocialProvider, error) {
				return instagramRefreshTestProvider{refresh: func(context.Context) (*providers.Credentials, error) { return nil, tc.failure }}, nil
			}})
			if _, err := svc.refreshToken(t.Context(), alias, old); !errors.Is(err, tc.failure) {
				t.Fatalf("lost cause: %v", err)
			}
			for _, row := range []*IntegrationRow{owner, alias} {
				current, err := testRepo.GetByID(t.Context(), row.ID, row.StoreID)
				if err != nil || current.Status != tc.status {
					t.Fatalf("status not shared: %v %v", current, err)
				}
			}
		})
	}
}

func TestInstagramMetadataCannotGrantSharedCredentials(t *testing.T) {
	svc, owner, _, _ := seedInstagramCredentials(t)
	storeID := seedStoreForReconnect(t)
	_, err := svc.Create(t.Context(), CreateIntegrationInput{StoreID: storeID, Type: "social", Provider: "instagram", Credentials: &providers.Credentials{AccessToken: "own-token"}, Metadata: map[string]any{"instagram_credentials_source_id": owner.ID}})
	if err != nil {
		t.Fatal(err)
	}
	row, err := testRepo.GetAnyByType(t.Context(), storeID, "social")
	if err != nil {
		t.Fatal(err)
	}
	creds, err := svc.decryptCredentials(row.Credentials)
	if err != nil || creds.AccessToken != "own-token" || row.InstagramCredentialsSourceID != "" {
		t.Fatalf("metadata granted cross-store credentials: %v", err)
	}
}

func TestInstagramRefreshPersistenceFailureIsNotSuccess(t *testing.T) {
	svc, owner, alias, old := seedInstagramCredentials(t)
	ctx := t.Context()
	constraint := "reject_ig_refresh_" + strings.ReplaceAll(owner.ID, "-", "")
	ddl := fmt.Sprintf(`ALTER TABLE integrations ADD CONSTRAINT %s CHECK (id <> '%s'::uuid OR credentials=decode('%x','hex'))`, constraint, owner.ID, owner.Credentials)
	if _, err := testPool.Exec(ctx, ddl); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := testPool.Exec(context.Background(), "ALTER TABLE integrations DROP CONSTRAINT "+constraint); err != nil {
			t.Error(err)
		}
	})
	svc.factory = providers.NewFactory(providers.FactoryConfig{Logger: zap.NewNop(), InstagramConstructor: func(providers.InstagramConfig) (providers.SocialProvider, error) {
		return instagramRefreshTestProvider{refresh: func(context.Context) (*providers.Credentials, error) {
			return &providers.Credentials{AccessToken: "new-token", ExpiresAt: time.Now().Add(60 * 24 * time.Hour)}, nil
		}}, nil
	}})
	if _, err := svc.refreshToken(ctx, alias, old); err == nil {
		t.Fatal("reported refresh success despite persistence failure")
	}
	current, err := testRepo.GetByID(ctx, alias.ID, alias.StoreID)
	if err != nil {
		t.Fatal(err)
	}
	creds, err := svc.decryptCredentials(current.Credentials)
	if err != nil || creds.AccessToken != old.AccessToken {
		t.Fatalf("failed transaction changed alias credentials: %v", err)
	}
}

func TestInstagramLinkRejectsConflictsAndInvalidSources(t *testing.T) {
	_, owner, alias, _ := seedInstagramCredentials(t)
	ctx := t.Context()
	other := seedStoreForReconnect(t)
	if err := linkInstagramFixture(t, alias.ID, other, owner.StoreID); err == nil {
		t.Fatal("linked a chain of aliases")
	}
	if err := linkInstagramFixture(t, owner.ID, owner.StoreID, owner.StoreID); err == nil {
		t.Fatal("linked owner to itself")
	}
	seedSharedInstagram(t, other, "another-account", "another-scoped-id", "active")
	if err := linkInstagramFixture(t, owner.ID, other, owner.StoreID); err == nil {
		t.Fatal("overwrote an independent integration")
	}
	if _, err := testPool.Exec(ctx, `UPDATE integrations SET credentials=$2 WHERE id=$1`, alias.ID, []byte("independent-token")); err == nil {
		t.Fatal("alias stored a token copy")
	}
	// A manually corrupted source reference must fail closed at runtime too.
	if _, err := testPool.Exec(ctx, `UPDATE integrations SET instagram_credentials_source_id=$2 WHERE id=$1`, alias.ID, other); err == nil {
		t.Fatal("accepted missing source (FK)")
	}
	third := seedStoreForReconnect(t)
	if _, err := testPool.Exec(ctx, `INSERT INTO integrations(store_id,type,provider,status,instagram_credentials_source_id) VALUES($1,'social','instagram','active',$2)`, third, alias.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := testRepo.GetAnyByType(ctx, third, "social"); err == nil {
		t.Fatal("resolved a chain of aliases")
	}
}
