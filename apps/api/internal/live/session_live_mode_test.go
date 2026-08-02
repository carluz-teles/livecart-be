package live

// D17 — o Modo Live é da SESSÃO. O que estes testes fixam é justamente a razão
// da decisão: no evento, duas transmissões simultâneas compartilhavam o mesmo
// produto em destaque e a mesma pausa.

import (
	"context"
	"testing"
)

func seedLiveModeFixture(t *testing.T, ctx context.Context, slug string) whitelistFixture {
	t.Helper()
	f := seedWhitelistFixture(t, ctx, slug, 2)
	return f
}

// Pausar uma sessão não pausa a outra do mesmo evento.
func TestSessionProcessingPauseIsPerSession(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	f := seedLiveModeFixture(t, ctx, "lm-pausa")

	if _, err := testRepo.SetSessionProcessingPaused(ctx, f.sessions[0], f.storeID, true); err != nil {
		t.Fatalf("SetSessionProcessingPaused: %v", err)
	}

	first, err := testRepo.GetSessionByID(ctx, f.sessions[0])
	if err != nil {
		t.Fatalf("GetSessionByID: %v", err)
	}
	second, err := testRepo.GetSessionByID(ctx, f.sessions[1])
	if err != nil {
		t.Fatalf("GetSessionByID: %v", err)
	}
	if !first.ProcessingPaused {
		t.Error("a sessao pausada devia ficar pausada")
	}
	if second.ProcessingPaused {
		t.Error("pausar uma transmissao pausou a outra — e exatamente o que a D17 desfaz")
	}
}

// O destaque também é por transmissão, e limpar é passar nil.
func TestSessionActiveProductIsPerSession(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	f := seedLiveModeFixture(t, ctx, "lm-destaque")

	if _, err := testRepo.SetSessionActiveProduct(ctx, f.sessions[0], f.storeID, &f.products[0]); err != nil {
		t.Fatalf("SetSessionActiveProduct: %v", err)
	}
	if _, err := testRepo.SetSessionActiveProduct(ctx, f.sessions[1], f.storeID, &f.products[1]); err != nil {
		t.Fatalf("SetSessionActiveProduct: %v", err)
	}

	for i, sessionID := range f.sessions {
		state, err := testRepo.GetSessionLiveModeState(ctx, sessionID, f.storeID)
		if err != nil {
			t.Fatalf("GetSessionLiveModeState: %v", err)
		}
		if state.ActiveProduct == nil || state.ActiveProduct.ID != f.products[i] {
			t.Errorf("sessao %d ficou com destaque %v, quero %s", i, state.ActiveProduct, f.products[i])
		}
	}

	// Limpar: productID nil.
	if _, err := testRepo.SetSessionActiveProduct(ctx, f.sessions[0], f.storeID, nil); err != nil {
		t.Fatalf("limpar destaque: %v", err)
	}
	state, err := testRepo.GetSessionLiveModeState(ctx, f.sessions[0], f.storeID)
	if err != nil {
		t.Fatalf("GetSessionLiveModeState: %v", err)
	}
	if state.ActiveProduct != nil {
		t.Error("productId nil devia limpar o destaque")
	}
	if state.SessionID != f.sessions[0] {
		t.Errorf("o estado devia dizer de qual sessao veio: %q", state.SessionID)
	}
}

// Posse: uma loja não mexe na sessão de outra. As queries antigas casavam
// (id, store_id) na mesma linha do evento; agora a loja está a duas tabelas de
// distância e o JOIN é a única coisa que segura isso.
func TestSessionLiveModeRejectsForeignStore(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	mine := seedLiveModeFixture(t, ctx, "lm-posse-a")
	other := seedLiveModeFixture(t, ctx, "lm-posse-b")

	if _, err := testRepo.SetSessionProcessingPaused(ctx, mine.sessions[0], other.storeID, true); err == nil {
		t.Error("a loja de outro evento conseguiu pausar esta sessao")
	}
	if _, err := testRepo.GetSessionLiveModeState(ctx, mine.sessions[0], other.storeID); err == nil {
		t.Error("a loja de outro evento conseguiu ler o estado desta sessao")
	}
}

// A rota legada por evento escreve nas sessões vivas e lê da mais recente — é o
// que mantém o painel atual (que só conhece eventId) funcionando.
func TestEventLevelLiveModeReachesLiveSessions(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	f := seedLiveModeFixture(t, ctx, "lm-legado")

	if err := testRepo.SetActiveProductForEventSessions(ctx, f.eventID, f.storeID, &f.products[0]); err != nil {
		t.Fatalf("SetActiveProductForEventSessions: %v", err)
	}
	for i, sessionID := range f.sessions {
		state, err := testRepo.GetSessionLiveModeState(ctx, sessionID, f.storeID)
		if err != nil {
			t.Fatalf("GetSessionLiveModeState: %v", err)
		}
		if state.ActiveProduct == nil {
			t.Errorf("sessao %d nao recebeu o destaque pela rota legada", i)
		}
	}

	state, err := testRepo.GetLiveModeState(ctx, f.eventID, f.storeID)
	if err != nil {
		t.Fatalf("GetLiveModeState: %v", err)
	}
	if state.ActiveProduct == nil || state.ActiveProduct.ID != f.products[0] {
		t.Errorf("a leitura por evento nao achou o destaque: %v", state.ActiveProduct)
	}
	if state.SessionID == "" {
		t.Error("a leitura por evento devia dizer QUAL sessao esta sendo controlada")
	}
}

// Evento sem sessão nenhuma devolve estado neutro em vez de 404: o painel não
// pode quebrar por causa de um evento que ainda não tem transmissão.
func TestEventLevelLiveModeWithoutSessions(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	f := seedWhitelistFixture(t, ctx, "lm-sem-sessao", 0)

	state, err := testRepo.GetLiveModeState(ctx, f.eventID, f.storeID)
	if err != nil {
		t.Fatalf("GetLiveModeState: %v", err)
	}
	if state.ActiveProduct != nil || state.ProcessingPaused {
		t.Errorf("evento sem sessao devia devolver estado neutro, veio %+v", state)
	}
}
