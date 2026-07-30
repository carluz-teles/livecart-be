package erp

import (
	"context"
	"errors"
	"testing"
	"time"

	"livecart/apps/api/internal/integration/providers"
)

// fakeInvoiceProvider satisfies providers.ERPInvoiceProvider (embedding the ERP
// provider interface for the methods the sync never calls). It records which
// fetch path the sync took so the id-vs-order branch can be asserted.
type fakeInvoiceProvider struct {
	providers.ERPProvider
	invoice *providers.ERPInvoice
	err     error
	byID    int
	byOrder int
}

func (f *fakeInvoiceProvider) GetInvoiceByID(context.Context, string) (*providers.ERPInvoice, error) {
	f.byID++
	return f.invoice, f.err
}
func (f *fakeInvoiceProvider) GetInvoiceByOrder(context.Context, string) (*providers.ERPInvoice, error) {
	f.byOrder++
	return f.invoice, f.err
}
func (f *fakeInvoiceProvider) GetInvoiceXML(context.Context, string) ([]byte, error) {
	return nil, nil
}

func TestService_SyncCartInvoiceFromERP(t *testing.T) {
	ctx := context.Background()

	t.Run("with invoiceID fetches by id and upserts", func(t *testing.T) {
		fake := &fakeInvoiceProvider{invoice: &providers.ERPInvoice{
			InvoiceID: "nf-1",
			AccessKey: "chave-1",
			Status:    providers.ERPInvoiceStatusAuthorized,
			IssuedAt:  time.Now(),
		}}
		repo := &mockRepo{anchorStoreID: "s", anchorExtOrder: "ORD-1", upsertRows: 1}
		collab := &mockCollab{provider: fake}
		if err := newSvc(repo, collab).SyncCartInvoiceFromERP(ctx, "s", "c", "nf-1"); err != nil {
			t.Fatalf("want nil, got %v", err)
		}
		if fake.byID != 1 || fake.byOrder != 0 {
			t.Fatalf("expected fetch by id, got byID=%d byOrder=%d", fake.byID, fake.byOrder)
		}
		if repo.upserts != 1 {
			t.Fatalf("expected one upsert, got %d", repo.upserts)
		}
		// AccessKey present but no shipment → no mirror.
		if repo.invoiceMirrors != 0 {
			t.Fatalf("no shipment means no mirror, got %d", repo.invoiceMirrors)
		}
	})

	t.Run("without invoiceID fetches by external order id", func(t *testing.T) {
		fake := &fakeInvoiceProvider{invoice: &providers.ERPInvoice{InvoiceID: "nf-2"}}
		repo := &mockRepo{anchorStoreID: "s", anchorExtOrder: "ORD-2", upsertRows: 1}
		collab := &mockCollab{provider: fake}
		if err := newSvc(repo, collab).SyncCartInvoiceFromERP(ctx, "s", "c", ""); err != nil {
			t.Fatalf("want nil, got %v", err)
		}
		if fake.byOrder != 1 || fake.byID != 0 {
			t.Fatalf("expected fetch by order, got byID=%d byOrder=%d", fake.byID, fake.byOrder)
		}
	})

	t.Run("invoice not found yet persists nothing", func(t *testing.T) {
		fake := &fakeInvoiceProvider{err: providers.ErrInvoiceNotFound}
		repo := &mockRepo{anchorStoreID: "s", anchorExtOrder: "ORD-3"}
		collab := &mockCollab{provider: fake}
		if err := newSvc(repo, collab).SyncCartInvoiceFromERP(ctx, "s", "c", ""); err != nil {
			t.Fatalf("aguardando-NFe must be a nil error, got %v", err)
		}
		if repo.upserts != 0 || collab.handledErrors != 0 {
			t.Fatalf("not-found must not write nor flag a provider error: upserts=%d handled=%d", repo.upserts, collab.handledErrors)
		}
	})

	t.Run("provider fetch error flags telemetry and propagates", func(t *testing.T) {
		fake := &fakeInvoiceProvider{err: errors.New("tiny down")}
		repo := &mockRepo{anchorStoreID: "s", anchorExtOrder: "ORD-4"}
		collab := &mockCollab{provider: fake}
		if err := newSvc(repo, collab).SyncCartInvoiceFromERP(ctx, "s", "c", ""); err == nil {
			t.Fatal("expected the fetch error to propagate")
		}
		if collab.handledErrors != 1 {
			t.Fatalf("fetch failure must flag provider error once, got %d", collab.handledErrors)
		}
		if repo.upserts != 0 {
			t.Fatalf("failed fetch must not upsert, got %d", repo.upserts)
		}
	})

	t.Run("zero rows written is a benign skip, not an error", func(t *testing.T) {
		fake := &fakeInvoiceProvider{invoice: &providers.ERPInvoice{InvoiceID: "nf-5"}}
		repo := &mockRepo{anchorStoreID: "s", anchorExtOrder: "ORD-5", upsertRows: 0}
		collab := &mockCollab{provider: fake}
		if err := newSvc(repo, collab).SyncCartInvoiceFromERP(ctx, "s", "c", ""); err != nil {
			t.Fatalf("0 rows (no materialised order) must not error, got %v", err)
		}
		if repo.upserts != 1 {
			t.Fatalf("still attempts the upsert, got %d", repo.upserts)
		}
	})

	t.Run("access key mirrors onto an existing keyless shipment", func(t *testing.T) {
		fake := &fakeInvoiceProvider{invoice: &providers.ERPInvoice{InvoiceID: "nf-6", AccessKey: "chave-6"}}
		repo := &mockRepo{
			anchorStoreID:  "s",
			anchorExtOrder: "ORD-6",
			upsertRows:     1,
			shipment:       &ShipmentInvoiceRef{ID: "sh-1", InvoiceKey: ""},
		}
		collab := &mockCollab{provider: fake}
		if err := newSvc(repo, collab).SyncCartInvoiceFromERP(ctx, "s", "c", ""); err != nil {
			t.Fatalf("want nil, got %v", err)
		}
		if repo.invoiceMirrors != 1 {
			t.Fatalf("expected the chave to mirror onto the shipment once, got %d", repo.invoiceMirrors)
		}
	})

	t.Run("shipment that already has a key is left untouched", func(t *testing.T) {
		fake := &fakeInvoiceProvider{invoice: &providers.ERPInvoice{InvoiceID: "nf-7", AccessKey: "chave-7"}}
		repo := &mockRepo{
			anchorStoreID:  "s",
			anchorExtOrder: "ORD-7",
			upsertRows:     1,
			shipment:       &ShipmentInvoiceRef{ID: "sh-2", InvoiceKey: "already-there"},
		}
		collab := &mockCollab{provider: fake}
		if err := newSvc(repo, collab).SyncCartInvoiceFromERP(ctx, "s", "c", ""); err != nil {
			t.Fatalf("want nil, got %v", err)
		}
		if repo.invoiceMirrors != 0 {
			t.Fatalf("existing key must not be overwritten, got %d", repo.invoiceMirrors)
		}
	})

	t.Run("store mismatch is a not-found", func(t *testing.T) {
		repo := &mockRepo{anchorStoreID: "other-store", anchorExtOrder: "ORD-8"}
		collab := &mockCollab{provider: &fakeInvoiceProvider{}}
		err := newSvc(repo, collab).SyncCartInvoiceFromERP(ctx, "s", "c", "")
		if err == nil {
			t.Fatal("expected a not-found for a cart in another store")
		}
		if repo.upserts != 0 {
			t.Fatalf("mismatch must short-circuit before any fetch/upsert, got %d", repo.upserts)
		}
	})

	t.Run("no ERP order id and no invoice id is a silent skip", func(t *testing.T) {
		repo := &mockRepo{anchorStoreID: "s", anchorExtOrder: ""}
		collab := &mockCollab{provider: &fakeInvoiceProvider{}}
		if err := newSvc(repo, collab).SyncCartInvoiceFromERP(ctx, "s", "c", ""); err != nil {
			t.Fatalf("aguardando-criação-no-ERP must be a nil error, got %v", err)
		}
		if repo.upserts != 0 {
			t.Fatalf("nothing to fetch means nothing to write, got %d", repo.upserts)
		}
	})
}

func TestService_SyncCartInvoiceByExternalOrder(t *testing.T) {
	ctx := context.Background()

	t.Run("resolves the cart then syncs", func(t *testing.T) {
		fake := &fakeInvoiceProvider{invoice: &providers.ERPInvoice{InvoiceID: "nf-9"}}
		repo := &mockRepo{
			findCartID:     "c",
			anchorStoreID:  "s",
			anchorExtOrder: "ORD-9",
			upsertRows:     1,
		}
		collab := &mockCollab{provider: fake}
		state, err := newSvc(repo, collab).SyncCartInvoiceByExternalOrder(ctx, "s", "ORD-9", "nf-9")
		if err != nil {
			t.Fatalf("want nil, got %v", err)
		}
		if state == nil || state.InvoiceID != "nf-9" {
			t.Fatalf("expected the synced state to surface, got %+v", state)
		}
	})

	t.Run("unknown pedido is skipped without error", func(t *testing.T) {
		repo := &mockRepo{findErr: errors.New("no such cart")}
		collab := &mockCollab{provider: &fakeInvoiceProvider{}}
		state, err := newSvc(repo, collab).SyncCartInvoiceByExternalOrder(ctx, "s", "ORD-X", "")
		if err != nil || state != nil {
			t.Fatalf("unknown pedido must be a nil,nil skip, got state=%+v err=%v", state, err)
		}
		if repo.upserts != 0 {
			t.Fatalf("unknown pedido must not reach the upsert, got %d", repo.upserts)
		}
	})
}
