package live

// Y is configurable from zero to thirty days. API validation and the
// defensive repository clamp must agree with the database constraint.

import (
	"context"
	"testing"
	"time"
)

func TestWaitlistNotifiedTTLRoundTrips(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	storeID := seedWindowStore(t, ctx, "ttl-roundtrip")
	svc := newWindowService(&fakeCloseScheduler{})

	ends := time.Now().UTC().Add(24 * time.Hour)
	ttl := 120
	out, err := svc.Create(ctx, CreateLiveInput{
		StoreID: storeID, Title: "Com TTL", Type: "multi", EndsAt: &ends,
		WaitlistNotifiedTTLMinutes: &ttl,
	})
	if err != nil {
		t.Fatalf("criar evento: %v", err)
	}

	got, err := svc.GetByID(ctx, out.ID, storeID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.WaitlistNotifiedTTLMinutes != 120 {
		t.Errorf("TTL na leitura = %d, queria 120", got.WaitlistNotifiedTTLMinutes)
	}

	// Edição: é o caminho que não existia (o PUT só trocava título e PIX).
	newTTL := 45
	if _, err := svc.Update(ctx, UpdateLiveInput{
		ID: out.ID, StoreID: storeID, Title: "Com TTL",
		WaitlistNotifiedTTLMinutes: &newTTL,
	}); err != nil {
		t.Fatalf("editar TTL: %v", err)
	}
	got, err = svc.GetByID(ctx, out.ID, storeID)
	if err != nil {
		t.Fatalf("GetByID pós-edição: %v", err)
	}
	if got.WaitlistNotifiedTTLMinutes != 45 {
		t.Errorf("TTL após edição = %d, queria 45", got.WaitlistNotifiedTTLMinutes)
	}
}

func TestWaitlistNotifiedTTLDefaultsAndClamps(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	storeID := seedWindowStore(t, ctx, "ttl-clamp")
	svc := newWindowService(&fakeCloseScheduler{})
	ends := time.Now().UTC().Add(24 * time.Hour)

	// Sem valor informado: fica o default da coluna (30, migration 000073).
	out, err := svc.Create(ctx, CreateLiveInput{
		StoreID: storeID, Title: "Sem TTL", Type: "multi", EndsAt: &ends,
	})
	if err != nil {
		t.Fatalf("criar evento: %v", err)
	}
	got, err := svc.GetByID(ctx, out.ID, storeID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.WaitlistNotifiedTTLMinutes != 30 {
		t.Errorf("default = %d, queria 30 (000073)", got.WaitlistNotifiedTTLMinutes)
	}

	// Boundary values round-trip; internal callers outside the range are clamped.
	for _, tc := range []struct{ in, want int }{{-1, 0}, {0, 0}, {1, 1}, {43200, 43200}, {43201, 43200}} {
		ttl := tc.in
		if _, err := svc.Update(ctx, UpdateLiveInput{
			ID: out.ID, StoreID: storeID, Title: "Sem TTL",
			WaitlistNotifiedTTLMinutes: &ttl,
		}); err != nil {
			t.Fatalf("editar TTL para %d: %v", tc.in, err)
		}
		got, err = svc.GetByID(ctx, out.ID, storeID)
		if err != nil {
			t.Fatalf("GetByID: %v", err)
		}
		if got.WaitlistNotifiedTTLMinutes != tc.want {
			t.Errorf("TTL %d virou %d, queria %d", tc.in, got.WaitlistNotifiedTTLMinutes, tc.want)
		}
	}
}
