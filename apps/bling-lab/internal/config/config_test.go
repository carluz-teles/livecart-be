package config

import "testing"

// O redirect cadastrado no aplicativo do Bling decide ONDE o servidor de
// callback escuta. Quando é localhost, escuta na porta do próprio redirect;
// quando é um túnel público, escuta na CallbackPort e o túnel faz a ponte.
// Errar isso dá um login que abre o navegador e nunca volta.
func TestOndeOCallbackEscuta(t *testing.T) {
	casos := []struct {
		nome         string
		redirect     string
		callbackPort int
		querLocal    bool
		querPorta    int
		querCaminho  string
	}{
		{"localhost com porta", "http://localhost:8790/callback", 9999, true, 8790, "/callback"},
		{"127.0.0.1 com porta", "http://127.0.0.1:1234/oauth/bling", 9999, true, 1234, "/oauth/bling"},
		{"túnel cloudflare", "https://abc-def.trycloudflare.com/callback", 8790, false, 8790, "/callback"},
		{"túnel loca.lt", "https://livecart-bling.loca.lt/oauth/bling/callback", 8790, false, 8790, "/oauth/bling/callback"},
		{"raiz sem caminho", "http://localhost:8790", 9999, true, 8790, "/"},
	}

	for _, c := range casos {
		t.Run(c.nome, func(t *testing.T) {
			cfg := &Config{RedirectURI: c.redirect, CallbackPort: c.callbackPort}
			if got := cfg.RedirectIsLocal(); got != c.querLocal {
				t.Errorf("RedirectIsLocal() = %v, queria %v", got, c.querLocal)
			}
			if got := cfg.ListenPort(); got != c.querPorta {
				t.Errorf("ListenPort() = %d, queria %d", got, c.querPorta)
			}
			if got := cfg.CallbackPath(); got != c.querCaminho {
				t.Errorf("CallbackPath() = %q, queria %q", got, c.querCaminho)
			}
		})
	}
}

// Um redirect inválido não pode virar pânico nem "é local" por engano — um
// falso positivo aqui faz o lab tentar bindar num host que não é dele.
func TestRedirectInvalidoNaoEhLocal(t *testing.T) {
	cfg := &Config{RedirectURI: "://isto-nao-e-url", CallbackPort: 8790}
	if cfg.RedirectIsLocal() {
		t.Error("redirect inválido não pode ser considerado local")
	}
	if cfg.ListenPort() != 8790 {
		t.Errorf("ListenPort() = %d, queria o fallback 8790", cfg.ListenPort())
	}
}

func TestCredenciaisSaoObrigatorias(t *testing.T) {
	if err := (&Config{}).RequireCredentials(); err == nil {
		t.Error("sem credencial devia falhar — nenhum segredo tem valor padrão")
	}
	if err := (&Config{ClientID: "a"}).RequireCredentials(); err == nil {
		t.Error("só o client_id não basta")
	}
	if err := (&Config{ClientID: "a", ClientSecret: "b"}).RequireCredentials(); err != nil {
		t.Errorf("com as duas devia passar: %v", err)
	}
}
