package integration

// O WEBHOOK DO BLING É POR CONTA, NÃO POR PEDIDO NOSSO.
//
// O Bling registra UMA URL de webhook por APLICATIVO e dispara evento de TODO
// pedido da conta: os que o lojista digita à mão, os do site dele, os de
// marketplace — e, quando duas instalações compartilham a conta, os da outra.
//
// Ler a situação de cada um custa uma requisição do teto de 3 req/s POR CONTA.
// Durante uma live esse é o MESMO teto que cria os pedidos da venda: toda
// leitura desperdiçada aqui é uma escrita que falta lá.
//
// O vínculo pedido→carrinho está no nosso banco, indexado. Perguntar a ele é
// grátis. Este teste trava a ordem: a pergunta grátis vem antes da cara.
//
//	TEST_DATABASE_URL='postgres://livecart:livecart@localhost:5432/livecart?sslmode=disable' \
//	go test -run TestBlingPedido -v ./apps/api/internal/integration/

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"livecart/apps/api/internal/integration/providers"
)

// semearCarrinhoComPedidoNoERP cria loja+evento+carrinho já vinculado a um
// pedido do ERP, e devolve a integração Bling daquela loja.
func semearCarrinhoComPedidoNoERP(t *testing.T, pedidoID string) (*IntegrationRow, string) {
	t.Helper()
	ctx := context.Background()
	n := fmt.Sprintf("%d", time.Now().UnixNano())

	var storeID, eventID, cartID string
	if err := testPool.QueryRow(ctx,
		`INSERT INTO stores (name, slug) VALUES ('PosseBling','posse-'||$1) RETURNING id::text`, n,
	).Scan(&storeID); err != nil {
		t.Fatalf("seed store: %v", err)
	}
	if err := testPool.QueryRow(ctx,
		`INSERT INTO live_events (store_id, status, title, ends_at)
		 VALUES ($1,'live','Live', now() + interval '4 hours') RETURNING id::text`, storeID,
	).Scan(&eventID); err != nil {
		t.Fatalf("seed event: %v", err)
	}
	if err := testPool.QueryRow(ctx,
		`INSERT INTO carts (event_id, platform_user_id, platform_handle, token, short_id,
		     status, payment_status, external_order_id)
		 VALUES ($1,'buyer','@buyer','tok-'||$2,(floor(random()*90000)+10000)::int,
		         'checkout','pending',$3)
		 RETURNING id::text`, eventID, n, pedidoID,
	).Scan(&cartID); err != nil {
		t.Fatalf("seed cart: %v", err)
	}

	return &IntegrationRow{
		ID:       "int-posse",
		StoreID:  storeID,
		Provider: "bling",
		Type:     string(providers.ProviderTypeERP),
	}, cartID
}

// Pedido que NÃO é de nenhum carrinho da loja não pode custar requisição.
//
// A prova é indireta e por isso vem em par com o controle abaixo: a integração
// é de mentira (sem credencial), então QUALQUER caminho que chegue a montar o
// provider falha. Voltar `nil` só é possível se a leitura parou antes disso.
func TestBlingPedidoAlheioNaoGastaRequisicaoDoERP(t *testing.T) {
	requireDB(t)
	svc := resilienceTestService(t)
	integracao, _ := semearCarrinhoComPedidoNoERP(t, "PEDIDO-NOSSO-1")

	err := svc.observarSituacaoDoPedidoBling(
		context.Background(), integracao, "PEDIDO-DE-OUTRO-CANAL", "order.updated")

	if err != nil {
		t.Fatalf("pedido alheio devolveu erro (logo tentou ler do ERP): %v", err)
	}
}

// O CONTROLE. Sem ele, o teste acima passaria mesmo se a função tivesse virado
// `return nil` — e passaria pelo motivo errado.
//
// Aqui o pedido É nosso, então a leitura TEM de seguir adiante e esbarrar na
// integração de mentira. O erro é o sinal de que o portão deixou passar.
func TestBlingPedidoNossoSegueParaALeitura(t *testing.T) {
	requireDB(t)
	svc := resilienceTestService(t)
	integracao, _ := semearCarrinhoComPedidoNoERP(t, "PEDIDO-NOSSO-2")

	err := svc.observarSituacaoDoPedidoBling(
		context.Background(), integracao, "PEDIDO-NOSSO-2", "order.updated")

	if err == nil {
		t.Fatal("pedido nosso voltou sem erro — o portão está barrando o que deveria passar")
	}
	if !strings.Contains(err.Error(), "provider") {
		t.Errorf("parou por outro motivo que não a montagem do provider: %v", err)
	}
}
