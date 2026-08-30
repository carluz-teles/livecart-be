package oauth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/livecart/bling-lab/internal/audit"
	"github.com/livecart/bling-lab/internal/config"
)

// bancada sobe um token endpoint falso e devolve o Client apontado para ele,
// junto com um ponteiro para a última requisição recebida.
type recebida struct {
	authorization string
	contentType   string
	enableJWT     string
	form          url.Values
}

func bancada(t *testing.T, resposta func(w http.ResponseWriter, r *http.Request)) (*Client, *recebida) {
	t.Helper()
	ultima := &recebida{}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		ultima.authorization = r.Header.Get("Authorization")
		ultima.contentType = r.Header.Get("Content-Type")
		ultima.enableJWT = r.Header.Get("enable-jwt")
		ultima.form = r.PostForm
		resposta(w, r)
	}))
	t.Cleanup(srv.Close)

	cfg := &config.Config{
		ClientID:     "meu-client-id",
		ClientSecret: "meu-client-secret",
		TokenURL:     srv.URL,
		RevokeURL:    srv.URL,
		StateDir:     t.TempDir(),
	}
	lg, err := audit.New(filepath.Join(cfg.StateDir, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	return NewClient(cfg, lg), ultima
}

func respostaOK(access, refresh string, expiraEm int) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  access,
			"refresh_token": refresh,
			"token_type":    "Bearer",
			"expires_in":    expiraEm,
			"scope":         "produtos estoques",
		})
	}
}

// A regra que custa uma tarde quando não se sabe: a doc do Bling diz que as
// credenciais têm de ir no header Basic e que "não é permitida a inserção
// destes parâmetros no body". Mandar no body devolve invalid_client MESMO com
// credencial correta — e o erro não diz que o problema é o lugar.
func TestCredenciaisVaoNoBasicENuncaNoBody(t *testing.T) {
	c, ultima := bancada(t, respostaOK("at", "rt", 3600))

	if _, err := c.exchange(context.Background(), url.Values{
		"grant_type": {"authorization_code"},
		"code":       {"o-code"},
	}); err != nil {
		t.Fatal(err)
	}

	esperado := "Basic " + base64.StdEncoding.EncodeToString([]byte("meu-client-id:meu-client-secret"))
	if ultima.authorization != esperado {
		t.Errorf("header Authorization = %q, queria %q", ultima.authorization, esperado)
	}
	if _, tem := ultima.form["client_id"]; tem {
		t.Error("client_id foi para o BODY — o Bling recusa com invalid_client")
	}
	if _, tem := ultima.form["client_secret"]; tem {
		t.Error("client_secret foi para o BODY — o Bling recusa com invalid_client")
	}
	if ultima.form.Get("code") != "o-code" {
		t.Errorf("o code não chegou no body: %v", ultima.form)
	}
	if !strings.HasPrefix(ultima.contentType, "application/x-www-form-urlencoded") {
		t.Errorf("Content-Type = %q, queria form-urlencoded", ultima.contentType)
	}
	if ultima.enableJWT != "1" {
		t.Errorf("header enable-jwt = %q, queria \"1\" (o token opaco foi descontinuado)", ultima.enableJWT)
	}
}

// O envelope de erro do Bling é aninhado, medido em 29/08/2026 contra o token
// endpoint real. Se o parse quebrar, o operador vê "HTTP 400" e nada mais.
func TestErroDoBlingViraMensagemUtil(t *testing.T) {
	c, _ := bancada(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"type":"invalid_client","message":"invalid_client",` +
			`"description":"Client credentials were not found in the headers"}}`))
	})

	_, err := c.exchange(context.Background(), url.Values{"grant_type": {"authorization_code"}})
	if err == nil {
		t.Fatal("queria erro")
	}
	if !strings.Contains(err.Error(), "invalid_client") {
		t.Errorf("mensagem sem o tipo do erro: %v", err)
	}
	if !strings.Contains(err.Error(), "Client credentials were not found") {
		t.Errorf("mensagem sem a descrição do erro: %v", err)
	}
	if !strings.Contains(err.Error(), "header Basic") {
		t.Errorf("faltou a dica que resolve o problema: %v", err)
	}
}

// A medição que decide se uma loja parada 30 dias perde a conexão. O Bling não
// documenta se o refresh token rotaciona; o laboratório descobre observando.
func TestRefreshRegistraSeRotacionou(t *testing.T) {
	t.Run("rotaciona", func(t *testing.T) {
		c, _ := bancada(t, respostaOK("at2", "rt-NOVO", 3600))
		antigo := &Tokens{RefreshToken: "rt-VELHO", RefreshObtainedAt: time.Now().Add(-20 * 24 * time.Hour)}

		novo, err := c.Refresh(context.Background(), antigo)
		if err != nil {
			t.Fatal(err)
		}
		if novo.Rotacionou == nil || !*novo.Rotacionou {
			t.Fatal("queria Rotacionou=true")
		}
		// O relógio dos 30 dias tem de REINICIAR.
		if !novo.RefreshObtainedAt.After(antigo.RefreshObtainedAt) {
			t.Error("rotacionou mas o relógio do refresh não reiniciou")
		}
	})

	t.Run("não rotaciona", func(t *testing.T) {
		c, _ := bancada(t, respostaOK("at2", "rt-VELHO", 3600))
		emitido := time.Now().Add(-20 * 24 * time.Hour)
		antigo := &Tokens{RefreshToken: "rt-VELHO", RefreshObtainedAt: emitido}

		novo, err := c.Refresh(context.Background(), antigo)
		if err != nil {
			t.Fatal(err)
		}
		if novo.Rotacionou == nil || *novo.Rotacionou {
			t.Fatal("queria Rotacionou=false")
		}
		// O relógio NÃO pode reiniciar: é o caso em que a loja parada perde a
		// conexão, e mentir aqui esconde exatamente isso.
		if !novo.RefreshObtainedAt.Equal(emitido) {
			t.Errorf("não rotacionou mas o relógio andou: %v → %v", emitido, novo.RefreshObtainedAt)
		}
	})

	t.Run("resposta sem refresh_token preserva o antigo", func(t *testing.T) {
		c, _ := bancada(t, respostaOK("at2", "", 3600))
		antigo := &Tokens{RefreshToken: "rt-VELHO", RefreshObtainedAt: time.Now().Add(-time.Hour)}

		novo, err := c.Refresh(context.Background(), antigo)
		if err != nil {
			t.Fatal(err)
		}
		if novo.RefreshToken != "rt-VELHO" {
			t.Errorf("perdemos o refresh token: %q", novo.RefreshToken)
		}
	})
}

// Revoke NÃO pode expor revoke_action/revoke_target: eles revogam TODOS os
// tokens de um usuário ou empresa, o que num app compartilhado derrubaria
// lojas que não são esta. A proibição é de código, não de disciplina.
func TestRevokeNaoMandaRevogacaoAvancada(t *testing.T) {
	c, ultima := bancada(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })

	if err := c.Revoke(context.Background(), "rt", "refresh_token"); err != nil {
		t.Fatal(err)
	}
	for _, proibido := range []string{"revoke_action", "revoke_target"} {
		if _, tem := ultima.form[proibido]; tem {
			t.Errorf("Revoke mandou %q — isso derruba outras lojas do mesmo aplicativo", proibido)
		}
	}
	if ultima.form.Get("token") != "rt" {
		t.Errorf("token errado no body: %v", ultima.form)
	}
	if !strings.HasPrefix(ultima.authorization, "Basic ") {
		t.Error("revoke também exige Basic")
	}
}

func TestAuthorizeURLNaoMandaRedirectNemScope(t *testing.T) {
	c, _ := bancada(t, respostaOK("at", "rt", 3600))
	c.cfg.AuthURL = "https://bling.com.br/Api/v3/oauth/authorize"

	u := c.AuthorizeURL("meu-state")
	// O Bling IGNORA redirect_uri e scope da requisição e usa os do cadastro.
	// Mandá-los sugere um controle que não temos e confunde quem depura.
	for _, ausente := range []string{"redirect_uri", "scope"} {
		if strings.Contains(u, ausente) {
			t.Errorf("AuthorizeURL mandou %q, que o Bling ignora: %s", ausente, u)
		}
	}
	for _, presente := range []string{"response_type=code", "client_id=meu-client-id", "state=meu-state"} {
		if !strings.Contains(u, presente) {
			t.Errorf("AuthorizeURL sem %q: %s", presente, u)
		}
	}
}
