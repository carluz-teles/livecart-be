package live

// RN-10 / adendo A8 — a extensão de prazo de quem é promovido da fila JÁ
// funcionava no runtime: o TTL é lido do EVENTO
// (GetWaitlistNotifiedTTLByEvent), vira notifiedUntil na reivindicação atômica
// e empurra o expires_at do carrinho com GREATEST (não encolhe). A coluna
// live_events.waitlist_notified_ttl_minutes existe desde a 000073, com
// CHECK 5..240 — e não existe equivalente em stores.
//
// O que FALTAVA era só exposição: a coluna não aparecia em nenhum DTO da API
// nem em nenhum arquivo do frontend, então o lojista não podia nem ver nem
// mudar o número que governa a regra. Sem migration.
//
// Estes testes travam o caminho de escrita/leitura e o alinhamento com o CHECK
// — a lição E6 da errata é exatamente essa: validação de aplicação frouxa
// transforma erro de campo em 500.

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

	// Fora da faixa: o repo faz o clamp em vez de deixar o CHECK 5..240
	// devolver 500. A validação sintática (5..240) já barra no handler; este é
	// o guarda-costas para chamadas internas.
	for _, tc := range []struct{ in, want int }{{1, 5}, {9999, 240}} {
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
