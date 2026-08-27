package providers

// Retry de ESCRITA: em quê se pode repetir, e em quê não.
//
// A distinção é a mesma que o resto do sistema já faz para lançamento de
// estoque, e vale por toda a integração:
//
//	429  o servidor recusou ANTES de processar. Provado não-aplicado.
//	     Repetir é seguro, e é a única cura — a janela vai rolar.
//	5xx  o servidor respondeu. Pode ter aplicado e quebrado depois.
//	     Repetir um POST /pedidos aqui criaria um segundo pedido.
//
// `DoRequestWithRetry` repete os dois, e por isso serve a LEITURA. A escrita usa
// `DoRequestRetrying429`, que repete só o primeiro.
//
// Motivação medida (25/08/2026): a API v3 do Tiny impõe 4 req/s em rajada e
// 30 req/min sustentado, e a finalização de um pedido gasta de 6 a 10 chamadas.
// Nenhuma escrita repetia em 429, e dois dos três pedidos travados da Canto da
// Art morreram exatamente assim — o #1087 com
// "create order failed: status 429" após 3 tentativas do fluxo inteiro.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"
)

func novaBase() *BaseProvider {
	return NewBaseProvider(BaseProviderConfig{
		IntegrationID: "int-teste",
		StoreID:       "loja-teste",
		Logger:        zap.NewNop(),
		Timeout:       10 * time.Second,
	})
}

// A propriedade que justifica a função existir.
func TestEscritaRepeteEm429(t *testing.T) {
	var chamadas int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if atomic.AddInt32(&chamadas, 1) == 1 {
			w.Header().Set("X-Ratelimit-Limit", "4")
			w.Header().Set("X-Ratelimit-Reset", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":847982356}`))
	}))
	defer srv.Close()

	resp, _, err := novaBase().DoRequestRetrying429(context.Background(), 2,
		http.MethodPost, srv.URL, map[string]any{"x": 1}, nil)
	if err != nil {
		t.Fatalf("DoRequestRetrying429: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status final = %d, esperava 200 após a janela rolar", resp.StatusCode)
	}
	if n := atomic.LoadInt32(&chamadas); n != 2 {
		t.Errorf("POST enviado %d vezes; esperava 2 (o 429 e o sucesso)", n)
	}
}

// A propriedade que impede duplicar pedido — a mais importante das duas.
func TestEscritaNaoRepeteEm5xx(t *testing.T) {
	for _, status := range []int{500, 502, 503, 504} {
		var chamadas int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			atomic.AddInt32(&chamadas, 1)
			w.WriteHeader(status)
		}))

		resp, _, err := novaBase().DoRequestRetrying429(context.Background(), 2,
			http.MethodPost, srv.URL, map[string]any{"x": 1}, nil)
		srv.Close()

		if err != nil {
			t.Fatalf("status %d: erro inesperado: %v", status, err)
		}
		if resp.StatusCode != status {
			t.Errorf("status %d virou %d", status, resp.StatusCode)
		}
		if n := atomic.LoadInt32(&chamadas); n != 1 {
			t.Errorf("status %d: POST enviado %d vezes — o servidor respondeu e pode "+
				"ter aplicado; repetir criaria um segundo registro", status, n)
		}
	}
}

// 4xx que não é 429 é recusa definitiva: repetir só gasta a janela.
func TestEscritaNaoRepeteEmOutros4xx(t *testing.T) {
	var chamadas int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&chamadas, 1)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"mensagem":"Ocorreram erros de validação"}`))
	}))
	defer srv.Close()

	_, _, err := novaBase().DoRequestRetrying429(context.Background(), 2,
		http.MethodPost, srv.URL, map[string]any{"x": 1}, nil)
	if err != nil {
		t.Fatalf("erro inesperado: %v", err)
	}
	if n := atomic.LoadInt32(&chamadas); n != 1 {
		t.Errorf("POST enviado %d vezes num 400", n)
	}
}

// Esgotadas as tentativas, o 429 volta como RESPOSTA, não como erro: quem chamou
// é que sabe se aquilo é falha do fluxo ou trabalho para reagendar.
func TestEscritaDevolveO429QuandoEsgota(t *testing.T) {
	var chamadas int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&chamadas, 1)
		w.Header().Set("X-Ratelimit-Reset", "1")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	resp, _, err := novaBase().DoRequestRetrying429(context.Background(), 2,
		http.MethodPost, srv.URL, nil, nil)
	if err != nil {
		t.Fatalf("o 429 final não pode virar erro de transporte: %v", err)
	}
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Errorf("status = %d, esperava 429 devolvido ao chamador", resp.StatusCode)
	}
	if n := atomic.LoadInt32(&chamadas); n != 3 {
		t.Errorf("POST enviado %d vezes; esperava 3 (original + 2 retentativas)", n)
	}
}

// Dormir além do prazo deixaria a escrita sem desfecho registrado. Melhor
// devolver o 429 na hora para quem sabe reagendar.
func TestEscritaNaoDormeAlemDoPrazo(t *testing.T) {
	var chamadas int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&chamadas, 1)
		w.Header().Set("X-Ratelimit-Reset", "58") // janela sustentada
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	comecou := time.Now()
	resp, _, err := novaBase().DoRequestRetrying429(ctx, 2, http.MethodPost, srv.URL, nil, nil)
	levou := time.Since(comecou)

	if err != nil {
		t.Fatalf("erro inesperado: %v", err)
	}
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Errorf("status = %d, esperava o 429 de volta", resp.StatusCode)
	}
	if levou > time.Second {
		t.Errorf("levou %s — não podia ter dormido: o reset de 58s não cabe num prazo de 2s", levou)
	}
	if n := atomic.LoadInt32(&chamadas); n != 1 {
		t.Errorf("POST enviado %d vezes; devia ter desistido na primeira", n)
	}
}

func TestResetDoRateLimit(t *testing.T) {
	casos := []struct {
		nome     string
		headers  map[string]string
		esperado time.Duration
	}{
		{"X-Ratelimit-Reset do Tiny", map[string]string{"X-Ratelimit-Reset": "1"}, time.Second},
		{"janela sustentada", map[string]string{"X-Ratelimit-Reset": "58"}, 58 * time.Second},
		{"Retry-After como fallback", map[string]string{"Retry-After": "7"}, 7 * time.Second},
		{"sem header nenhum", nil, 2 * time.Second},
		{"valor absurdo é ignorado", map[string]string{"X-Ratelimit-Reset": "99999"}, 2 * time.Second},
		{"valor não numérico é ignorado", map[string]string{"X-Ratelimit-Reset": "logo"}, 2 * time.Second},
		{"zero é ignorado", map[string]string{"X-Ratelimit-Reset": "0"}, 2 * time.Second},
	}
	for _, c := range casos {
		t.Run(c.nome, func(t *testing.T) {
			h := http.Header{}
			for k, v := range c.headers {
				h.Set(k, v)
			}
			if got := resetDoRateLimit(h, 2*time.Second); got != c.esperado {
				t.Errorf("resetDoRateLimit = %s, esperava %s", got, c.esperado)
			}
		})
	}
}
