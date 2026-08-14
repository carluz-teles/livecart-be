package integration

// Simulação em escala: 50 produtos com estoques variados, 50+ carrinhos, e um
// Tiny simulado do outro lado do de-para.
//
// As simulações menores provam mecanismos isolados. Esta procura o que só
// aparece no volume: uma linha empilhada em mil operações, uma unidade que
// escapa quando quinze carrinhos disputam o mesmo SKU, um estorno que passa
// duas vezes porque a ordem foi outra.
//
// O DE-PARA é explícito e é metade do teste. Cada produto nosso tem um
// `external_id` que é o código no Tiny, e todo movimento viaja por ele. Trocar
// os dois lados é um erro que o teste pequeno não pega, porque com um produto
// só qualquer mapeamento acerta.
//
// O invariante, por produto e não no agregado: `estoque local + reservas
// ativas` volta ao inicial depois de tudo cancelado, e o saldo no Tiny bate com
// o que a nossa contabilidade diz. Em 12/08 o agregado fechava (41 saídas, 41
// entradas) enquanto dois produtos estavam errados com sinais opostos — por
// isso a verificação é uma a uma.
//
// Rodar:
//
//	docker compose up -d postgres
//	TEST_DATABASE_URL='postgres://livecart:livecart@localhost:5432/livecart?sslmode=disable' \
//	go test -race -run TestSimulacaoEscala -v ./apps/api/internal/integration/

import (
	"context"
	"fmt"
	"math/rand"
	"sync"
	"testing"

	"livecart/apps/api/db/sqlc"
	"livecart/apps/api/internal/erp"
)

// tinyEscala é o ERP do outro lado do de-para. Guarda saldo por CÓDIGO EXTERNO,
// nunca por id nosso — se o de-para estiver invertido em algum caminho, o
// movimento cai no produto errado e o teste acusa.
//
// Aceita saldo negativo sem reclamar, como o Tiny de verdade: lançar sobre 0
// leva a -1 sem erro. É por isso que o porteiro tem de ser nosso.
type tinyEscala struct {
	mu        sync.Mutex
	saldo     map[string]int
	inicial   map[string]int
	saidas    map[string]int
	entradas  map[string]int
	negativou []string
}

func novoTinyEscala() *tinyEscala {
	return &tinyEscala{
		saldo: map[string]int{}, inicial: map[string]int{},
		saidas: map[string]int{}, entradas: map[string]int{},
	}
}

func (t *tinyEscala) cadastrar(externalID string, qtd int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.saldo[externalID] = qtd
	t.inicial[externalID] = qtd
}

func (t *tinyEscala) saida(externalID string, qtd int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.saldo[externalID] -= qtd
	t.saidas[externalID] += qtd
	if t.saldo[externalID] < 0 {
		t.negativou = append(t.negativou, externalID)
	}
}

func (t *tinyEscala) entrada(externalID string, qtd int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.saldo[externalID] += qtd
	t.entradas[externalID] += qtd
}

func (t *tinyEscala) ler(externalID string) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.saldo[externalID]
}

// produtoSim carrega o de-para dos dois lados.
type produtoSim struct {
	id         string // nosso
	externalID string // do Tiny
	inicial    int
}

func seedProdutosVariados(t *testing.T, fx scaleFixture, n int, rng *rand.Rand) []produtoSim {
	t.Helper()
	ctx := context.Background()
	out := make([]produtoSim, 0, n)

	for i := 0; i < n; i++ {
		// Estoques variados de propósito: 1 unidade expõe disputa máxima, 100
		// expõe acúmulo. Os extremos são onde os defeitos moram.
		estoque := []int{1, 2, 3, 5, 8, 13, 21, 50, 100}[rng.Intn(9)]
		externalID := fmt.Sprintf("TINY-%04d", i)

		var id string
		if err := testPool.QueryRow(ctx, `
			INSERT INTO products (store_id, name, keyword, price, stock, active, external_id, external_source)
			VALUES ($1::uuid, $2, $3, 1000, $4, true, $5, 'tiny')
			RETURNING id::text`,
			fx.storeID,
			fmt.Sprintf("Produto Simulado %d", i),
			fmt.Sprintf("%04d", 1000+i),  // keyword é character(4): é o número que o comprador digita na live
			estoque, externalID,
		).Scan(&id); err != nil {
			t.Fatalf("seed produto %d: %v", i, err)
		}
		// Liga o produto ao evento, como um produto vendável da live.
		if _, err := testPool.Exec(ctx, `
			INSERT INTO session_products (session_id, product_id, max_quantity)
			SELECT ls.id, $2::uuid, 0 FROM live_sessions ls WHERE ls.event_id = $1::uuid
			ON CONFLICT DO NOTHING`, fx.eventID, id); err != nil {
			// A tabela pode não existir neste fixture; não é o alvo do teste.
			_ = err
		}
		out = append(out, produtoSim{id: id, externalID: externalID, inicial: estoque})
	}
	return out
}

func seedCarrinhos(t *testing.T, fx scaleFixture, n int) []string {
	t.Helper()
	ctx := context.Background()
	out := make([]string, 0, n)
	for i := 0; i < n; i++ {
		var id string
		if err := testPool.QueryRow(ctx, `
			INSERT INTO carts (event_id, platform_user_id, platform_handle, token, short_id, status, payment_status)
			VALUES ($1::uuid, $2, $3, $4, (floor(random()*2000000000))::int, 'active', 'unpaid')
			RETURNING id::text`,
			fx.eventID,
			fmt.Sprintf("user-esc-%d", i),
			fmt.Sprintf("handle%d", i),
			fmt.Sprintf("tok-esc-%d-%d", i, rand.Intn(1_000_000)),
		).Scan(&id); err != nil {
			t.Fatalf("seed carrinho %d: %v", i, err)
		}
		out = append(out, id)
	}
	return out
}

// contabilidade é o que o LiveCart acredita sobre um produto.
func contabilidade(t *testing.T, productID string) (local, reservado int) {
	t.Helper()
	if err := testPool.QueryRow(context.Background(), `
		SELECT p.stock,
		       COALESCE((SELECT SUM(sr.quantity) FROM stock_reservations sr
		                 WHERE sr.product_id = p.id AND sr.status = 'active'), 0)::int
		FROM products p WHERE p.id = $1::uuid`, productID).Scan(&local, &reservado); err != nil {
		t.Fatalf("lendo contabilidade: %v", err)
	}
	return local, reservado
}

func TestSimulacaoEscalaCinquentaProdutosCinquentaCarrinhos(t *testing.T) {
	requireDB(t)
	ctx := context.Background()

	// Semente fixa: uma falha aqui tem de ser reproduzível.
	rng := rand.New(rand.NewSource(20260812))

	fx := seedScaleEvent(t)
	repo := NewRepository(sqlc.New(testPool), testPool)

	const nProdutos = 50
	const nCarrinhos = 60

	produtos := seedProdutosVariados(t, fx, nProdutos, rng)
	carrinhos := seedCarrinhos(t, fx, nCarrinhos)

	tiny := novoTinyEscala()
	for _, p := range produtos {
		tiny.cadastrar(p.externalID, p.inicial)
	}

	// --- FASE 1: a live. Compradores comentam, repetem, e o checkout mexe. ----
	type operacao struct {
		cart    string
		produto produtoSim
		tipo    string
		qtd     int
	}

	var (
		wg        sync.WaitGroup
		mu        sync.Mutex
		aplicadas []operacao
	)

	for _, cartID := range carrinhos {
		// Cada carrinho mexe em 3 produtos sorteados.
		for k := 0; k < 3; k++ {
			p := produtos[rng.Intn(len(produtos))]
			qtd := 1 + rng.Intn(3)

			wg.Add(1)
			go func(cartID string, p produtoSim, qtd int) {
				defer wg.Done()

				// O GATE. É a única coisa que impede vender a descoberto: um
				// UPDATE condicional (WHERE stock >= qtd) que recusa quando não
				// há unidade. A camada de reserva não tem teto próprio, e o Tiny
				// aceita saldo negativo sem reclamar — então tudo depende daqui.
				if err := repo.DecrementProductStock(ctx, p.id, qtd); err != nil {
					return // estoque insuficiente: o comprador entra na fila, não reserva
				}

				// Só depois de o estoque ser nosso, a reserva e o movimento.
				// O de-para viaja aqui.
				if _, err := repo.UpsertActiveReservationQuantity(ctx, erp.UpsertReservationParams{
					EventID: fx.eventID, CartID: cartID, ProductID: p.id,
					ExternalProductID: p.externalID, IncQty: qtd,
				}); err != nil {
					t.Errorf("reserva (%s/%s): %v", cartID[:8], p.externalID, err)
					return
				}
				tiny.saida(p.externalID, qtd)
				mu.Lock()
				aplicadas = append(aplicadas, operacao{cartID, p, "reserva", qtd})
				mu.Unlock()
			}(cartID, p, qtd)
		}
	}
	wg.Wait()

	// --- FASE 2: alterações no checkout, concorrentes -----------------------
	for _, op := range append([]operacao{}, aplicadas...) {
		if rng.Intn(3) != 0 {
			continue // só um terço mexe no checkout
		}
		wg.Add(1)
		go func(op operacao) {
			defer wg.Done()
			baixa := 1
			dec, err := repo.DecrementActiveReservationQuantity(ctx, op.cart, op.produto.id, baixa)
			if err != nil {
				t.Errorf("redução no checkout (%s/%s): %v", op.cart[:8], op.produto.externalID, err)
				return
			}
			// O movimento no ERP só sai quando o banco confirmou — a ordem que a
			// correção instituiu.
			if dec.Applied {
				if err := repo.IncrementProductStock(ctx, op.produto.id, baixa); err != nil {
					t.Errorf("devolvendo ao estoque local: %v", err)
				}
				tiny.entrada(op.produto.externalID, baixa)
			}
		}(op)
	}
	wg.Wait()

	// --- FASE 3: adição de novos produtos a carrinhos já existentes ----------
	// Cobre "adição de novos" com reserva já viva no mesmo carrinho.
	for i := 0; i < nCarrinhos/2; i++ {
		cartID := carrinhos[rng.Intn(len(carrinhos))]
		p := produtos[rng.Intn(len(produtos))]
		wg.Add(1)
		go func(cartID string, p produtoSim) {
			defer wg.Done()
			if err := repo.DecrementProductStock(ctx, p.id, 1); err != nil {
				return // esgotado: vai para a fila
			}
			if _, err := repo.UpsertActiveReservationQuantity(ctx, erp.UpsertReservationParams{
				EventID: fx.eventID, CartID: cartID, ProductID: p.id,
				ExternalProductID: p.externalID, IncQty: 1,
			}); err != nil {
				t.Errorf("adição tardia (%s/%s): %v", cartID[:8], p.externalID, err)
				return
			}
			tiny.saida(p.externalID, 1)
		}(cartID, p)
	}
	wg.Wait()

	// Nenhuma linha empilhada, em produto nenhum.
	var duplicadas int
	if err := testPool.QueryRow(ctx, `
		SELECT COUNT(*) FROM (
			SELECT cart_id, product_id FROM stock_reservations
			WHERE status = 'active' AND event_id = $1::uuid
			GROUP BY cart_id, product_id HAVING COUNT(*) > 1
		) d`, fx.eventID).Scan(&duplicadas); err != nil {
		t.Fatalf("procurando linhas empilhadas: %v", err)
	}
	if duplicadas > 0 {
		t.Errorf("%d pares (carrinho, produto) com mais de uma reserva ativa — "+
			"empilhar linha é estoque contado duas vezes", duplicadas)
	}

	// --- FASE 4: cancelamento de TUDO ---------------------------------------
	for _, cartID := range carrinhos {
		wg.Add(1)
		go func(cartID string) {
			defer wg.Done()
			// Lê o que o carrinho segura ANTES de estornar, para espelhar no Tiny.
			rows, err := testPool.Query(ctx, `
				SELECT p.external_id, p.id::text, sr.quantity FROM stock_reservations sr
				JOIN products p ON p.id = sr.product_id
				WHERE sr.cart_id = $1::uuid AND sr.status = 'active'`, cartID)
			if err != nil {
				t.Errorf("lendo reservas do carrinho: %v", err)
				return
			}
			type par struct {
				ext       string
				produtoID string
				qtd       int
			}
			var pares []par
			for rows.Next() {
				var p par
				if err := rows.Scan(&p.ext, &p.produtoID, &p.qtd); err != nil {
					rows.Close()
					t.Errorf("scan: %v", err)
					return
				}
				pares = append(pares, p)
			}
			rows.Close()

			if err := repo.ReverseReservationsByCart(ctx, cartID); err != nil {
				t.Errorf("estorno do carrinho %s: %v", cartID[:8], err)
				return
			}
			for _, p := range pares {
				if err := repo.IncrementProductStock(ctx, p.produtoID, p.qtd); err != nil {
					t.Errorf("creditando estoque local no cancelamento: %v", err)
				}
				tiny.entrada(p.ext, p.qtd)
			}
		}(cartID)
	}
	wg.Wait()

	// --- VERIFICAÇÃO, PRODUTO A PRODUTO -------------------------------------
	//
	// Uma a uma, e não no agregado: em 12/08 o agregado fechava (41 saídas, 41
	// entradas) com dois produtos errados em sinais opostos.
	errados := 0
	for _, p := range produtos {
		local, reservado := contabilidade(t, p.id)

		if reservado != 0 {
			t.Errorf("%s (%s): %d unidades ainda reservadas depois de cancelar tudo",
				p.externalID, p.id[:8], reservado)
			errados++
			continue
		}
		if local != p.inicial {
			t.Errorf("%s: estoque local = %d, quero %d — cancelar tudo tem de devolver "+
				"o saldo ao inicial", p.externalID, local, p.inicial)
			errados++
		}
		if got := tiny.ler(p.externalID); got != p.inicial {
			t.Errorf("%s: saldo no TINY = %d, quero %d (saídas %d, entradas %d) — "+
				"cada unidade de diferença é oversell ou venda perdida",
				p.externalID, got, p.inicial, tiny.saidas[p.externalID], tiny.entradas[p.externalID])
			errados++
		}
	}

	if len(tiny.negativou) > 0 {
		t.Errorf("%d produtos ficaram com saldo NEGATIVO no Tiny: %v — o Tiny aceita "+
			"sem reclamar, então quem tinha de barrar éramos nós",
			len(tiny.negativou), tiny.negativou[:primeiros(5, len(tiny.negativou))])
	}

	t.Logf("simulação: %d produtos, %d carrinhos, %d reservas na fase 1, %d produtos com desvio",
		nProdutos, nCarrinhos, len(aplicadas), errados)
}

func primeiros(n, total int) int {
	if n < total {
		return n
	}
	return total
}

// O de-para errado é o defeito que teste pequeno nunca pega: com um produto só,
// qualquer mapeamento acerta. Aqui os movimentos de 50 SKUs viajam junto, e um
// cruzamento manda unidade para o produto errado — cada um fica com o saldo do
// outro e o agregado continua fechando.
func TestSimulacaoDeParaNaoCruzaProdutos(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	rng := rand.New(rand.NewSource(7))

	fx := seedScaleEvent(t)
	repo := NewRepository(sqlc.New(testPool), testPool)

	const n = 30
	produtos := seedProdutosVariados(t, fx, n, rng)
	carrinhos := seedCarrinhos(t, fx, n)

	tiny := novoTinyEscala()
	for _, p := range produtos {
		tiny.cadastrar(p.externalID, p.inicial)
	}

	// Cada carrinho pega EXATAMENTE um produto, na mesma posição. Se o de-para
	// cruzar em qualquer camada, o saldo cai no vizinho e a verificação por
	// produto acusa — mesmo com o total certo.
	var wg sync.WaitGroup
	for i := range produtos {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			p := produtos[i]
			if err := repo.DecrementProductStock(ctx, p.id, 1); err != nil {
				return
			}
			if _, err := repo.UpsertActiveReservationQuantity(ctx, erp.UpsertReservationParams{
				EventID: fx.eventID, CartID: carrinhos[i], ProductID: p.id,
				ExternalProductID: p.externalID, IncQty: 1,
			}); err != nil {
				t.Errorf("reserva: %v", err)
				return
			}
			tiny.saida(p.externalID, 1)
		}(i)
	}
	wg.Wait()

	// Cada produto tem de ter exatamente 1 unidade reservada, e o external_id
	// gravado na reserva tem de ser o DELE.
	for i, p := range produtos {
		var ext string
		var qtd int
		err := testPool.QueryRow(ctx, `
			SELECT sr.external_product_id, sr.quantity FROM stock_reservations sr
			WHERE sr.cart_id = $1::uuid AND sr.product_id = $2::uuid AND sr.status = 'active'`,
			carrinhos[i], p.id).Scan(&ext, &qtd)
		if err != nil {
			// Produto sem estoque não reservou; é resultado legítimo.
			continue
		}
		if ext != p.externalID {
			t.Errorf("de-para cruzado: produto %s gravou external_id %q, esperado %q — "+
				"movimento cairia no SKU errado no Tiny", p.id[:8], ext, p.externalID)
		}
		if qtd != 1 {
			t.Errorf("%s reservou %d, quero 1", p.externalID, qtd)
		}
	}
}

// Expiração e pagamento são os dois outros finais possíveis do carrinho, e cada
// um trata o estoque de forma oposta: expirar DEVOLVE, pagar CONVERTE (a unidade
// foi vendida e não volta). Confundir os dois é oversell ou estoque preso.
func TestSimulacaoExpiracaoDevolveEPagamentoConverte(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	rng := rand.New(rand.NewSource(11))

	fx := seedScaleEvent(t)
	repo := NewRepository(sqlc.New(testPool), testPool)

	produtos := seedProdutosVariados(t, fx, 20, rng)
	carrinhos := seedCarrinhos(t, fx, 20)

	// Metade vai expirar, metade vai ser paga.
	for i, p := range produtos {
		if err := repo.DecrementProductStock(ctx, p.id, 1); err != nil {
			t.Fatalf("gate do produto %s: %v", p.externalID, err)
		}
		if _, err := repo.UpsertActiveReservationQuantity(ctx, erp.UpsertReservationParams{
			EventID: fx.eventID, CartID: carrinhos[i], ProductID: p.id,
			ExternalProductID: p.externalID, IncQty: 1,
		}); err != nil {
			t.Fatalf("reserva: %v", err)
		}
	}

	for i, p := range produtos {
		if i%2 == 0 {
			// EXPIRAÇÃO: estorna e devolve ao estoque local.
			if err := repo.ReverseReservationsByCart(ctx, carrinhos[i]); err != nil {
				t.Fatalf("estorno da expiração: %v", err)
			}
			if err := repo.IncrementProductStock(ctx, p.id, 1); err != nil {
				t.Fatalf("devolvendo ao local: %v", err)
			}
		} else {
			// PAGAMENTO: a reserva vira conversão. A unidade NÃO volta — foi
			// vendida de verdade.
			if _, err := testPool.Exec(ctx, `
				UPDATE stock_reservations SET status = 'converted', reversed_at = now()
				WHERE cart_id = $1::uuid AND status = 'active'`, carrinhos[i]); err != nil {
				t.Fatalf("convertendo reserva: %v", err)
			}
		}
	}

	for i, p := range produtos {
		local, reservado := contabilidade(t, p.id)
		if reservado != 0 {
			t.Errorf("%s: %d ainda reservado depois do desfecho", p.externalID, reservado)
		}
		if i%2 == 0 {
			// Expirou: a unidade voltou.
			if local != p.inicial {
				t.Errorf("%s expirou e o estoque ficou em %d, quero %d — expirar devolve",
					p.externalID, local, p.inicial)
			}
		} else {
			// Pagou: a unidade saiu de vez.
			if local != p.inicial-1 {
				t.Errorf("%s foi pago e o estoque ficou em %d, quero %d — pagar não devolve, "+
					"a peça foi vendida", p.externalID, local, p.inicial-1)
			}
		}
	}
}
