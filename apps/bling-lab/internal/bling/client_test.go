package bling

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/livecart/bling-lab/internal/audit"
	"github.com/livecart/bling-lab/internal/config"
	"github.com/livecart/bling-lab/internal/oauth"
)

// bancada sobe uma API falsa e um Client autenticado apontado para ela.
func bancada(t *testing.T, rps float64, h http.HandlerFunc) (*Client, *config.Config) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	dir := t.TempDir()
	cfg := &config.Config{
		ClientID: "id", ClientSecret: "secret",
		APIBase: srv.URL, TokenURL: srv.URL, RevokeURL: srv.URL,
		StateDir: dir, RateLimitRPS: rps,
	}
	// Token válido em disco: os comandos não devem tentar renovar no meio do teste.
	if err := oauth.SaveTokens(cfg.TokensPath(), &oauth.Tokens{
		AccessToken: "at", RefreshToken: "rt", TokenType: "Bearer",
		ObtainedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour),
		RefreshObtainedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	lg, err := audit.New(filepath.Join(dir, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	return New(cfg, oauth.NewClient(cfg, lg), lg), cfg
}

func jsonHandler(corpo string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(corpo))
	}
}

// O freio é preditivo porque a API não devolve header de cota nenhum. Se ele
// não segurar, uma varredura de catálogo estoura os 3 req/s da conta — e o
// lojista descobre pelo 429 no meio da live dele, não pelo nosso log.
func TestFreioEspacaAsRequisicoes(t *testing.T) {
	var chegadas []time.Time
	c, _ := bancada(t, 5, func(w http.ResponseWriter, _ *http.Request) {
		chegadas = append(chegadas, time.Now())
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[]}`))
	})

	inicio := time.Now()
	for i := 0; i < 4; i++ {
		if _, err := c.Get(context.Background(), "/depositos", nil); err != nil {
			t.Fatal(err)
		}
	}
	decorrido := time.Since(inicio)

	// 4 chamadas a 5/s = 3 intervalos de 200 ms = pelo menos 600 ms.
	if decorrido < 600*time.Millisecond {
		t.Errorf("4 chamadas a 5 req/s levaram %s — o freio não segurou", decorrido)
	}
	if len(chegadas) != 4 {
		t.Fatalf("a API falsa viu %d chamadas, queria 4", len(chegadas))
	}
	// O contador tem de bater com o que a API viu: divergência significa
	// caminho cego, e é assim que uma cota estoura sem ninguém entender.
	if c.Chamadas() != len(chegadas) {
		t.Errorf("contador diz %d, a API viu %d", c.Chamadas(), len(chegadas))
	}
}

// O 429 do Bling não traz Retry-After (medido). A mensagem tem de dizer o que
// fazer, senão vira "erro 429" no log e ninguém sabe que a cota é COMPARTILHADA
// com o e-commerce do próprio lojista.
func TestErro429ExplicaACotaCompartilhada(t *testing.T) {
	c, _ := bancada(t, 100, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("x-amzn-RequestId", "req-abc-123")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"type":"TOO_MANY_REQUESTS","description":"limite excedido"}}`))
	})

	_, err := c.Get(context.Background(), "/produtos", nil)
	if err == nil {
		t.Fatal("queria erro")
	}
	e, ok := err.(*ErroAPI)
	if !ok {
		t.Fatalf("queria *ErroAPI, veio %T", err)
	}
	if e.Status != http.StatusTooManyRequests {
		t.Errorf("status = %d", e.Status)
	}
	// O x-amzn-RequestId é o identificador que o suporte do Bling pede.
	if e.RequestID != "req-abc-123" {
		t.Errorf("perdemos o x-amzn-RequestId: %q", e.RequestID)
	}
	msg := e.Error()
	for _, esperado := range []string{"3 req/s", "POR CONTA", "Retry-After"} {
		if !strings.Contains(msg, esperado) {
			t.Errorf("a mensagem do 429 não menciona %q: %s", esperado, msg)
		}
	}
}

// Dois portões, e o primeiro tem de barrar ANTES de qualquer requisição sair:
// o Bling não tem sandbox, então uma escrita por engano acerta o ERP de um
// lojista de verdade.
func TestEscritaBloqueadaSemAsDuasChaves(t *testing.T) {
	t.Run("sem BLING_ALLOW_WRITE nem chega a sair", func(t *testing.T) {
		var saiu bool
		c, _ := bancada(t, 100, func(w http.ResponseWriter, _ *http.Request) {
			saiu = true
			w.WriteHeader(http.StatusCreated)
		})
		_, err := c.Write(context.Background(), http.MethodPost, "/pedidos/vendas", map[string]any{"x": 1})
		if err == nil {
			t.Fatal("queria bloqueio")
		}
		if saiu {
			t.Fatal("a requisição SAIU — o guard tem de barrar antes da rede")
		}
		if !strings.Contains(err.Error(), "BLOQUEADA") {
			t.Errorf("mensagem pouco clara: %v", err)
		}
	})

	t.Run("com a flag mas sem allowlist", func(t *testing.T) {
		c, cfg := bancada(t, 100, jsonHandler(`{"data":{}}`))
		cfg.AllowWrite = true
		_, err := c.Write(context.Background(), http.MethodPost, "/pedidos/vendas", nil)
		if err == nil || !strings.Contains(err.Error(), "allowlist") {
			t.Fatalf("queria bloqueio por allowlist vazia, veio: %v", err)
		}
	})

	t.Run("conta diferente da allowlist", func(t *testing.T) {
		c, cfg := bancada(t, 100, jsonHandler(`{"data":{"id":"conta-REAL","nome":"Loja do Fulano"}}`))
		cfg.AllowWrite = true
		cfg.AllowedCompanyID = []string{"conta-de-teste"}

		_, err := c.Write(context.Background(), http.MethodPost, "/pedidos/vendas", nil)
		if err == nil {
			t.Fatal("queria bloqueio")
		}
		if !strings.Contains(err.Error(), "conta-REAL") || !strings.Contains(err.Error(), "Loja do Fulano") {
			t.Errorf("a mensagem tem de dizer em QUAL conta ia escrever: %v", err)
		}
	})
}

// A ausência de um produto na resposta de /estoques/saldos é a armadilha do
// filtroSaldoEstoque: o default do Bling é 1 (só positivo), então um produto
// esgotado some. O cliente não pode inventar zero nem repetir o saldo velho —
// devolve só o que veio, e quem chama decide.
func TestSaldosDevolveApenasOQueVeio(t *testing.T) {
	c, _ := bancada(t, 100, jsonHandler(`{"data":[{"produto":{"id":1,"codigo":"A"},"saldoFisicoTotal":5,"saldoVirtualTotal":3}]}`))

	saldos, err := c.Saldos(context.Background(), []int64{1, 2, 3}, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(saldos) != 1 {
		t.Fatalf("devolveu %d linhas, queria 1 — não pode inventar as ausentes", len(saldos))
	}
	if saldos[0].Produto.ID != 1 || saldos[0].SaldoVirtualTotal != 3 {
		t.Errorf("linha errada: %+v", saldos[0])
	}
}

func TestSaldosExigeIDs(t *testing.T) {
	c, _ := bancada(t, 100, jsonHandler(`{"data":[]}`))
	if _, err := c.Saldos(context.Background(), nil, 1); err == nil {
		t.Error("idsProdutos[] é obrigatório no Bling — devia falhar antes de sair")
	}
}

// Todos os headers da resposta vão para o log. É assim que se PROVA a ausência
// de header de cota contra a conta real — uma ausência só é dado se a coleta
// for completa.
func TestAuditoriaGravaHeadersDaResposta(t *testing.T) {
	c, cfg := bancada(t, 100, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("x-amzn-RequestId", "abc")
		w.Header().Set("cf-ray", "xyz")
		w.Header().Set("Set-Cookie", "PHPSESSID=segredo")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[]}`))
	})

	if _, err := c.Get(context.Background(), "/depositos", nil); err != nil {
		t.Fatal(err)
	}

	linhas := lerAuditoria(t, cfg.AuditPath())
	if len(linhas) == 0 {
		t.Fatal("nada auditado")
	}
	h := linhas[len(linhas)-1].Headers
	if h["x-amzn-requestid"] != "abc" || h["cf-ray"] != "xyz" {
		t.Errorf("headers não gravados: %v", h)
	}
	if h["set-cookie"] != "<omitido>" {
		t.Errorf("o Set-Cookie vazou para o log: %q", h["set-cookie"])
	}
}

func lerAuditoria(t *testing.T, caminho string) []audit.Entry {
	t.Helper()
	b, err := os.ReadFile(caminho)
	if err != nil {
		t.Fatal(err)
	}
	var out []audit.Entry
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		var e audit.Entry
		if json.Unmarshal([]byte(l), &e) == nil {
			out = append(out, e)
		}
	}
	return out
}
