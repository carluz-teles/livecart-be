package live

// RN-05 / CA-05.1, CA-05.3, CA-05.4, CA-05.7 — a janela comercial do evento.
//
// Antes desta fatia não existia caminho de ESCRITA de ends_at para evento de
// live nem caminho de EDIÇÃO para tipo nenhum: SetEventWindow tinha um único
// chamador (a criação de post/story) e o PUT só trocava título e desconto PIX.
// Um ends_at errado era permanente, e sem ends_at o carrinho ficava sem prazo
// para sempre (a RN-04 mantém expires_at NULL durante o evento).
//
// O que estes testes fixam:
//  1. criar evento sem ends_at é 400 — em qualquer tipo;
//  2. criar grava starts_at E ends_at e ARMA o fechamento;
//  3. editar só o fim NÃO apaga o início (o UPDATE antigo escrevia as duas
//     colunas de uma vez e apagava o que não fosse enviado);
//  4. editar o fim RE-AGENDA (move), não re-arma: um arm com o mesmo TaskID é
//     engolido pelo asynq e o evento fecharia na hora antiga — que é
//     exatamente o bug do CA-05.4 (antecipar).

import (
	"context"
	"testing"
	"time"

	"go.uber.org/zap"

	"livecart/apps/api/lib/httpx"
)

// fakeCloseScheduler grava as chamadas para o teste poder distinguir ARMAR de
// MOVER — a diferença que fazia a antecipação virar no-op.
type fakeCloseScheduler struct {
	armed []time.Time
	moved []time.Time
}

func (f *fakeCloseScheduler) ScheduleEventClose(_ context.Context, _, _ string, at time.Time) error {
	f.armed = append(f.armed, at)
	return nil
}

func (f *fakeCloseScheduler) RescheduleEventClose(_ context.Context, _, _ string, at time.Time) error {
	f.moved = append(f.moved, at)
	return nil
}

func newWindowService(sched EventCloseScheduler) *Service {
	svc := NewService(testRepo, zap.NewNop())
	if sched != nil {
		svc.SetEventCloseScheduler(sched)
	}
	return svc
}

func seedWindowStore(t *testing.T, ctx context.Context, slug string) string {
	t.Helper()
	var storeID string
	if err := testPool.QueryRow(ctx,
		`INSERT INTO stores (name, slug) VALUES ($1,$1) RETURNING id::text`, slug,
	).Scan(&storeID); err != nil {
		t.Fatalf("seed store: %v", err)
	}
	return storeID
}

// readWindow devolve (starts_at, scheduled_at, ends_at) crus do banco.
func readWindow(t *testing.T, ctx context.Context, eventID string) (*time.Time, *time.Time, *time.Time) {
	t.Helper()
	var starts, scheduled, ends *time.Time
	if err := testPool.QueryRow(ctx,
		`SELECT starts_at, scheduled_at, ends_at FROM live_events WHERE id = $1::uuid`, eventID,
	).Scan(&starts, &scheduled, &ends); err != nil {
		t.Fatalf("ler janela: %v", err)
	}
	return starts, scheduled, ends
}

func TestCreateEventRequiresEndsAt(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	storeID := seedWindowStore(t, ctx, "win-required")
	svc := newWindowService(nil)

	_, err := svc.Create(ctx, CreateLiveInput{StoreID: storeID, Title: "Sem teto", Type: "multi"})
	if err == nil {
		t.Fatal("criar evento sem endsAt passou — o carrinho ficaria sem prazo para sempre")
	}
	if httpx.StatusFromError(err) != 400 {
		t.Errorf("erro de campo obrigatório deveria ser 400, veio %d (%v)", httpx.StatusFromError(err), err)
	}
}

func TestCreateEventWritesWindowAndArmsClose(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	storeID := seedWindowStore(t, ctx, "win-create")
	sched := &fakeCloseScheduler{}
	svc := newWindowService(sched)

	starts := time.Now().UTC().Add(1 * time.Hour).Truncate(time.Second)
	ends := starts.Add(48 * time.Hour)

	out, err := svc.Create(ctx, CreateLiveInput{
		StoreID: storeID, Title: "Semana Black", Type: "multi",
		StartsAt: &starts, EndsAt: &ends,
	})
	if err != nil {
		t.Fatalf("criar evento: %v", err)
	}

	gotStarts, gotScheduled, gotEnds := readWindow(t, ctx, out.ID)
	if gotStarts == nil || !gotStarts.UTC().Equal(starts) {
		t.Errorf("starts_at = %v, queria %v", gotStarts, starts)
	}
	// scheduled_at segue starts_at até a 000119 — EffectiveStatus e o FE ainda
	// leem a coluna antiga.
	if gotScheduled == nil || !gotScheduled.UTC().Equal(starts) {
		t.Errorf("scheduled_at = %v, queria espelhar starts_at (%v)", gotScheduled, starts)
	}
	if gotEnds == nil || !gotEnds.UTC().Equal(ends) {
		t.Errorf("ends_at = %v, queria %v", gotEnds, ends)
	}

	// O fechamento tem de nascer armado: sem isso ends_at é só um rótulo e
	// nada fecha o evento de live (o sweep só via post/reel/story).
	if len(sched.armed) != 1 || !sched.armed[0].UTC().Equal(ends) {
		t.Errorf("fechamento não foi armado no ends_at: armed=%v", sched.armed)
	}
	if len(sched.moved) != 0 {
		t.Errorf("criação não deveria MOVER nada: moved=%v", sched.moved)
	}
}

func TestCreateEventRejectsEndBeforeStart(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	storeID := seedWindowStore(t, ctx, "win-order")
	svc := newWindowService(nil)

	starts := time.Now().UTC().Add(24 * time.Hour)
	ends := starts.Add(-1 * time.Hour)
	if _, err := svc.Create(ctx, CreateLiveInput{
		StoreID: storeID, Title: "Invertido", Type: "multi",
		StartsAt: &starts, EndsAt: &ends,
	}); err == nil {
		t.Fatal("aceitou ends_at antes de starts_at")
	}
}

func TestUpdateEndsAtRescheduesAndKeepsStart(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	storeID := seedWindowStore(t, ctx, "win-edit")
	sched := &fakeCloseScheduler{}
	svc := newWindowService(sched)

	starts := time.Now().UTC().Add(1 * time.Hour).Truncate(time.Second)
	ends := starts.Add(48 * time.Hour)
	out, err := svc.Create(ctx, CreateLiveInput{
		StoreID: storeID, Title: "Editável", Type: "multi",
		StartsAt: &starts, EndsAt: &ends,
	})
	if err != nil {
		t.Fatalf("criar evento: %v", err)
	}

	// CA-05.4: ANTECIPAR. É o caso que provava o bug — um arm com o mesmo
	// TaskID seria engolido e o evento fecharia na hora antiga.
	earlier := starts.Add(6 * time.Hour)
	if _, err := svc.Update(ctx, UpdateLiveInput{
		ID: out.ID, StoreID: storeID, Title: "Editável",
		Window: EventWindowUpdate{SetEndsAt: true, EndsAt: &earlier},
	}); err != nil {
		t.Fatalf("antecipar ends_at: %v", err)
	}

	gotStarts, _, gotEnds := readWindow(t, ctx, out.ID)
	if gotEnds == nil || !gotEnds.UTC().Equal(earlier) {
		t.Errorf("ends_at = %v, queria %v", gotEnds, earlier)
	}
	// O bug do UPDATE antigo: escrevia scheduled_at e ends_at juntos, então
	// editar só o fim apagava o início.
	if gotStarts == nil || !gotStarts.UTC().Equal(starts) {
		t.Errorf("editar só o fim apagou starts_at: %v (queria %v)", gotStarts, starts)
	}
	if len(sched.moved) != 1 || !sched.moved[0].UTC().Equal(earlier) {
		t.Errorf("antecipação não re-agendou (MOVE) o fechamento: moved=%v", sched.moved)
	}

	// CA-05.3: ESTENDER também é MOVE, pelo mesmo motivo.
	later := starts.Add(72 * time.Hour)
	if _, err := svc.Update(ctx, UpdateLiveInput{
		ID: out.ID, StoreID: storeID, Title: "Editável",
		Window: EventWindowUpdate{SetEndsAt: true, EndsAt: &later},
	}); err != nil {
		t.Fatalf("estender ends_at: %v", err)
	}
	if len(sched.moved) != 2 || !sched.moved[1].UTC().Equal(later) {
		t.Errorf("extensão não re-agendou o fechamento: moved=%v", sched.moved)
	}
}

func TestUpdateCannotRemoveEndsAt(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	storeID := seedWindowStore(t, ctx, "win-noremove")
	svc := newWindowService(&fakeCloseScheduler{})

	ends := time.Now().UTC().Add(24 * time.Hour)
	out, err := svc.Create(ctx, CreateLiveInput{
		StoreID: storeID, Title: "Com teto", Type: "multi", EndsAt: &ends,
	})
	if err != nil {
		t.Fatalf("criar evento: %v", err)
	}

	if _, err := svc.Update(ctx, UpdateLiveInput{
		ID: out.ID, StoreID: storeID, Title: "Com teto",
		Window: EventWindowUpdate{SetEndsAt: true, EndsAt: nil},
	}); err == nil {
		t.Fatal("aceitou remover o ends_at — o carrinho voltaria a não ter prazo")
	}

	_, _, gotEnds := readWindow(t, ctx, out.ID)
	if gotEnds == nil {
		t.Error("ends_at foi apagado mesmo com o erro")
	}
}
