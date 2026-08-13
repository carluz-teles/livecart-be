package integration

// O mesmo SKU disputado por dois canais ao mesmo tempo: a live do LiveCart e o
// e-commerce do lojista, com o Tiny no meio disparando webhook a cada venda.
//
// É o cenário que o dono do produto levantou e que nenhum teste cobria: o
// lojista tem o produto anunciado no LiveCart, no Mercado Livre e no site dele.
// Vende nos três ao mesmo tempo. O Tiny agrega tudo e nos avisa por webhook,
// sempre com o SALDO ABSOLUTO — nunca com "vendeu 1".
//
// Duas coisas podem dar errado, e são opostas:
//
//	oversell     vendemos unidade que o e-commerce já tinha levado
//	sub-venda    recusamos unidade que existia, porque nosso contador ficou atrás
//
// A trava de sequência (migration 000124) é o que impede as duas sem janela de
// tolerância: uma leitura do ERP só entra se nenhum movimento nosso tiver
// acontecido depois dela.
//
// Aqui o Tiny é modelado como ele é de verdade — inclusive aceitando saldo
// negativo sem reclamar, o que significa que ele NÃO é o porteiro. Se deixarmos
// passar, ele deixa também, e a venda a descoberto só aparece no dia seguinte.
//
// Rodar:
//
//	docker compose up -d postgres
//	TEST_DATABASE_URL='postgres://livecart:livecart@localhost:5432/livecart?sslmode=disable' \
//	go test -race -run TestGuerraDeCanais -v ./apps/api/internal/integration/

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"livecart/apps/api/db/sqlc"
)

// tinyReal é o ERP: guarda o saldo verdadeiro do produto, somando o que sai por
// qualquer canal. Aceita ir abaixo de zero, como o Tiny de verdade.
type tinyReal struct {
	mu        sync.Mutex
	saldo     int
	negativou bool
	vendasWeb int
	saidasLC  int
	entradas  int
}

// venderNoEcommerce é o Mercado Livre ou o site do lojista. Só vende o que
// existe — a plataforma dele consulta o Tiny antes. Devolve se conseguiu.
func (t *tinyReal) venderNoEcommerce(qtd int) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.saldo < qtd {
		return false
	}
	t.saldo -= qtd
	t.vendasWeb += qtd
	return true
}

// saidaDoLiveCart é a nossa reserva chegando ao ERP. NÃO recusa: o Tiny aceita
// saldo negativo, e é justamente por isso que o porteiro tem de ser nosso.
func (t *tinyReal) saidaDoLiveCart(qtd int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.saldo -= qtd
	t.saidasLC += qtd
	if t.saldo < 0 {
		t.negativou = true
	}
}

func (t *tinyReal) entradaDoLiveCart(qtd int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.saldo += qtd
	t.entradas += qtd
}

func (t *tinyReal) ler() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.saldo
}

// A guerra: compradores da live e vendas no e-commerce disputando o mesmo SKU,
// com webhooks do Tiny chegando o tempo todo no meio.
func TestGuerraDeCanaisLiveContraEcommerce(t *testing.T) {
	requireDB(t)
	ctx := context.Background()

	fx := seedScaleEvent(t)
	repo := NewRepository(sqlc.New(testPool), testPool)

	for _, caso := range []struct {
		nome        string
		inicial     int
		compradores int
		vendasWeb   int
		webhooks    int
	}{
		{"disputa apertada", 10, 15, 8, 30},
		{"estoque farto", 100, 40, 30, 60},
		{"ultima unidade", 1, 20, 5, 20},
		{"e-commerce agressivo", 20, 10, 40, 50},
	} {
		t.Run(caso.nome, func(t *testing.T) {
			productID := seedSoldOutProductWithQueue(t, fx, caso.inicial, 0)
			tiny := &tinyReal{saldo: caso.inicial}

			var (
				wg            sync.WaitGroup
				mu            sync.Mutex
				vendasNaLive  int
				recusasNaLive int
			)
			largada := make(chan struct{})

			// --- Compradores da live -------------------------------------
			for i := 0; i < caso.compradores; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					<-largada
					// O porteiro: UPDATE condicional atômico. É a única coisa
					// entre a live e o oversell, porque o Tiny não recusa.
					if err := repo.DecrementProductStock(ctx, productID, 1); err != nil {
						mu.Lock()
						recusasNaLive++
						mu.Unlock()
						return
					}
					// Passou pelo porteiro: a saída vai ao ERP.
					tiny.saidaDoLiveCart(1)
					mu.Lock()
					vendasNaLive++
					mu.Unlock()
				}()
			}

			// --- Vendas no e-commerce do lojista -------------------------
			//
			// Cada venda dispara o PRÓPRIO webhook, que é como o Tiny funciona:
			// qualquer alteração de saldo gera notificação. Modelar a venda sem
			// o webhook dela seria injusto — equivaleria a supor que o ERP
			// nunca nos conta o que aconteceu.
			for i := 0; i < caso.vendasWeb; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					<-largada
					if !tiny.venderNoEcommerce(1) {
						return
					}
					// O webhook dessa venda: lê o seq, depois o saldo, e tenta
					// aplicar. A trava recusa se um movimento nosso entrou no meio.
					seq := seqDoProduto(t, productID)
					if _, err := repo.ApplyERPStockMirror(ctx, productID, tiny.ler(), seq); err != nil {
						t.Errorf("webhook da venda externa: %v", err)
					}
				}()
			}

			// --- Webhooks do Tiny, entrando no meio da disputa -----------
			//
			// Cada um lê o seq ANTES do saldo, exatamente como o handler faz.
			// A trava recusa a escrita se um movimento nosso entrou no meio.
			for i := 0; i < caso.webhooks; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					<-largada
					seq := seqDoProduto(t, productID)
					saldo := tiny.ler()
					if _, err := repo.ApplyERPStockMirror(ctx, productID, saldo, seq); err != nil {
						t.Errorf("aplicando webhook: %v", err)
					}
				}()
			}

			close(largada)
			wg.Wait()

			// --- O que É nosso, e tem de valer sempre --------------------
			//
			// A live nunca pode vender mais do que o NOSSO contador dizia haver.
			// Isso está inteiramente sob o nosso controle e é o que o porteiro
			// atômico garante: com estoque inicial N, no máximo N reservas
			// passam, por mais compradores que apertem ao mesmo tempo.
			if tiny.saidasLC > caso.inicial {
				t.Errorf("a live vendeu %d de um estoque de %d — o porteiro falhou, e "+
					"isso é falha nossa: o Tiny aceita saldo negativo sem reclamar, "+
					"então quem barra somos nós", tiny.saidasLC, caso.inicial)
			}

			// --- O que NÃO é nosso, e por isso é MEDIDO, não exigido -----
			//
			// Dois vendedores independentes sobre o mesmo estoque, com aviso
			// assíncrono, podem vender a mesma unidade. No caso da última
			// peça: nós olhamos e vemos 1, o e-commerce olha e vê 1, os dois
			// vendem. Nenhuma trava do nosso lado impede isso — só o ERP
			// recusando a segunda saída impediria, e o Tiny não recusa.
			//
			// Exigir zero aqui seria cobrar de nós algo fora do nosso alcance.
			// O que se pode fazer é medir a exposição e decidir, como produto,
			// se ela justifica uma margem de segurança (não vender as últimas N
			// unidades na live) — o que custa venda para evitar promessa
			// impossível.
			sobreposicao := 0
			if soma := tiny.saidasLC + tiny.vendasWeb; soma > caso.inicial {
				sobreposicao = soma - caso.inicial
			}
			if sobreposicao > 0 {
				t.Logf("SOBREPOSIÇÃO ENTRE CANAIS: %d unidade(s). Inicial %d, live %d, "+
					"e-commerce %d. É o limite inerente de vender o mesmo SKU em dois "+
					"lugares sem o ERP arbitrar — não um defeito do LiveCart.",
					sobreposicao, caso.inicial, tiny.saidasLC, tiny.vendasWeb)
			}

			if vendasNaLive+recusasNaLive != caso.compradores {
				t.Errorf("%d compradores viraram %d vendas + %d recusas — alguém sumiu",
					caso.compradores, vendasNaLive, recusasNaLive)
			}

			// --- Convergência: um webhook final põe todos de acordo ------
			//
			// Terminada a disputa, uma leitura atual tem de entrar e o nosso
			// contador tem de ficar igual ao do Tiny. Se a trava fosse rígida
			// demais, ela recusaria para sempre e ficaríamos defasados.
			seqFinal := seqDoProduto(t, productID)
			aplicou, err := repo.ApplyERPStockMirror(ctx, productID, tiny.ler(), seqFinal)
			if err != nil {
				t.Fatalf("webhook final: %v", err)
			}
			if !aplicou {
				t.Fatal("webhook final recusado com o sistema parado — a trava não pode " +
					"impedir a convergência, senão nunca saberíamos das vendas de fora")
			}
			// Convergência: o nosso contador tem de terminar igual ao do ERP.
			// Saldo negativo do ERP é sintoma, e o piso em zero é deliberado —
			// copiar um negativo propagaria o defeito em vez de mostrá-lo.
			esperado := tiny.ler()
			if esperado < 0 {
				esperado = 0
			}
			if got := estoqueDoProduto(t, productID); got != esperado {
				t.Errorf("depois de tudo, LiveCart diz %d e o ERP diz %d — os dois têm "+
					"de convergir quando o barulho para", got, tiny.ler())
			}

			t.Logf("%s: inicial %d | live vendeu %d, recusou %d | e-commerce vendeu %d | sobrou %d",
				caso.nome, caso.inicial, vendasNaLive, recusasNaLive, tiny.vendasWeb, tiny.ler())
		})
	}
}

// O ciclo completo com os dois canais: live vende, e-commerce vende, live
// cancela — e no fim a conta tem de fechar contra o que realmente saiu.
func TestGuerraDeCanaisComCancelamento(t *testing.T) {
	requireDB(t)
	ctx := context.Background()

	fx := seedScaleEvent(t)
	repo := NewRepository(sqlc.New(testPool), testPool)

	const inicial = 30
	productID := seedSoldOutProductWithQueue(t, fx, inicial, 0)
	tiny := &tinyReal{saldo: inicial}

	var wg sync.WaitGroup
	var mu sync.Mutex
	reservadas := 0
	largada := make(chan struct{})

	// 12 compradores na live.
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-largada
			if err := repo.DecrementProductStock(ctx, productID, 1); err != nil {
				return
			}
			tiny.saidaDoLiveCart(1)
			mu.Lock()
			reservadas++
			mu.Unlock()
		}()
	}
	// 10 vendas no e-commerce, no mesmo instante.
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); <-largada; tiny.venderNoEcommerce(1) }()
	}
	// Webhooks pipocando.
	for i := 0; i < 25; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-largada
			seq := seqDoProduto(t, productID)
			if _, err := repo.ApplyERPStockMirror(ctx, productID, tiny.ler(), seq); err != nil {
				t.Errorf("webhook: %v", err)
			}
		}()
	}
	close(largada)
	wg.Wait()

	// Metade dos carrinhos da live é cancelada.
	cancelar := reservadas / 2
	for i := 0; i < cancelar; i++ {
		if err := repo.IncrementProductStock(ctx, productID, 1); err != nil {
			t.Fatalf("cancelando: %v", err)
		}
		tiny.entradaDoLiveCart(1)
	}

	// Convergência final.
	seq := seqDoProduto(t, productID)
	if _, err := repo.ApplyERPStockMirror(ctx, productID, tiny.ler(), seq); err != nil {
		t.Fatalf("webhook final: %v", err)
	}

	esperado := inicial - (reservadas - cancelar) - tiny.vendasWeb
	if got := tiny.ler(); got != esperado {
		t.Errorf("saldo do Tiny = %d, quero %d (inicial %d − live %d efetivas − web %d)",
			got, esperado, inicial, reservadas-cancelar, tiny.vendasWeb)
	}
	if got := estoqueDoProduto(t, productID); got != tiny.ler() {
		t.Errorf("LiveCart diz %d, Tiny diz %d — têm de convergir", got, tiny.ler())
	}
	if tiny.negativou {
		t.Error("saldo foi a negativo em algum momento: vendemos o que não existia")
	}

	t.Logf("live reservou %d, cancelou %d; e-commerce vendeu %d; sobrou %d de %d",
		reservadas, cancelar, tiny.vendasWeb, tiny.ler(), inicial)
}

// Vários SKUs disputados ao mesmo tempo, para o de-para não cruzar sob pressão:
// o movimento de um produto não pode aterrissar no saldo do vizinho.
func TestGuerraDeCanaisMultiplosProdutos(t *testing.T) {
	requireDB(t)
	ctx := context.Background()

	fx := seedScaleEvent(t)
	repo := NewRepository(sqlc.New(testPool), testPool)

	const nProdutos = 12
	const inicialPorProduto = 8

	type sku struct {
		id   string
		tiny *tinyReal
	}
	skus := make([]sku, 0, nProdutos)
	for i := 0; i < nProdutos; i++ {
		id := seedSoldOutProductWithQueue(t, fx, inicialPorProduto, 0)
		skus = append(skus, sku{id: id, tiny: &tinyReal{saldo: inicialPorProduto}})
	}

	var wg sync.WaitGroup
	largada := make(chan struct{})
	vendas := make([]int, nProdutos)
	var mu sync.Mutex

	for i := range skus {
		for j := 0; j < 10; j++ { // 10 compradores por SKU
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-largada
				if err := repo.DecrementProductStock(ctx, skus[i].id, 1); err != nil {
					return
				}
				skus[i].tiny.saidaDoLiveCart(1)
				mu.Lock()
				vendas[i]++
				mu.Unlock()
			}(i)
		}
		for j := 0; j < 4; j++ { // 4 vendas no e-commerce por SKU
			wg.Add(1)
			go func(i int) { defer wg.Done(); <-largada; skus[i].tiny.venderNoEcommerce(1) }(i)
		}
		for j := 0; j < 8; j++ { // webhooks
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-largada
				seq := seqDoProduto(t, skus[i].id)
				if _, err := repo.ApplyERPStockMirror(ctx, skus[i].id, skus[i].tiny.ler(), seq); err != nil {
					t.Errorf("webhook: %v", err)
				}
			}(i)
		}
	}
	close(largada)
	wg.Wait()

	sobreposicaoTotal := 0
	for i, s := range skus {
		// O de-para não cruzou: cada saída que o ERP viu naquele SKU corresponde
		// a uma venda registrada NAQUELE SKU. Se um movimento aterrissasse no
		// vizinho, estes dois números divergiriam — é este o teste, e não a
		// soma com o e-commerce, que depende de um canal que não controlamos.
		if s.tiny.saidasLC != vendas[i] {
			t.Errorf("SKU %d: o ERP recebeu %d saídas nossas mas registramos %d vendas — "+
				"movimento aterrissou no produto errado sob concorrência",
				i, s.tiny.saidasLC, vendas[i])
		}
		// O porteiro segurou: nunca mais que o estoque inicial, por SKU.
		if s.tiny.saidasLC > inicialPorProduto {
			t.Errorf("SKU %d: a live vendeu %d de %d — o porteiro falhou",
				i, s.tiny.saidasLC, inicialPorProduto)
		}

		if soma := s.tiny.saidasLC + s.tiny.vendasWeb; soma > inicialPorProduto {
			sobreposicaoTotal += soma - inicialPorProduto
		}

		// Convergência por SKU, com o piso deliberado em zero.
		seq := seqDoProduto(t, s.id)
		if _, err := repo.ApplyERPStockMirror(ctx, s.id, s.tiny.ler(), seq); err != nil {
			t.Fatalf("webhook final SKU %d: %v", i, err)
		}
		esperado := s.tiny.ler()
		if esperado < 0 {
			esperado = 0
		}
		if got := estoqueDoProduto(t, s.id); got != esperado {
			t.Errorf("SKU %d: LiveCart %d, ERP %d — têm de convergir", i, got, s.tiny.ler())
		}
	}
	if sobreposicaoTotal > 0 {
		t.Logf("SOBREPOSIÇÃO ENTRE CANAIS: %d unidade(s) somando os %d SKUs — inerente a "+
			"vender o mesmo item em dois lugares sem o ERP arbitrar",
			sobreposicaoTotal, nProdutos)
	}

	total := 0
	for _, v := range vendas {
		total += v
	}
	t.Logf("%d SKUs, %d vendas na live, %d por e-commerce", nProdutos, total, nProdutos*4)
	_ = fmt.Sprint()
}

// A pergunta que decide se a trava vale: durante uma live QUENTE, o LiveCart
// continua enxergando o e-commerce?
//
// A trava descarta a leitura do ERP sempre que um movimento nosso aterrissa
// depois dela. Numa live com compradores constantes, movimento nosso é o que
// não falta — e a suspeita legítima é que ela vire, na prática, a mesma cegueira
// dos mecanismos antigos (suprimir enquanto houvesse reserva viva cegava por
// trinta minutos; a janela de sessenta segundos, por um minuto a cada movimento).
//
// A diferença tem de ser esta: a trava não descarta por HAVER reserva, e sim por
// a leitura ser VELHA. Uma leitura tirada agora entra agora, mesmo com a live no
// auge. Este teste mede, e não argumenta: conta quantos webhooks entram e em
// quanto tempo uma venda no e-commerce chega ao nosso contador.
func TestLiveQuenteNaoCegaParaOEcommerce(t *testing.T) {
	requireDB(t)
	ctx := context.Background()

	fx := seedScaleEvent(t)
	repo := NewRepository(sqlc.New(testPool), testPool)

	const inicial = 400
	productID := seedSoldOutProductWithQueue(t, fx, inicial, 0)
	tiny := &tinyReal{saldo: inicial}

	var aplicados, descartados int64
	parar := make(chan struct{})
	var wg sync.WaitGroup

	// A live: 200 compradores em rajada contínua, cada um com um movimento nosso.
	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := repo.DecrementProductStock(ctx, productID, 1); err != nil {
				return
			}
			tiny.saidaDoLiveCart(1)
		}()
	}

	// O e-commerce vendendo em paralelo, e cada venda gerando o seu webhook —
	// que é como o Tiny se comporta: saldo absoluto, um evento por movimento.
	var vendasWebObservadas int64
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if !tiny.venderNoEcommerce(1) {
				return
			}
			atomic.AddInt64(&vendasWebObservadas, 1)
			// O webhook correspondente, insistindo até entrar — é o que o Tiny
			// faz de verdade: ele reenvia o que não confirmamos.
			for tentativa := 0; tentativa < 50; tentativa++ {
				seq := seqDoProduto(t, productID)
				ok, err := repo.ApplyERPStockMirror(ctx, productID, tiny.ler(), seq)
				if err != nil {
					t.Errorf("webhook: %v", err)
					return
				}
				if ok {
					atomic.AddInt64(&aplicados, 1)
					return
				}
				atomic.AddInt64(&descartados, 1)
			}
		}()
	}

	wg.Wait()
	close(parar)

	// Depois do barulho, uma última venda no e-commerce tem de chegar até nós.
	// Esta é a prova de que a cegueira não é permanente: sem live batendo, a
	// primeira leitura entra.
	antes := estoqueDoProduto(t, productID)
	tiny.venderNoEcommerce(1)
	seq := seqDoProduto(t, productID)
	ok, err := repo.ApplyERPStockMirror(ctx, productID, tiny.ler(), seq)
	if err != nil {
		t.Fatalf("webhook pós-live: %v", err)
	}
	if !ok {
		t.Fatal("com a live parada, a leitura do ERP AINDA foi descartada — a trava " +
			"virou supressão permanente, que é exatamente o defeito que ela substituiu")
	}
	depois := estoqueDoProduto(t, productID)
	if depois >= antes {
		t.Errorf("venda no e-commerce não baixou o nosso contador (%d → %d)", antes, depois)
	}

	total := aplicados + descartados
	t.Logf("live quente: %d movimentos nossos | %d vendas no e-commerce | "+
		"%d tentativas de webhook, %d entraram (%.0f%%), %d descartadas por leitura velha",
		tiny.saidasLC, vendasWebObservadas, total, aplicados,
		float64(aplicados)*100/float64(total), descartados)

	// Toda venda do e-commerce tem de ter chegado ao nosso contador. Se alguma
	// ficasse de fora, o LiveCart seguiria oferecendo unidade que o marketplace
	// já levou — que é o buraco que a trava existe para não abrir.
	if aplicados != vendasWebObservadas {
		t.Errorf("%d vendas no e-commerce, só %d chegaram até nós — as outras ficaram "+
			"invisíveis e o contador local segue inflado", vendasWebObservadas, aplicados)
	}
}

// A pergunta direta: durante a guerra, alguma escrita mexeu no estoque de um
// jeito que os nossos movimentos não justificam?
//
// Há duas direções, e elas não são igualmente perigosas. Contador a MENOS segura
// venda que existia: chato, o lojista vê, e o próximo saldo do ERP corrige.
// Contador a MAIS oferece unidade que não existe — vira promoção fantasma da
// fila, venda confirmada de produto esgotado e pedido sem como atender.
//
// Duas armadilhas derrubaram as versões anteriores deste teste, e as duas o
// faziam passar até com a trava REMOVIDA:
//
//  1. O ERP atualizado no mesmo instante do decremento local. Sem atraso não
//     existe foto velha, e é a foto velha que infla. Na produção baixamos o
//     contador local primeiro e só então mandamos o delta por HTTP — ordem
//     deliberada, para nunca vender o que já saiu — e durante essa ida-e-volta
//     o Tiny responde o saldo ANTIGO, maior que o nosso.
//
//  2. Estoque menor que a demanda. A conta final batia no piso zero, e o piso
//     engolia qualquer erro. Aqui o estoque é folgado de propósito: a conta
//     final é exata, e um contador inflado não tem onde se esconder.
//
// Medir por foto "antes/depois" de cada aplicação também não funciona: entre as
// duas leituras caem dezenas de decrementos concorrentes que mascaram a subida.
// Quem denuncia é a conta fechada no fim.
func TestEspelhoDoERPNuncaInventaUnidade(t *testing.T) {
	requireDB(t)
	ctx := context.Background()

	fx := seedScaleEvent(t)
	repo := NewRepository(sqlc.New(testPool), testPool)

	// Folgado: 200 para 70 de demanda. Nada é aparado no piso.
	const inicial = 200
	productID := seedSoldOutProductWithQueue(t, fx, inicial, 0)
	tiny := &tinyReal{saldo: inicial}

	var mu sync.Mutex
	var aplicacoes int
	var movimentosNossos int

	var wg sync.WaitGroup
	largada := make(chan struct{})

	// A live comprando, com a chamada ao ERP viajando depois do decremento.
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-largada
			if err := repo.DecrementProductStock(ctx, productID, 1); err != nil {
				return
			}
			mu.Lock()
			movimentosNossos++
			mu.Unlock()
			// Enquanto isto dorme, o Tiny responde o saldo de ANTES desta saída.
			time.Sleep(time.Duration(2+i%7) * time.Millisecond)
			tiny.saidaDoLiveCart(1)
		}(i)
	}

	// O e-commerce vendendo, cada venda com o seu webhook — lendo o saldo do
	// Tiny exatamente enquanto as nossas saídas estão em voo.
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-largada
			time.Sleep(time.Duration(i%9) * time.Millisecond)
			if !tiny.venderNoEcommerce(1) {
				return
			}
			for tentativa := 0; tentativa < 200; tentativa++ {
				seq := seqDoProduto(t, productID)
				ok, err := repo.ApplyERPStockMirror(ctx, productID, tiny.ler(), seq)
				if err != nil {
					t.Errorf("webhook: %v", err)
					return
				}
				if ok {
					mu.Lock()
					aplicacoes++
					mu.Unlock()
					return
				}
			}
		}(i)
	}
	close(largada)
	wg.Wait()

	// A conta fechada. Tudo que saiu, saiu por um motivo rastreável: nossas
	// vendas na live e as do lojista no e-commerce. Um contador MAIOR que isto é
	// unidade inventada por uma foto velha do ERP; um contador MENOR é unidade
	// sumida. Nenhum dos dois pode acontecer.
	esperado := inicial - movimentosNossos - tiny.vendasWeb
	got := estoqueDoProduto(t, productID)
	switch {
	case got > esperado:
		t.Errorf("contador local = %d, deveria ser %d: o espelho do ERP INVENTOU %d "+
			"unidade(s) a partir de uma leitura tirada enquanto a nossa saída estava em "+
			"voo. Cada uma dessas oferece peça que não existe — é o que dispara promoção "+
			"fantasma da fila e venda de produto esgotado",
			got, esperado, got-esperado)
	case got < esperado:
		t.Errorf("contador local = %d, deveria ser %d: sumiram %d unidade(s) que ninguém "+
			"comprou", got, esperado, esperado-got)
	}

	if tiny.ler() != esperado {
		t.Errorf("o ERP terminou em %d e o nosso contador em %d — os dois têm de "+
			"convergir quando o barulho para", tiny.ler(), got)
	}

	t.Logf("%d movimentos nossos, %d vendas no e-commerce, %d aplicações do saldo do ERP "+
		"| contador final %d (esperado %d), Tiny %d",
		movimentosNossos, tiny.vendasWeb, aplicacoes, got, esperado, tiny.ler())
}
