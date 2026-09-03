package order_test

// Matéria-prima da árvore de histórico do pedido (20/08/2026): as DMs que o
// LiveCart mandou (notification_logs, com texto e desfecho) e a jornada
// COMPLETA da fila — inclusive entradas encerradas, que a seção "Aguardando
// estoque" de propósito esconde. O detalhe passa a carregar as duas.

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"livecart/apps/api/internal/order"
)

func TestDetalheCarregaDMsEnviadasComDesfecho(t *testing.T) {
	requireDB(t)
	storeID, eventID := seedIsolatedStore(t, "HistDM")
	cartID := insertCart(t, eventID, "ana", "tok-hist-1", 9301, "pending", nil)

	ctx := context.Background()
	if _, err := testPool.Exec(ctx,
		`INSERT INTO notification_logs (store_id, event_id, cart_id, platform_user_id, notification_type, status, message_text, sent_at)
		 VALUES ($1, $2, $3, 'u-ana', 'checkout_immediate', 'sent', 'Olá! Seu carrinho: link', now())`,
		storeID, eventID, cartID); err != nil {
		t.Fatalf("seed DM enviada: %v", err)
	}
	if _, err := testPool.Exec(ctx,
		`INSERT INTO notification_logs (store_id, event_id, cart_id, platform_user_id, notification_type, status, error_message)
		 VALUES ($1, $2, $3, 'u-ana', 'checkout_reminder', 'failed', 'janela de 24h fechada')`,
		storeID, eventID, cartID); err != nil {
		t.Fatalf("seed DM falhada: %v", err)
	}

	rows, err := order.NewRepository(testPool).ListCartNotifications(ctx, cartID)
	if err != nil {
		t.Fatalf("ListCartNotifications: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("DMs carregadas = %d; esperava 2", len(rows))
	}
	if rows[0].Type != "checkout_immediate" || rows[0].Status != "sent" ||
		rows[0].Message == nil || *rows[0].Message == "" {
		t.Errorf("primeira DM = %+v; esperava enviada COM o texto verbatim", rows[0])
	}
	if rows[1].Status != "failed" || rows[1].Error == nil {
		t.Errorf("segunda DM = %+v; a falha precisa vir com o motivo", rows[1])
	}
}

func TestDetalheCarregaJornadaCompletaDaFila(t *testing.T) {
	requireDB(t)
	storeID, eventID := seedIsolatedStore(t, "HistFila")
	produto := seedProduct(t, storeID, 1500)
	cartID := insertCart(t, eventID, "bia", "tok-hist-2", 9302, "pending", nil)

	ctx := context.Background()
	// Entrada ENCERRADA: liberou e o prazo venceu — o desfecho que hoje some
	// da tela (a fila ativa não a mostra mais).
	if _, err := testPool.Exec(ctx,
		`INSERT INTO waitlist_items (event_id, cart_id, product_id, platform_user_id, platform_handle, quantity, position, status, notified_at, expires_at)
		 VALUES ($1, $2, $3, 'u-bia', '@bia', 2, 1, 'expired', now() - interval '2 hours', now() - interval '1 hour')`,
		eventID, cartID, produto); err != nil {
		t.Fatalf("seed fila expirada: %v", err)
	}

	rows, err := order.NewRepository(testPool).ListWaitlistJourney(ctx, cartID)
	if err != nil {
		t.Fatalf("ListWaitlistJourney: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("jornada = %d entradas; esperava 1 (a encerrada TEM de aparecer)", len(rows))
	}
	w := rows[0]
	if w.Status != "expired" || w.NotifiedAt == nil || w.ExpiresAt == nil {
		t.Errorf("jornada = %+v; esperava expired com notified_at e expires_at", w)
	}
	if w.NotifiedAt != nil && time.Since(*w.NotifiedAt) < time.Hour {
		t.Errorf("notified_at = %v; esperava ~2h atrás", w.NotifiedAt)
	}
	if w.Quantity != 2 || w.ProductName == "" {
		t.Errorf("jornada sem produto/quantidade: %+v", w)
	}
}

// ─── Comentários da live COM DESFECHO (02/09/2026) ──────────────────────────
//
// A consulta trazia `id, text, created_at` e nada mais. Na linha do tempo, três
// coisas muito diferentes ficavam com a mesma cara cinza:
//
//	"2074"  → virou item        (venda)
//	"2096"  → foi para a fila   (venda adiada)
//	"2071"  → não casou produto (venda PERDIDA, e a mais invisível das três)
//
// `result`, `matched_product_id` e `matched_quantity` estavam em live_comments
// desde sempre. Estes testes travam que eles cheguem até quem lê.

// seedComentario grava uma fala da compradora com o desfecho que ela teve.
func seedComentario(t *testing.T, eventID, userID, texto, resultado string, produtoID *string, qtd int, quando time.Time) {
	t.Helper()
	if _, err := testPool.Exec(context.Background(),
		`INSERT INTO live_comments (event_id, platform, platform_comment_id, platform_user_id,
		     platform_handle, text, has_purchase_intent, matched_product_id, matched_quantity,
		     result, created_at)
		 VALUES ($1,'instagram','c-'||md5(random()::text),$2,'@ana',$3,true,$4::uuid,$5,$6,$7)`,
		eventID, userID, texto, produtoID, qtd, resultado, quando); err != nil {
		t.Fatalf("seed comentário: %v", err)
	}
}

func TestComentariosChegamComDesfechoProdutoEQuantidade(t *testing.T) {
	requireDB(t)
	storeID, eventID := seedIsolatedStore(t, "HistCom")
	cartID := insertCart(t, eventID, "ana", "tok-com-1", 9401, "pending", nil)

	var userID string
	if err := testPool.QueryRow(context.Background(),
		`SELECT platform_user_id FROM carts WHERE id = $1`, cartID).Scan(&userID); err != nil {
		t.Fatalf("lendo o comprador: %v", err)
	}
	var produtoID string
	if err := testPool.QueryRow(context.Background(),
		`INSERT INTO products (store_id, name, keyword, external_source, price, stock, active)
		 VALUES ($1,'Vela LED','2074','manual',590,5,true) RETURNING id::text`,
		storeID).Scan(&produtoID); err != nil {
		t.Fatalf("seed produto: %v", err)
	}

	// No passado: o comentário que ORIGINA o carrinho vem antes dele, e o
	// seguinte vem depois. Semear no futuro passaria do `now()` da janela.
	agora := time.Now()
	seedComentario(t, eventID, userID, "2074", "added_to_cart", &produtoID, 2, agora.Add(-30*time.Second))
	seedComentario(t, eventID, userID, "quero 9999", "no_product", nil, 0, agora.Add(-10*time.Second))

	rows, err := order.NewRepository(testPool).GetCustomerComments(context.Background(), cartID, userID)
	if err != nil {
		t.Fatalf("GetCustomerComments: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("comentários = %d, queria 2", len(rows))
	}

	if rows[0].Result != "added_to_cart" {
		t.Errorf("result = %q, queria 'added_to_cart' — sem ele a linha do tempo "+
			"não distingue venda de venda perdida", rows[0].Result)
	}
	if rows[0].ProductName != "Vela LED" || rows[0].ProductKeyword != "2074" {
		t.Errorf("produto = %q/%q, queria 'Vela LED'/'2074'", rows[0].ProductName, rows[0].ProductKeyword)
	}
	if rows[0].Quantity != 2 {
		t.Errorf("quantidade = %d, queria 2", rows[0].Quantity)
	}

	// O comentário que NÃO casou é o mais importante de aparecer, e é o que
	// mais facilmente some: sem produto, um JOIN interno o eliminaria.
	if rows[1].Result != "no_product" {
		t.Errorf("result do segundo = %q, queria 'no_product'", rows[1].Result)
	}
	if rows[1].ProductName != "" {
		t.Errorf("produto do segundo = %q, queria vazio", rows[1].ProductName)
	}
}

// O carrinho ETERNO do VIP existe para atravessar eventos. Casar por event_id
// mostraria só as falas de um dos dias — e o pedido tem itens dos dois.
func TestCarrinhoEternoVeOsComentariosDosDoisEventos(t *testing.T) {
	requireDB(t)
	storeID, evSegunda := seedIsolatedStore(t, "HistVip")
	cartID := insertCart(t, evSegunda, "ana", "tok-com-2", 9402, "pending", nil)

	ctx := context.Background()
	var userID string
	if err := testPool.QueryRow(ctx,
		`SELECT platform_user_id FROM carts WHERE id = $1`, cartID).Scan(&userID); err != nil {
		t.Fatalf("lendo o comprador: %v", err)
	}
	var evQuinta string
	if err := testPool.QueryRow(ctx,
		`INSERT INTO live_events (store_id, status, title, ends_at)
		 VALUES ($1,'live','Quinta', now() + interval '2 days') RETURNING id::text`,
		storeID).Scan(&evQuinta); err != nil {
		t.Fatalf("seed evento de quinta: %v", err)
	}

	agora := time.Now()
	seedComentario(t, evSegunda, userID, "segunda", "added_to_cart", nil, 1, agora.Add(-30*time.Second))
	seedComentario(t, evQuinta, userID, "quinta", "added_to_cart", nil, 1, agora.Add(-10*time.Second))

	rows, err := order.NewRepository(testPool).GetCustomerComments(ctx, cartID, userID)
	if err != nil {
		t.Fatalf("GetCustomerComments: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("comentários = %d, queria 2 — o carrinho eterno atravessa "+
			"eventos e a janela precisa acompanhá-lo", len(rows))
	}
}

// Comentário apagado no Instagram não pode aparecer: é prova que ninguém
// consegue conferir do outro lado.
func TestComentarioApagadoOuEscondidoNaoAparece(t *testing.T) {
	requireDB(t)
	_, eventID := seedIsolatedStore(t, "HistDel")
	cartID := insertCart(t, eventID, "ana", "tok-com-3", 9403, "pending", nil)

	ctx := context.Background()
	var userID string
	if err := testPool.QueryRow(ctx,
		`SELECT platform_user_id FROM carts WHERE id = $1`, cartID).Scan(&userID); err != nil {
		t.Fatalf("lendo o comprador: %v", err)
	}

	agora := time.Now()
	seedComentario(t, eventID, userID, "fica", "added_to_cart", nil, 1, agora.Add(-30*time.Second))
	seedComentario(t, eventID, userID, "apagado", "added_to_cart", nil, 1, agora.Add(-20*time.Second))
	seedComentario(t, eventID, userID, "escondido", "added_to_cart", nil, 1, agora.Add(-10*time.Second))
	if _, err := testPool.Exec(ctx,
		`UPDATE live_comments SET deleted_at = now() WHERE text = 'apagado' AND event_id = $1`,
		eventID); err != nil {
		t.Fatalf("apagando: %v", err)
	}
	if _, err := testPool.Exec(ctx,
		`UPDATE live_comments SET hidden = true WHERE text = 'escondido' AND event_id = $1`,
		eventID); err != nil {
		t.Fatalf("escondendo: %v", err)
	}

	rows, err := order.NewRepository(testPool).GetCustomerComments(ctx, cartID, userID)
	if err != nil {
		t.Fatalf("GetCustomerComments: %v", err)
	}
	if len(rows) != 1 || rows[0].Text != "fica" {
		t.Fatalf("comentários = %+v, queria só o 'fica'", rows)
	}
}

// O CONTRATO COM O FRONT.
//
// Os campos novos só valem se chegarem com o NOME que a tela espera. Um tag
// JSON trocado compila, passa nos testes de repositório e some em silêncio na
// tela — o comentário volta a ser cinza e ninguém liga uma coisa à outra.
func TestJSONDoComentarioCarregaODesfecho(t *testing.T) {
	resp := order.NewOrderDetailResponse(order.OrderDetailOutput{
		Comments: []order.CommentOutput{{
			ID:             "c1",
			Text:           "2074 x2",
			CreatedAt:      time.Now(),
			Result:         "added_to_cart",
			ProductName:    "Vela LED",
			ProductKeyword: "2074",
			Quantity:       2,
		}},
	})

	b, err := json.Marshal(resp.Comments[0])
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}

	for chave, esperado := range map[string]any{
		"result":         "added_to_cart",
		"productName":    "Vela LED",
		"productKeyword": "2074",
		"quantity":       float64(2),
	} {
		if m[chave] != esperado {
			t.Errorf("%s = %v (%T), queria %v — o front lê exatamente esta chave",
				chave, m[chave], m[chave], esperado)
		}
	}
}
