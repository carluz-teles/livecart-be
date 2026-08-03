package live

// Encerrar a SESSÃO não encerra o EVENTO nem finaliza carrinho.
//
// É a regra que dá sentido ao evento guarda-chuva: a live de segunda acaba, o
// evento segue até sábado e o comprador de segunda continua com o carrinho
// aberto para comprar de novo na terça. Se encerrar a live fechasse o evento —
// ou pior, finalizasse os carrinhos, como POST /lives/:id/end faz — não existiria
// pedido único atravessando os dias, que é a razão do épico.
//
// Antes deste caminho o lojista não tinha escolha: o único "encerrar" era o do
// evento, então parar a live de segunda custava o evento inteiro.

import (
	"context"
	"testing"
	"time"
)

func TestEndSessionKeepsTheEventAndItsCartsAlive(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	storeID := seedWindowStore(t, ctx, "encerrar-sessao")
	svc := newWindowService(nil)

	endsAt := time.Now().Add(72 * time.Hour)
	evento, err := svc.Create(ctx, CreateLiveInput{
		StoreID: storeID,
		Title:   "Semana Black",
		EndsAt:  &endsAt,
	})
	if err != nil {
		t.Fatalf("criar evento: %v", err)
	}

	sessao, err := svc.CreateSession(ctx, CreateSessionInput{
		EventID:        evento.ID,
		StoreID:        storeID,
		Type:           "live",
		Platform:       "instagram",
		PlatformLiveID: "media-encerrar-sessao",
	})
	if err != nil {
		t.Fatalf("criar sessao: %v", err)
	}

	// Um carrinho aberto na sessão, como o de um comprador que comentou.
	// carts não tem store_id: a loja vem pelo evento.
	var cartID string
	if err := testPool.QueryRow(ctx,
		`INSERT INTO carts (event_id, session_id, platform_user_id, platform_handle, token, short_id, status)
		 VALUES ($1::uuid, $2::uuid, 'comprador-1', '@comprador_1', 'tok-encerrar-sessao', 77001, 'active')
		 RETURNING id::text`, evento.ID, sessao.ID,
	).Scan(&cartID); err != nil {
		t.Fatalf("seed carrinho: %v", err)
	}

	out, err := svc.EndSession(ctx, EndSessionInput{
		StoreID:   storeID,
		EventID:   evento.ID,
		SessionID: sessao.ID,
	})
	if err != nil {
		t.Fatalf("encerrar sessao: %v", err)
	}
	if out.Status != SessionStatusEnded {
		t.Errorf("sessao ficou %q, quero %q", out.Status, SessionStatusEnded)
	}

	// 1. O EVENTO continua aberto — era a única sessão, e mesmo assim.
	var statusEvento string
	if err := testPool.QueryRow(ctx,
		`SELECT status FROM live_events WHERE id = $1::uuid`, evento.ID,
	).Scan(&statusEvento); err != nil {
		t.Fatalf("ler status do evento: %v", err)
	}
	if statusEvento == "ended" {
		t.Error("o evento foi encerrado junto com a sessao — o comprador perde o prazo que ainda tinha")
	}

	// 2. O CARRINHO continua aberto. É o ponto todo: ele atravessa a sessão.
	var statusCarrinho string
	if err := testPool.QueryRow(ctx,
		`SELECT status FROM carts WHERE id = $1::uuid`, cartID,
	).Scan(&statusCarrinho); err != nil {
		t.Fatalf("ler status do carrinho: %v", err)
	}
	if statusCarrinho != "active" {
		t.Errorf("carrinho virou %q ao encerrar a sessao — deveria seguir aberto ate o fim do evento", statusCarrinho)
	}
}

// Encerrar duas vezes devolve o estado atual em vez de erro: o botão vive na
// tela de uma live que acabou de acabar, que é o momento de clique repetido.
func TestEndSessionIsIdempotent(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	storeID := seedWindowStore(t, ctx, "encerrar-sessao-2x")
	svc := newWindowService(nil)

	endsAt := time.Now().Add(48 * time.Hour)
	evento, err := svc.Create(ctx, CreateLiveInput{StoreID: storeID, Title: "Evento", EndsAt: &endsAt})
	if err != nil {
		t.Fatalf("criar evento: %v", err)
	}
	sessao, err := svc.CreateSession(ctx, CreateSessionInput{
		EventID: evento.ID, StoreID: storeID, Type: "live",
	})
	if err != nil {
		t.Fatalf("criar sessao: %v", err)
	}

	in := EndSessionInput{StoreID: storeID, EventID: evento.ID, SessionID: sessao.ID}
	if _, err := svc.EndSession(ctx, in); err != nil {
		t.Fatalf("primeiro encerramento: %v", err)
	}
	out, err := svc.EndSession(ctx, in)
	if err != nil {
		t.Fatalf("segundo encerramento devia ser no-op, veio erro: %v", err)
	}
	if out.Status != SessionStatusEnded {
		t.Errorf("segundo encerramento devolveu status %q", out.Status)
	}
}

// A sessão de OUTRO evento não pode ser encerrada pela rota deste. A posse é
// checada contra o par (evento, loja) — o id da sessão sozinho não basta.
func TestEndSessionRejectsSessionFromAnotherEvent(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	storeID := seedWindowStore(t, ctx, "encerrar-sessao-alheia")
	svc := newWindowService(nil)

	endsAt := time.Now().Add(48 * time.Hour)
	eventoA, err := svc.Create(ctx, CreateLiveInput{StoreID: storeID, Title: "Evento A", EndsAt: &endsAt})
	if err != nil {
		t.Fatalf("criar evento A: %v", err)
	}
	eventoB, err := svc.Create(ctx, CreateLiveInput{StoreID: storeID, Title: "Evento B", EndsAt: &endsAt})
	if err != nil {
		t.Fatalf("criar evento B: %v", err)
	}
	sessaoB, err := svc.CreateSession(ctx, CreateSessionInput{
		EventID: eventoB.ID, StoreID: storeID, Type: "live",
	})
	if err != nil {
		t.Fatalf("criar sessao do evento B: %v", err)
	}

	if _, err := svc.EndSession(ctx, EndSessionInput{
		StoreID:   storeID,
		EventID:   eventoA.ID,
		SessionID: sessaoB.ID,
	}); err == nil {
		t.Fatal("encerrar sessao de outro evento devia falhar")
	}
}
