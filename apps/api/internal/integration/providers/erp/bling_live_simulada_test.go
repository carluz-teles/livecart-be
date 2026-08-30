package erp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"

	"livecart/apps/api/internal/integration/providers"
	"livecart/apps/api/lib/ratelimit"
)

// Ensaio de live simulada contra um Bling FALSO — o gate que autoriza produção.
//
// É o ensaio que faltou no Tiny e que teria custado zero. Ele reproduz, contra
// um servidor que imita os limites MEDIDOS da API real, a única coisa que o
// laboratório manual não consegue: CARGA.
//
// O Bling falso aqui não é generoso. Ele aplica o teto de 3 req/s e devolve 429
// de verdade quando alguém passa — que é exatamente o que a conta do lojista faz.

// blingFalsoSobCarga imita a API com o teto real.
type blingFalsoSobCarga struct {
	mu sync.Mutex

	// chegadas guarda o instante de TODA requisição, para a asserção de janela
	// deslizante. É o coração do ensaio: um contador total esconderia a rajada.
	chegadas []time.Time
	// excedentes conta as que estouraram o teto e levaram 429.
	excedentes int
	// tetoRPS é o limite instantâneo da conta.
	tetoRPS int

	pedidos map[string]int64 // numeroLoja → id, para o claim por âncora
	proxID  int64
}

func novoBlingFalsoSobCarga(tetoRPS int) *blingFalsoSobCarga {
	return &blingFalsoSobCarga{tetoRPS: tetoRPS, pedidos: map[string]int64{}, proxID: 1000}
}

// registrar marca a chegada e diz se a requisição estourou o teto.
func (b *blingFalsoSobCarga) registrar(agora time.Time) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.chegadas = append(b.chegadas, agora)

	// Quantas chegaram no ÚLTIMO SEGUNDO, contando esta.
	corte := agora.Add(-time.Second)
	n := 0
	for i := len(b.chegadas) - 1; i >= 0; i-- {
		if b.chegadas[i].Before(corte) {
			break
		}
		n++
	}
	if n > b.tetoRPS {
		b.excedentes++
		return false
	}
	return true
}

// picoPorSegundo devolve o maior número de requisições numa janela DESLIZANTE
// de 1 s. É a métrica que importa: a média esconde a rajada, e é a rajada que
// leva 429.
func (b *blingFalsoSobCarga) picoPorSegundo() int {
	b.mu.Lock()
	defer b.mu.Unlock()

	pico := 0
	for i := range b.chegadas {
		fim := b.chegadas[i].Add(time.Second)
		n := 0
		for j := i; j < len(b.chegadas) && b.chegadas[j].Before(fim); j++ {
			n++
		}
		if n > pico {
			pico = n
		}
	}
	return pico
}

func (b *blingFalsoSobCarga) total() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.chegadas)
}

func (b *blingFalsoSobCarga) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !b.registrar(time.Now()) {
			// 429 SEM Retry-After, como a API real (medido: nenhum header de cota).
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":{"type":"TOO_MANY_REQUESTS"}}`))
			return
		}

		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/formas-pagamentos"):
			_, _ = w.Write([]byte(`{"data":[{"id":11010300,"descricao":"Boleto","situacao":1,"padrao":1}]}`))

		case strings.Contains(r.URL.Path, "/pedidos/vendas") && r.Method == http.MethodGet:
			// Claim por âncora: devolve o pedido quando o marcador já existe.
			marcador := r.URL.Query().Get("numerosLojas[]")
			b.mu.Lock()
			id, existe := b.pedidos[marcador]
			b.mu.Unlock()
			if !existe {
				_, _ = w.Write([]byte(`{"data":[]}`))
				return
			}
			_, _ = fmt.Fprintf(w, `{"data":[{"id":%d,"numeroLoja":%q}]}`, id, marcador)

		case strings.HasSuffix(r.URL.Path, "/pedidos/vendas") && r.Method == http.MethodPost:
			var p struct {
				NumeroLoja string `json:"numeroLoja"`
			}
			_ = json.NewDecoder(r.Body).Decode(&p)
			b.mu.Lock()
			b.proxID++
			id := b.proxID
			b.pedidos[p.NumeroLoja] = id
			b.mu.Unlock()
			w.WriteHeader(http.StatusCreated)
			_, _ = fmt.Fprintf(w, `{"data":{"id":%d,"alertas":[]}}`, id)

		case strings.Contains(r.URL.Path, "/estoques/saldos"):
			ids := r.URL.Query()["idsProdutos[]"]
			var linhas []string
			for _, id := range ids {
				linhas = append(linhas, fmt.Sprintf(
					`{"produto":{"id":%s},"saldoFisicoTotal":20,"saldoVirtualTotal":18}`, id))
			}
			_, _ = fmt.Fprintf(w, `{"data":[%s]}`, strings.Join(linhas, ","))

		default:
			_, _ = w.Write([]byte(`{"data":{}}`))
		}
	}
}

func blingSobCarga(t *testing.T, teto int, rps float64) (*Bling, *blingFalsoSobCarga) {
	t.Helper()
	falso := novoBlingFalsoSobCarga(teto)
	srv := httptest.NewServer(falso.handler())
	t.Cleanup(srv.Close)

	antes := blingAPIBaseURL
	blingAPIBaseURL = srv.URL
	t.Cleanup(func() { blingAPIBaseURL = antes })

	b, err := NewBling(BlingConfig{
		IntegrationID: "int-1", StoreID: "loja-1",
		ClientID: "cid", ClientSecret: "csec",
		Credentials: &providers.Credentials{AccessToken: "at", ExpiresAt: time.Now().Add(time.Hour)},
		Logger:      zap.NewNop(),
		// O MESMO limitador que a produção usa, com a MESMA taxa. Um ensaio com
		// limitador diferente do de produção não prova nada sobre produção.
		RateLimiter: ratelimit.NovoFixo(rps),
	})
	if err != nil {
		t.Fatal(err)
	}
	return b, falso
}

// ENSAIO PRINCIPAL: 15 compradoras chegando ao mesmo tempo, como numa live.
//
// A asserção que importa não é "funcionou" — é "não estourou o teto da conta".
// Uma live que cria os pedidos e leva 429 no meio deixa carrinho pago sem
// pedido, e é exatamente o que os 115 `unconfirmed` do Tiny foram.
func TestEnsaioDeLive15CompradorasNaoEstouraOTetoDaConta(t *testing.T) {
	const compradoras = 15
	const tetoDaConta = 3

	b, falso := blingSobCarga(t, tetoDaConta, 2) // 2 req/s, o padrão de produção

	var wg sync.WaitGroup
	erros := make(chan error, compradoras)
	inicio := time.Now()

	for i := 0; i < compradoras; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			// Prazo generoso: o objetivo é medir a TAXA, não exercitar o
			// desfecho de prazo curto (que tem teste próprio no Fixo).
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()

			_, err := b.CreateOrder(ctx, providers.ERPOrder{
				ExternalID: "cart-" + strconv.Itoa(n),
				ContactID:  "18357840921",
				Items: []providers.ERPOrderItem{{
					ProductID: "16698952209", Quantity: 1, UnitPrice: 390000, Name: "Produto",
				}},
			})
			if err != nil {
				erros <- fmt.Errorf("compradora %d: %w", n, err)
			}
		}(i)
	}
	wg.Wait()
	close(erros)
	decorrido := time.Since(inicio)

	var falhas []string
	for err := range erros {
		falhas = append(falhas, err.Error())
	}
	if len(falhas) > 0 {
		t.Errorf("%d de %d compradoras falharam:\n  %s",
			len(falhas), compradoras, strings.Join(falhas, "\n  "))
	}

	// A ASSERÇÃO DO ENSAIO: nenhuma janela de 1 s passou do teto da conta.
	if pico := falso.picoPorSegundo(); pico > tetoDaConta {
		t.Errorf("PICO de %d req/s numa janela deslizante — o teto da conta é %d. "+
			"Numa conta real isso vira 429 no meio da venda", pico, tetoDaConta)
	}
	if falso.excedentes > 0 {
		t.Errorf("%d requisição(ões) levaram 429 do Bling falso — o freio não segurou",
			falso.excedentes)
	}

	// ORÇAMENTO, e este número é o achado do ensaio.
	//
	// Cada compradora custa claim por âncora + POST = 2. A forma de pagamento é
	// UMA para todas — mas na primeira versão as 15 goroutines erravam o cache
	// juntas e disparavam 15 leituras idênticas: 45 requisições em vez de 31,
	// um TERÇO da cota da live gasto para descobrir a mesma coisa quinze vezes.
	// O single-flight em formaPagamentoPadrao consertou.
	//
	// O teto de 2,2 por compradora é o que impede a regressão: com o
	// thundering herd de volta, a média sobe para 3,0 e este teste falha.
	total := falso.total()
	if media := float64(total) / compradoras; media > 2.2 {
		t.Errorf("%d requisições para %d compradoras (%.1f cada) — acima do orçamento "+
			"de 2,2. A leitura da forma de pagamento voltou a ser uma por compradora?",
			total, compradoras, media)
	}

	t.Logf("ensaio: %d compradoras, %d requisições (%.1f/compradora), pico %d req/s, %s",
		compradoras, total, float64(total)/compradoras, falso.picoPorSegundo(), decorrido.Round(time.Millisecond))
}

// O claim por âncora tem de impedir pedido DUPLICADO quando a mesma compradora
// entra duas vezes — o retry de uma tentativa que morreu depois do POST.
//
// No Bling não existe 409 no POST (verificado: as respostas declaradas são 201
// e 400), então o claim é a ÚNICA defesa. Sem ele, a peça é vendida duas vezes.
func TestClaimPorAncoraImpedePedidoDuplicado(t *testing.T) {
	b, falso := blingSobCarga(t, 3, 5)

	pedido := providers.ERPOrder{
		ExternalID: "cart-repetido",
		ContactID:  "18357840921",
		Items: []providers.ERPOrderItem{{
			ProductID: "1", Quantity: 1, UnitPrice: 1000, Name: "x",
		}},
	}

	primeiro, err := b.CreateOrder(context.Background(), pedido)
	if err != nil {
		t.Fatal(err)
	}
	segundo, err := b.CreateOrder(context.Background(), pedido)
	if err != nil {
		t.Fatal(err)
	}

	if primeiro.OrderID != segundo.OrderID {
		t.Errorf("a segunda tentativa criou OUTRO pedido (%s vs %s) — a peça seria "+
			"vendida duas vezes", primeiro.OrderID, segundo.OrderID)
	}
	if segundo.Status != "adopted" {
		t.Errorf("a segunda tentativa devia ADOTAR o pedido existente, veio %q", segundo.Status)
	}
	falso.mu.Lock()
	criados := len(falso.pedidos)
	falso.mu.Unlock()
	if criados != 1 {
		t.Errorf("o Bling falso guardou %d pedidos, queria 1", criados)
	}
}

// Duas lojas na MESMA conta Bling dividem UM teto. Se cada uma tiver o próprio
// balde, o pico é o DOBRO do teto — e o 429 aparece sem ninguém entender por quê.
func TestDuasLojasNaMesmaContaCompartilhamOTeto(t *testing.T) {
	const tetoDaConta = 3
	falso := novoBlingFalsoSobCarga(tetoDaConta)
	srv := httptest.NewServer(falso.handler())
	defer srv.Close()

	antes := blingAPIBaseURL
	blingAPIBaseURL = srv.URL
	defer func() { blingAPIBaseURL = antes }()

	// O MESMO limitador para as duas lojas — é o que a chave por conta produz.
	compartilhado := ratelimit.NovoFixo(2)
	novaLoja := func(id string) *Bling {
		b, err := NewBling(BlingConfig{
			IntegrationID: id, StoreID: id,
			ClientID: "cid", ClientSecret: "csec",
			Credentials: &providers.Credentials{AccessToken: "at", ExpiresAt: time.Now().Add(time.Hour)},
			Logger:      zap.NewNop(),
			RateLimiter: compartilhado,
		})
		if err != nil {
			t.Fatal(err)
		}
		return b
	}

	lojaA, lojaB := novaLoja("A"), novaLoja("B")

	var wg sync.WaitGroup
	for _, loja := range []*Bling{lojaA, lojaB} {
		for i := 0; i < 6; i++ {
			wg.Add(1)
			go func(b *Bling) {
				defer wg.Done()
				_, _ = b.GetProductStockBatch(context.Background(), []string{"1"})
			}(loja)
		}
	}
	wg.Wait()

	if pico := falso.picoPorSegundo(); pico > tetoDaConta {
		t.Errorf("pico de %d req/s com DUAS lojas na mesma conta (teto %d) — "+
			"o balde compartilhado não segurou", pico, tetoDaConta)
	}
	if falso.excedentes > 0 {
		t.Errorf("%d requisições levaram 429", falso.excedentes)
	}
}
