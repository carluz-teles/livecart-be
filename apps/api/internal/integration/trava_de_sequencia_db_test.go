package integration

// A trava que substitui as heurísticas de janela.
//
// O problema é de ORDENAÇÃO, não de tempo. Nós mandamos deltas ao ERP ("saiu
// 1") e ele responde com saldo absoluto por webhook. O payload do Tiny é
// `{cnpj, tipo, dados{sku, nome, saldo, idProduto}, versao}` — sem timestamp,
// sem sequência. Dois números na mesma linha do tempo e nada que diga qual veio
// primeiro.
//
// Três mecanismos tentaram conviver com essa ambiguidade, e cada um errava de um
// jeito:
//
//	suprimir enquanto houvesse reserva ativa  → cego para o Mercado Livre por 30min
//	janela fixa de 60 segundos                → erra quando a chamada demora mais
//	contar chamadas em voo                    → some com a fresta, não com o erro
//
// Todos adivinhavam quem tinha causado a mudança no saldo. O `erp_seq` resolve
// pelo único lado que controlamos: cada movimento NOSSO sobe o contador. Quem lê
// o saldo do ERP guarda o seq daquele instante; a escrita só vale se ele não
// mudou. Se mudou, a leitura descreve um passado — e é descartada, sem palpite.
//
// Custo: nenhuma janela, nenhuma tolerância, nenhum parâmetro chutado.
//
// Rodar:
//
//	docker compose up -d postgres
//	TEST_DATABASE_URL='postgres://livecart:livecart@localhost:5432/livecart?sslmode=disable' \
//	go test -race -run TestTrava -v ./apps/api/internal/integration/

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"livecart/apps/api/db/sqlc"
)

func seqDoProduto(t *testing.T, productID string) int64 {
	t.Helper()
	var seq int64
	if err := testPool.QueryRow(context.Background(),
		`SELECT erp_seq FROM products WHERE id = $1::uuid`, productID).Scan(&seq); err != nil {
		t.Fatalf("lendo seq: %v", err)
	}
	return seq
}

func estoqueDoProduto(t *testing.T, productID string) int {
	t.Helper()
	var n int
	if err := testPool.QueryRow(context.Background(),
		`SELECT stock FROM products WHERE id = $1::uuid`, productID).Scan(&n); err != nil {
		t.Fatalf("lendo stock: %v", err)
	}
	return n
}

// O caso que nenhuma janela resolvia: a leitura do ERP é feita, um movimento
// nosso acontece, e só então a leitura tenta entrar. Aplicá-la reporia a unidade
// que acabou de sair.
func TestTravaRecusaLeituraVencidaDoERP(t *testing.T) {
	requireDB(t)
	ctx := context.Background()

	fx := seedScaleEvent(t)
	const inicial = 10
	productID := seedSoldOutProductWithQueue(t, fx, inicial, 0)
	repo := NewRepository(sqlc.New(testPool), testPool)

	seqNaLeitura := seqDoProduto(t, productID)

	// Um comprador leva 1 unidade enquanto a leitura do ERP viaja.
	if err := repo.DecrementProductStock(ctx, productID, 1); err != nil {
		t.Fatalf("reservando 1: %v", err)
	}
	if estoqueDoProduto(t, productID) != inicial-1 {
		t.Fatalf("a reserva não baixou o estoque")
	}

	// A leitura antiga chega agora, trazendo o saldo de antes.
	aplicou, err := repo.ApplyERPStockMirror(ctx, productID, inicial, seqNaLeitura)
	if err != nil {
		t.Fatalf("aplicando espelho: %v", err)
	}
	if aplicou {
		t.Error("leitura vencida foi aplicada — ela repõe a unidade que o comprador " +
			"acabou de levar, e o produto volta a oferecer o que já não tem")
	}
	if got := estoqueDoProduto(t, productID); got != inicial-1 {
		t.Errorf("estoque = %d, quero %d — o movimento manda, a leitura velha não",
			got, inicial-1)
	}
}

// A contrapartida, e é o motivo de a trava existir em vez da supressão: uma
// leitura ATUAL entra sempre, mesmo com reserva viva. É assim que a venda do
// lojista no Mercado Livre chega até nós.
func TestTravaAceitaLeituraAtualMesmoComReservaViva(t *testing.T) {
	requireDB(t)
	ctx := context.Background()

	fx := seedScaleEvent(t)
	const inicial = 10
	productID := seedSoldOutProductWithQueue(t, fx, inicial, 0)
	repo := NewRepository(sqlc.New(testPool), testPool)

	// Há reserva viva: o mecanismo antigo suprimiria o webhook por causa dela.
	seedReservaAtiva(t, fx, productID, 3)
	if err := repo.DecrementProductStock(ctx, productID, 3); err != nil {
		t.Fatalf("reservando 3: %v", err)
	}

	// O lojista vende 2 no marketplace. O ERP agora diz 5 (10 - 3 nossas - 2 dele).
	seqAtual := seqDoProduto(t, productID)
	aplicou, err := repo.ApplyERPStockMirror(ctx, productID, 5, seqAtual)
	if err != nil {
		t.Fatalf("aplicando espelho: %v", err)
	}
	if !aplicou {
		t.Fatal("leitura atual foi recusada — sem ela nunca saberíamos das vendas " +
			"fora do LiveCart, que era o buraco dos mecanismos anteriores")
	}
	if got := estoqueDoProduto(t, productID); got != 5 {
		t.Errorf("estoque = %d, quero 5 — venda em outro canal tem de refletir", got)
	}
}

// Saldo negativo do ERP não é estoque, é sintoma: o Tiny aceita ir abaixo de
// zero e copiar isso propagaria o defeito.
func TestTravaNaoCopiaSaldoNegativoDoERP(t *testing.T) {
	requireDB(t)
	ctx := context.Background()

	fx := seedScaleEvent(t)
	productID := seedSoldOutProductWithQueue(t, fx, 5, 0)
	repo := NewRepository(sqlc.New(testPool), testPool)

	seq := seqDoProduto(t, productID)
	if _, err := repo.ApplyERPStockMirror(ctx, productID, -3, seq); err != nil {
		t.Fatalf("aplicando espelho negativo: %v", err)
	}
	if got := estoqueDoProduto(t, productID); got < 0 {
		t.Errorf("estoque local ficou em %d — saldo negativo do ERP é sintoma de "+
			"saída além do que existia, não um número a copiar", got)
	}
}

// Todo movimento nosso sobe o seq. Se algum caminho esquecer, a leitura seguinte
// passa a ser aceita indevidamente e a trava vira decoração.
func TestTodoMovimentoNossoSobeOSeq(t *testing.T) {
	requireDB(t)
	ctx := context.Background()

	fx := seedScaleEvent(t)
	productID := seedSoldOutProductWithQueue(t, fx, 20, 0)
	repo := NewRepository(sqlc.New(testPool), testPool)

	casos := []struct {
		nome string
		agir func() error
	}{
		{"reserva", func() error { return repo.DecrementProductStock(ctx, productID, 1) }},
		{"devolução", func() error { return repo.IncrementProductStock(ctx, productID, 1) }},
		{"promoção parcial", func() error {
			_, err := repo.DecrementProductStockUpTo(ctx, productID, 1)
			return err
		}},
	}

	for _, c := range casos {
		t.Run(c.nome, func(t *testing.T) {
			antes := seqDoProduto(t, productID)
			if err := c.agir(); err != nil {
				t.Fatalf("%s: %v", c.nome, err)
			}
			if depois := seqDoProduto(t, productID); depois <= antes {
				t.Errorf("%s não subiu o seq (%d → %d) — uma leitura do ERP anterior "+
					"a ela seria aceita como se fosse do presente", c.nome, antes, depois)
			}
		})
	}
}

// Sob concorrência: N movimentos e uma leitura antiga disputando. A leitura tem
// de perder, sempre — e o estoque tem de refletir exatamente os N movimentos.
func TestTravaSobConcorrencia(t *testing.T) {
	requireDB(t)
	ctx := context.Background()

	fx := seedScaleEvent(t)
	const inicial = 50
	productID := seedSoldOutProductWithQueue(t, fx, inicial, 0)
	repo := NewRepository(sqlc.New(testPool), testPool)

	seqNaLeitura := seqDoProduto(t, productID)

	const movimentos = 20
	var wg sync.WaitGroup
	largada := make(chan struct{})

	for i := 0; i < movimentos; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-largada
			if err := repo.DecrementProductStock(ctx, productID, 1); err != nil {
				t.Errorf("reserva concorrente: %v", err)
			}
		}()
	}
	// A leitura velha tenta entrar no meio da disputa.
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-largada
		if _, err := repo.ApplyERPStockMirror(ctx, productID, inicial, seqNaLeitura); err != nil {
			t.Errorf("aplicando espelho: %v", err)
		}
	}()

	close(largada)
	wg.Wait()

	if got := estoqueDoProduto(t, productID); got != inicial-movimentos {
		t.Errorf("estoque = %d, quero %d — se a leitura velha tivesse entrado em "+
			"qualquer ponto, o saldo teria voltado para %d e as reservas depois dela "+
			"sumiriam do contador", got, inicial-movimentos, inicial)
	}
	if seq := seqDoProduto(t, productID); seq != seqNaLeitura+movimentos {
		t.Errorf("seq = %d, quero %d — um por movimento", seq, seqNaLeitura+movimentos)
	}
}

// A ponta que fecha o ciclo: resolver o produto pelo código do ERP e ler o seq
// no MESMO instante. Se viessem de consultas separadas, caberia um movimento
// entre elas e a trava validaria contra um seq que já não é o da leitura.
func TestResolveProdutoESeqNoMesmoInstante(t *testing.T) {
	requireDB(t)
	ctx := context.Background()

	fx := seedScaleEvent(t)
	productID := seedSoldOutProductWithQueue(t, fx, 7, 0)
	repo := NewRepository(sqlc.New(testPool), testPool)

	var ext string
	if err := testPool.QueryRow(ctx,
		`SELECT external_id FROM products WHERE id = $1::uuid`, productID).Scan(&ext); err != nil {
		t.Fatalf("lendo external_id: %v", err)
	}

	gotID, gotSeq, err := repo.ProductSeqByExternalID(ctx, fx.storeID, "manual", ext)
	if err != nil {
		t.Fatalf("resolvendo: %v", err)
	}
	if gotID != productID {
		t.Errorf("resolveu %q, quero %q — de-para errado manda o movimento para o SKU vizinho",
			gotID, productID)
	}
	if gotSeq != seqDoProduto(t, productID) {
		t.Errorf("seq %d diverge do atual — as duas informações têm de vir do mesmo instante", gotSeq)
	}

	// Produto que o lojista não importou: silêncio, não erro. O ERP notifica
	// sobre o catálogo inteiro dele.
	semID, _, err := repo.ProductSeqByExternalID(ctx, fx.storeID, "manual", fmt.Sprintf("INEXISTENTE-%d", seedSeq))
	if err != nil {
		t.Errorf("produto não importado devolveu erro: %v", err)
	}
	if semID != "" {
		t.Errorf("resolveu um produto que não existe: %q", semID)
	}
}
