package integration

// Simulação de uma LIVE inteira contra o Postgres de verdade, com o Tiny
// disparando webhooks em tempos distintos.
//
// O que se quer saber não é se cada peça funciona isolada — isso os outros
// testes cobrem —, e sim como o conjunto REAGE quando as coisas chegam fora de
// ordem, que é o que acontece em produção: o comprador comenta, o checkout
// mexe na quantidade, o Tiny responde com atraso variável, e no meio disso o
// lojista vende o mesmo SKU no Mercado Livre.
//
// O invariante, ao fim de tudo: `estoque local + reservas ativas` volta ao que
// era antes, descontado o que saiu por outro canal. Nem uma unidade a mais
// (oversell) nem a menos (venda perdida).
//
// Rodar:
//
//	docker compose up -d postgres
//	TEST_DATABASE_URL='postgres://livecart:livecart@localhost:5432/livecart?sslmode=disable' \
//	go test -race -run TestSimulacaoLive -v ./apps/api/internal/integration/

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"livecart/apps/api/db/sqlc"
	"livecart/apps/api/internal/erp"

	"go.uber.org/zap"
)

// estoqueContabilizado é a soma que tem de fechar: o que está na prateleira mais
// o que seguramos no ERP.
func estoqueContabilizado(t *testing.T, productID string) int {
	t.Helper()
	var local, reservado int
	if err := testPool.QueryRow(context.Background(), `
		SELECT p.stock,
		       COALESCE((SELECT SUM(sr.quantity) FROM stock_reservations sr
		                 WHERE sr.product_id = p.id AND sr.status = 'active'), 0)::int
		FROM products p WHERE p.id = $1::uuid`, productID).Scan(&local, &reservado); err != nil {
		t.Fatalf("lendo estoque contabilizado: %v", err)
	}
	return local + reservado
}

// Uma live com vários compradores mexendo no carrinho ao mesmo tempo.
func TestSimulacaoLiveComComprasEAlteracoesConcorrentes(t *testing.T) {
	requireDB(t)
	ctx := context.Background()

	fx := seedScaleEvent(t)
	const inicial = 20
	productID := seedSoldOutProductWithQueue(t, fx, inicial, 0)
	repo := NewRepository(sqlc.New(testPool), testPool)

	if got := estoqueContabilizado(t, productID); got != inicial {
		t.Fatalf("partida: contabilizado %d, quero %d", got, inicial)
	}

	// Seis compradores, cada um com o seu carrinho, agindo ao mesmo tempo.
	const compradores = 6
	carrinhos := make([]string, compradores)
	for i := range carrinhos {
		if err := testPool.QueryRow(ctx, `
			INSERT INTO carts (event_id, platform_user_id, platform_handle, token, short_id, status, payment_status)
			VALUES ($1, 'u-'||$2, 'h-'||$2, 't-'||$2||'-'||floor(random()*1000000)::text,
			        (floor(random()*2000000000))::int, 'active', 'unpaid')
			RETURNING id::text`, fx.eventID, fmt.Sprintf("sim%d", i)).Scan(&carrinhos[i]); err != nil {
			t.Fatalf("seed cart %d: %v", i, err)
		}
	}

	var wg sync.WaitGroup
	var mu sync.Mutex
	reservadoTotal := 0

	for i, cartID := range carrinhos {
		wg.Add(1)
		go func(i int, cartID string) {
			defer wg.Done()

			// Comentário na live: reserva 2.
			out, err := repo.UpsertActiveReservationQuantity(ctx, erp.UpsertReservationParams{
				EventID: fx.eventID, CartID: cartID, ProductID: productID,
				ExternalProductID: "EXT-SIM", IncQty: 2,
			})
			if err != nil {
				t.Errorf("comprador %d, reserva inicial: %v", i, err)
				return
			}
			_ = out
			mu.Lock()
			reservadoTotal += 2
			mu.Unlock()

			// Comentário repetido: mais 1.
			if _, err := repo.UpsertActiveReservationQuantity(ctx, erp.UpsertReservationParams{
				EventID: fx.eventID, CartID: cartID, ProductID: productID,
				ExternalProductID: "EXT-SIM", IncQty: 1,
			}); err != nil {
				t.Errorf("comprador %d, comentário repetido: %v", i, err)
				return
			}
			mu.Lock()
			reservadoTotal++
			mu.Unlock()

			// No checkout, metade reduz 1 unidade.
			if i%2 == 0 {
				dec, err := repo.DecrementActiveReservationQuantity(ctx, cartID, productID, 1)
				if err != nil {
					t.Errorf("comprador %d, redução: %v", i, err)
					return
				}
				if dec.Applied {
					mu.Lock()
					reservadoTotal--
					mu.Unlock()
				}
			}
		}(i, cartID)
	}
	wg.Wait()

	// Uma linha ativa por carrinho, sempre.
	var linhas int
	if err := testPool.QueryRow(ctx, `
		SELECT count(*) FROM stock_reservations
		WHERE product_id = $1::uuid AND status = 'active'`, productID).Scan(&linhas); err != nil {
		t.Fatalf("contando reservas: %v", err)
	}
	if linhas != compradores {
		t.Errorf("%d linhas ativas para %d carrinhos — linhas empilhadas viram "+
			"estoque contado duas vezes", linhas, compradores)
	}

	var somaReservas int
	if err := testPool.QueryRow(ctx, `
		SELECT COALESCE(SUM(quantity),0)::int FROM stock_reservations
		WHERE product_id = $1::uuid AND status = 'active'`, productID).Scan(&somaReservas); err != nil {
		t.Fatalf("somando reservas: %v", err)
	}
	if somaReservas != reservadoTotal {
		t.Errorf("reservas somam %d, as operações pediram %d — cada unidade de "+
			"diferença é oversell ou venda perdida", somaReservas, reservadoTotal)
	}

	// Cancelamento de todos: tudo volta.
	for _, cartID := range carrinhos {
		if err := repo.ReverseReservationsByCartAndProduct(ctx, cartID, productID); err != nil {
			t.Fatalf("estorno: %v", err)
		}
	}

	var ativas int
	if err := testPool.QueryRow(ctx, `
		SELECT count(*) FROM stock_reservations
		WHERE product_id = $1::uuid AND status = 'active'`, productID).Scan(&ativas); err != nil {
		t.Fatalf("contando ativas: %v", err)
	}
	if ativas != 0 {
		t.Errorf("%d reservas continuam ativas depois de cancelar todos os carrinhos", ativas)
	}
}

// O webhook do Tiny chegando em tempos distintos durante a live.
//
// Cada caso muda QUANDO o saldo do ERP chega em relação aos nossos movimentos.
// O desenho não pode depender dessa ordem: em produção os atrasos variaram de
// 1 a 50 segundos no mesmo dia.
//
// Antes esta pergunta era respondida por uma janela de tempo — suprimir o
// webhook por alguns segundos depois de mandarmos um movimento. Uma janela erra
// dos dois lados: curta demais não protege, longa demais cega o LiveCart para o
// lojista vendendo o mesmo SKU em outro canal. Quem responde agora é o
// comprovante da leitura: o saldo entra se ninguém nosso mexeu desde que ele foi
// lido, e não entra se mexeu. Sem prazo, sem tolerância.
func TestSimulacaoWebhookDoTinyEmTemposDistintos(t *testing.T) {
	requireDB(t)
	ctx := context.Background()

	for _, caso := range []struct {
		nome                  string
		webhookAntesDoEstorno bool
	}{
		{"webhook chega ANTES do estorno", true},
		{"webhook chega DEPOIS do estorno", false},
	} {
		t.Run(caso.nome, func(t *testing.T) {
			fx := seedScaleEvent(t)
			const inicial = 10
			productID := seedSoldOutProductWithQueue(t, fx, inicial, 0)
			repo := NewRepository(sqlc.New(testPool), testPool)

			cartID, _ := seedReservaAtiva(t, fx, productID, 3)

			// O comprovante, tirado no instante em que o Tiny foi consultado.
			seqNaLeitura := seqDoProduto(t, productID)

			if caso.webhookAntesDoEstorno {
				// Um movimento nosso aterrissa entre a consulta e a escrita. O
				// saldo que está a caminho descreve o estado ANTERIOR a ele;
				// aplicá-lo repõe a unidade que acabou de sair e o produto volta
				// a oferecer o que já não tem.
				if err := repo.DecrementProductStock(ctx, productID, 1); err != nil {
					t.Fatalf("movimento nosso: %v", err)
				}
				aplicou, err := repo.ApplyERPStockMirror(ctx, productID, inicial, seqNaLeitura)
				if err != nil {
					t.Fatalf("webhook: %v", err)
				}
				if aplicou {
					t.Error("o saldo lido ANTES do nosso movimento entrou — é o eco da " +
						"nossa própria saída, e aplicá-lo desfaz a reserva do comprador")
				}
				if got := estoqueDoProduto(t, productID); got != inicial-1 {
					t.Errorf("estoque = %d, quero %d: o movimento manda, a leitura velha não",
						got, inicial-1)
				}
			}

			// Estorno do carrinho.
			if err := repo.ReverseReservationsByCartAndProduct(ctx, cartID, productID); err != nil {
				t.Fatalf("estorno: %v", err)
			}

			if !caso.webhookAntesDoEstorno {
				// Nada nosso mexeu desde a consulta. O saldo do Tiny é notícia
				// legítima — o lojista vendeu 2 no marketplace — e tem de entrar.
				// É a única forma de sabermos dos outros canais dele.
				aplicou, err := repo.ApplyERPStockMirror(ctx, productID, inicial-2, seqNaLeitura)
				if err != nil {
					t.Fatalf("webhook: %v", err)
				}
				if !aplicou {
					t.Fatal("o saldo do ERP foi recusado sem nenhum movimento nosso no meio — " +
						"assim a venda do lojista em outro canal nunca chega, que era " +
						"exatamente o buraco da janela de tempo")
				}
				if got := estoqueDoProduto(t, productID); got != inicial-2 {
					t.Errorf("estoque = %d, quero %d", got, inicial-2)
				}
			}
		})
	}
}

func zapNopLogger() *zap.Logger { return zap.NewNop() }
