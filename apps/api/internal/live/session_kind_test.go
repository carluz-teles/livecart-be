package live

// A espécie do evento sai das SESSÕES, nunca de live_events.type.
//
// Por que este teste existe: a 000119 dropa live_events.type, e a tela de
// eventos (lista, detalhe e o checkout do COMPRADOR) escolhe o vocabulário
// — "Live em andamento" contra "Promoção ativa" — a partir dessa espécie.
// Enquanto a fonte for a coluna do container, o drop derruba as três telas de
// uma vez. Enquanto a fonte for a sessão, o drop é inerte.
//
// O caso que prova a diferença não é o evento de uma sessão só (nele os dois
// caminhos concordam): é a campanha MISTA, que é justamente o que o épico
// passou a permitir. Um evento gravado como type='single' que ganhou um post
// tem DUAS espécies dentro; a coluna do container só sabe dizer uma.

import (
	"context"
	"testing"
	"time"
)

func TestSessionTypesComeFromSessionsNotFromEventType(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	storeID := seedWindowStore(t, ctx, "especie-por-sessao")
	svc := newWindowService(nil)

	endsAt := time.Now().Add(48 * time.Hour)
	created, err := svc.Create(ctx, CreateLiveInput{
		StoreID: storeID,
		Title:   "Semana Black",
		// O container nasce 'multi' — o vocabulário legado. A campanha abaixo
		// vira mista, e nenhum valor de coluna única descreveria isso.
		Type:   "multi",
		EndsAt: &endsAt,
	})
	if err != nil {
		t.Fatalf("criar evento: %v", err)
	}

	// Sessão de post na MESMA campanha: é o guarda-chuva funcionando.
	if _, err := svc.CreateSession(ctx, CreateSessionInput{
		EventID:        created.ID,
		StoreID:        storeID,
		Type:           "post",
		Platform:       "instagram",
		PlatformLiveID: "media-especie-post",
	}); err != nil {
		t.Fatalf("criar sessao de post: %v", err)
	}

	t.Run("detalhe", func(t *testing.T) {
		out, err := svc.GetEventWithSessions(ctx, created.ID, storeID)
		if err != nil {
			t.Fatalf("detalhe do evento: %v", err)
		}
		assertKinds(t, distinctSessionTypes(out.Sessions), "live", "post")
	})

	t.Run("lista", func(t *testing.T) {
		out, err := svc.List(ctx, ListLivesInput{StoreID: storeID})
		if err != nil {
			t.Fatalf("listar eventos: %v", err)
		}
		var found bool
		for _, l := range out.Lives {
			if l.ID != created.ID {
				continue
			}
			found = true
			assertKinds(t, l.SessionTypes, "live", "post")
		}
		if !found {
			t.Fatalf("evento %s nao apareceu na listagem", created.ID)
		}
	})
}

// TestSessionTypesIsEmptyNotNullWithoutSessions fixa o contrato do JSON: lista
// vazia significa "esta campanha ainda não tem transmissão", que é um estado
// real e navegável. Emitir null faria a tela ter de adivinhar se é isso ou se o
// backend não respondeu.
func TestSessionTypesIsEmptyNotNullWithoutSessions(t *testing.T) {
	if got := nonNilStrings(nil); got == nil || len(got) != 0 {
		t.Fatalf("nonNilStrings(nil) = %#v, queria lista vazia nao-nula", got)
	}
	if got := distinctSessionTypes(nil); got == nil || len(got) != 0 {
		t.Fatalf("distinctSessionTypes(nil) = %#v, queria lista vazia nao-nula", got)
	}
}

func assertKinds(t *testing.T, got []string, want ...string) {
	t.Helper()
	set := make(map[string]struct{}, len(got))
	for _, g := range got {
		set[g] = struct{}{}
	}
	if len(set) != len(got) {
		t.Errorf("sessionTypes tem repetição: %v", got)
	}
	for _, w := range want {
		if _, ok := set[w]; !ok {
			t.Errorf("sessionTypes = %v, faltou %q", got, w)
		}
	}
	if len(got) != len(want) {
		t.Errorf("sessionTypes = %v, queria exatamente %v", got, want)
	}
}
