package integration

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
	"github.com/hibiken/asynq"
	"go.uber.org/zap"

	"livecart/apps/api/internal/erp"
	"livecart/apps/api/internal/integration/providers"
	"livecart/apps/api/lib/httpx"
)

func TestBlingWebhookAcknowledgesOnlyAuthenticatedDurableEvents(t *testing.T) {
	for _, tc := range []struct {
		name      string
		secret    string
		header    string
		body      string
		lookupErr error
		emitErr   error
		status    int
		lookups   int
		emits     int
	}{
		{"durable event", segredoDoApp, "valid", envelopeReal, nil, nil, 200, 1, 1},
		{"missing signature", segredoDoApp, "", envelopeReal, nil, nil, 401, 0, 0},
		{"wrong signature", segredoDoApp, "sha256=" + strings.Repeat("0", 64), envelopeReal, nil, nil, 401, 1, 0},
		{"unconfigured secret", "", "valid", envelopeReal, nil, nil, 503, 1, 0},
		{"invalid json", segredoDoApp, "valid", "{", nil, nil, 400, 0, 0},
		{"missing owner", segredoDoApp, "valid", `{"eventId":"a","event":"order.updated","data":{"id":1}}`, nil, nil, 400, 0, 0},
		{"unknown account", segredoDoApp, "valid", envelopeReal, httpx.ErrNotFound("integration"), nil, 200, 1, 0},
		{"database lookup outage", segredoDoApp, "valid", envelopeReal, errors.New("database unavailable"), nil, 503, 1, 0},
		{"outbox unavailable", segredoDoApp, "valid", envelopeReal, nil, errors.New("insert failed"), 503, 1, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lookups, emits := 0, 0
			app := fiber.New()
			app.Post("/", func(c *fiber.Ctx) error {
				return handleBlingWebhook(c, tc.secret, func(ctx context.Context, provider, account string) (*IntegrationRow, error) {
					lookups++
					if provider != "bling" || account != "9db3b9e60022d0eddb121a4319dfbe15" {
						t.Error("incorrect account lookup")
					}
					deadline, ok := ctx.Deadline()
					if !ok || time.Until(deadline) > 4*time.Second {
						t.Error("missing ingress time budget")
					}
					return &IntegrationRow{ID: "integration", StoreID: "store"}, tc.lookupErr
				}, func(*IntegrationRow) (string, error) { return tc.secret, nil }, func(ctx context.Context, integration *IntegrationRow, env BlingEnvelope) error {
					emits++
					if integration.ID != "integration" || env.EventID == "" {
						t.Error("lost ownership or event identity")
					}
					return tc.emitErr
				}, zap.NewNop())
			})
			req := httptest.NewRequest("POST", "/", strings.NewReader(tc.body))
			header := tc.header
			if header == "valid" {
				header = assinarBling(t, tc.body, segredoDoApp)
			}
			req.Header.Set(blingSignatureHeader, header)
			response, err := app.Test(req)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			if response.StatusCode != tc.status || lookups != tc.lookups || emits != tc.emits {
				t.Fatalf("status/lookups/emits = %d/%d/%d, want %d/%d/%d", response.StatusCode, lookups, emits, tc.status, tc.lookups, tc.emits)
			}
		})
	}
}

func TestBlingWebhookAuthenticatesTheOwningApplication(t *testing.T) {
	const sharedSecret = "shared-application-secret"
	const privateSecret = "private-application-secret"
	for _, tc := range []struct {
		name               string
		globalSecret       string
		signWith           string
		extra              map[string]any
		lookupErr          error
		corruptCredentials bool
		status             int
	}{
		{"shared app", sharedSecret, sharedSecret, nil, nil, false, 200},
		{"private app", sharedSecret, privateSecret, map[string]any{"client_id": "private-app", "client_secret": privateSecret}, nil, false, 200},
		{"private without global config", "", privateSecret, map[string]any{"client_id": "private-app", "client_secret": privateSecret}, nil, false, 200},
		{"shared signature cannot claim private integration", sharedSecret, sharedSecret, map[string]any{"client_id": "private-app", "client_secret": privateSecret}, nil, false, 401},
		{"other private app cannot claim integration", sharedSecret, "another-private-secret", map[string]any{"client_id": "private-app", "client_secret": privateSecret}, nil, false, 401},
		{"private client missing secret", sharedSecret, sharedSecret, map[string]any{"client_id": "private-app"}, nil, false, 503},
		{"private app lookup failure", sharedSecret, privateSecret, nil, errors.New("database unavailable"), false, 503},
		{"unknown private account", sharedSecret, privateSecret, nil, httpx.ErrNotFound("integration"), false, 401},
		{"corrupt encrypted secret", sharedSecret, privateSecret, nil, nil, true, 503},
		{"shared app missing config", "", privateSecret, nil, nil, false, 503},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("BLING_CLIENT_ID", "shared-app")
			t.Setenv("BLING_CLIENT_SECRET", tc.globalSecret)
			svc := resilienceTestService(t)
			encrypted, err := svc.encryptor.EncryptJSON(providers.Credentials{AccessToken: "unused", Extra: tc.extra})
			if err != nil {
				t.Fatal(err)
			}
			if tc.corruptCredentials {
				encrypted = []byte("invalid encrypted credentials")
			}
			owner := &IntegrationRow{ID: "owning-integration", StoreID: "store", Provider: "bling", Credentials: encrypted}
			emits := 0
			app := fiber.New()
			app.Post("/", func(c *fiber.Ctx) error {
				return handleBlingWebhook(c, tc.globalSecret, func(context.Context, string, string) (*IntegrationRow, error) {
					return owner, tc.lookupErr
				}, svc.blingWebhookSecret, func(_ context.Context, integration *IntegrationRow, _ BlingEnvelope) error {
					if integration != owner {
						t.Error("persisted different integration")
					}
					emits++
					return nil
				}, zap.NewNop())
			})
			req := httptest.NewRequest("POST", "/", strings.NewReader(envelopeReal))
			req.Header.Set(blingSignatureHeader, assinarBling(t, envelopeReal, tc.signWith))
			response, err := app.Test(req)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			if response.StatusCode != tc.status {
				t.Fatalf("status = %d, want %d", response.StatusCode, tc.status)
			}
			wantEmits := 0
			if tc.status == 200 {
				wantEmits = 1
			}
			if emits != wantEmits {
				t.Fatalf("persisted %d commands, want %d", emits, wantEmits)
			}
		})
	}
}

func seedBlingWebhookIntegration(t *testing.T) (*Service, *IntegrationRow, string) {
	t.Helper()
	requireDB(t)
	svc := resilienceTestService(t)
	storeID := seedStoreForReconnect(t)
	account := uuid.NewString()
	var id string
	if err := testPool.QueryRow(context.Background(), `INSERT INTO integrations
		(store_id, type, provider, status, erp_account_id, metadata)
		VALUES ($1, 'erp', 'bling', 'active', $2, '{"unrelated":"preserved"}') RETURNING id::text`, storeID, account).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return svc, &IntegrationRow{ID: id, StoreID: storeID, Provider: "bling", Type: "erp", Status: "active"}, account
}

func TestBlingWebhookOutboxDeduplicatesAndSurvivesServiceRecreation(t *testing.T) {
	svc, integration, account := seedBlingWebhookIntegration(t)
	ctx := context.Background()
	env := BlingEnvelope{EventID: uuid.NewString(), CompanyID: account, Event: "invoice.updated", Data: json.RawMessage(`{"id":123}`)}
	for range 2 {
		if err := svc.enqueueBlingWebhook(ctx, integration, env); err != nil {
			t.Fatal(err)
		}
	}
	var count int
	if err := testPool.QueryRow(ctx, `SELECT count(*) FROM event_outbox WHERE dedup_key=$1`, "bling.webhook:"+integration.ID+":"+env.EventID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("duplicate delivery persisted %d commands", count)
	}
	var payload []byte
	if err := testPool.QueryRow(ctx, `SELECT payload FROM event_outbox WHERE dedup_key=$1`, "bling.webhook:"+integration.ID+":"+env.EventID).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	var command BlingWebhookCommand
	if err := json.Unmarshal(payload, &command); err != nil {
		t.Fatal(err)
	}
	// An invoice ID must never trigger a sales-order request, even after restart.
	if err := resilienceTestService(t).ProcessBlingWebhook(ctx, command); err != nil {
		t.Fatal(err)
	}
	command.Envelope.Event = "order.updated"
	// Not our cart: ownership lookup stops before constructing an ERP client.
	if err := svc.ProcessBlingWebhook(ctx, command); err != nil {
		t.Fatal(err)
	}
}

func TestBlingWebhookDeletionPreservesReferencesAndOtherStores(t *testing.T) {
	svc, integration, account := seedBlingWebhookIntegration(t)
	ctx := context.Background()
	otherStore := seedStoreForReconnect(t)
	for _, store := range []string{integration.StoreID, otherStore} {
		if _, err := testPool.Exec(ctx, `INSERT INTO products (store_id, name, keyword, price, stock, active, external_source, external_id)
			VALUES ($1, 'test', '1001', 100, 5, true, 'bling', '123')`, store); err != nil {
			t.Fatal(err)
		}
	}
	command := BlingWebhookCommand{StoreID: integration.StoreID, IntegrationID: integration.ID,
		Envelope: BlingEnvelope{CompanyID: account, EventID: uuid.NewString(), Event: "product.deleted", Data: json.RawMessage(`{"id":123}`)}}
	for range 2 {
		if err := svc.ProcessBlingWebhook(ctx, command); err != nil {
			t.Fatal(err)
		}
	}
	for _, store := range []string{integration.StoreID, otherStore} {
		var active bool
		var stock, seq int
		if err := testPool.QueryRow(ctx, `SELECT active, stock, erp_seq FROM products WHERE store_id=$1 AND external_id='123'`, store).Scan(&active, &stock, &seq); err != nil {
			t.Fatal(err)
		}
		if store == integration.StoreID {
			if active || stock != 0 || seq != 1 {
				t.Fatalf("deleted product = %v/%d/%d", active, stock, seq)
			}
		} else if !active || stock != 5 || seq != 0 {
			t.Fatal("changed another store")
		}
	}
	if err := svc.repo.recordBlingReservationCapability(ctx, integration.ID); err != nil {
		t.Fatal(err)
	}
	var retained string
	var capability bool
	if err := testPool.QueryRow(ctx, `SELECT metadata->>'unrelated', (metadata->>$2)::bool FROM integrations WHERE id=$1`, integration.ID, erp.ChaveCapacidadeDeReserva).Scan(&retained, &capability); err != nil {
		t.Fatal(err)
	}
	if retained != "preserved" || !capability {
		t.Fatal("metadata merge lost settings")
	}
}

func TestBlingWebhookWorkerRejectsInvalidCommand(t *testing.T) {
	svc := &Service{logger: zap.NewNop()}
	if err := svc.ProcessBlingWebhook(context.Background(), BlingWebhookCommand{}); !errors.Is(err, asynq.SkipRetry) {
		t.Fatalf("invalid command should enter dead letter without retries: %v", err)
	}
}

func TestBlingWebhookWorkerRetriesDatabaseFailureAndChecksOwnerAgain(t *testing.T) {
	svc, integration, account := seedBlingWebhookIntegration(t)
	command := BlingWebhookCommand{StoreID: integration.StoreID, IntegrationID: integration.ID,
		Envelope: BlingEnvelope{CompanyID: account, EventID: uuid.NewString(), Event: "product.deleted", Data: json.RawMessage(`{"id":123}`)}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := svc.ProcessBlingWebhook(ctx, command)
	if err == nil || errors.Is(err, asynq.SkipRetry) {
		t.Fatalf("database read failure must reach queue for retry: %v", err)
	}
	// Stale/replaced integration ownership must be checked before decoding or
	// executing the old event, even when its company is still connected.
	command.IntegrationID = uuid.NewString()
	command.Envelope.Data = json.RawMessage(`{"id":null}`)
	if err := svc.ProcessBlingWebhook(context.Background(), command); err != nil {
		t.Fatalf("old integration event was processed: %v", err)
	}
}
