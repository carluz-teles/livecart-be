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

// Publicar um post/reel DENTRO de um evento existente grava a lista de produtos
// junto com a sessão.
//
// É o atalho de publicar pelo LiveCart: o lojista escolhe, no mesmo formulário,
// a mídia e o que aquela publicação vende. Sem a lista nascer com a sessão, a
// publicação vai ao ar vendendo TUDO — porque sessão sem lista vende tudo — que
// é o oposto do que ele acabou de escolher.
func TestCreateSessionGravaProdutosDaPublicacao(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	storeID := seedWindowStore(t, ctx, "publicar-no-evento")
	svc := newWindowService(nil)

	productIDs := make([]string, 0, 2)
	for _, kw := range []string{"PUB1", "PUB2"} {
		var id string
		if err := testPool.QueryRow(ctx,
			`INSERT INTO products (store_id, name, keyword, external_source, price, stock, active)
			 VALUES ($1,$2,$3,'manual',5000,5,true) RETURNING id::text`, storeID, "Produto "+kw, kw,
		).Scan(&id); err != nil {
			t.Fatalf("seed produto: %v", err)
		}
		productIDs = append(productIDs, id)
	}

	endsAt := time.Now().Add(48 * time.Hour)
	evento, err := svc.Create(ctx, CreateLiveInput{
		StoreID: storeID, Title: "Semana Black", EndsAt: &endsAt,
	})
	if err != nil {
		t.Fatalf("criar evento: %v", err)
	}

	sessao, err := svc.CreateSession(ctx, CreateSessionInput{
		EventID:        evento.ID,
		StoreID:        storeID,
		Type:           SessionTypePost,
		Platform:       "instagram",
		PlatformLiveID: "media-publicada-no-evento",
		ProductIDs:     productIDs,
	})
	if err != nil {
		t.Fatalf("criar sessao de post no evento: %v", err)
	}

	list, err := testRepo.ListSessionProducts(ctx, sessao.ID)
	if err != nil {
		t.Fatalf("ListSessionProducts: %v", err)
	}
	if len(list) != len(productIDs) {
		t.Fatalf("a publicacao ficou com %d produto(s), quero %d — sem lista ela venderia o catalogo inteiro",
			len(list), len(productIDs))
	}
	for i, p := range list {
		if p.DisplayOrder != int32(i) {
			t.Errorf("produto %d veio com display_order %d, quero %d (a ordem escolhida no formulario)", i, p.DisplayOrder, i)
		}
	}

	// E continua sendo uma sessão DO evento — não um evento novo.
	if sessao.EventID != evento.ID {
		t.Errorf("sessao nasceu no evento %s, esperado %s", sessao.EventID, evento.ID)
	}
}
