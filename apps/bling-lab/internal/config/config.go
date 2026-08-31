// Package config carrega a configuração do bling-lab a partir do ambiente e de
// um arquivo .env local. Nenhum segredo tem valor padrão: se não vier do
// ambiente, o comando falha em vez de silenciosamente usar outra coisa.
package config

import (
	"bufio"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Endpoints da API v3 do Bling.
//
// A separação de hosts é a pegadinha que custa uma tarde: o SPEC declara o
// token endpoint em bling.com.br, e a DOC mostra api.bling.com.br. Medido em
// 29/08/2026: os dois respondem idêntico ao POST (400 invalid_client sem o
// header Basic), e um GET no mesmo caminho devolve 404 — daí a confusão. Fica
// bling.com.br porque é o que o próprio openapi.json do Bling declara em
// components.securitySchemes.OAuth2.
const (
	DefaultAuthURL   = "https://bling.com.br/Api/v3/oauth/authorize"
	DefaultTokenURL  = "https://bling.com.br/Api/v3/oauth/token"
	DefaultRevokeURL = "https://bling.com.br/Api/v3/oauth/revoke"
	DefaultAPIBase   = "https://api.bling.com.br/Api/v3"
)

type Config struct {
	ClientID     string
	ClientSecret string

	// RedirectURI é o que está CADASTRADO no aplicativo do Bling. O Bling
	// ignora o redirect_uri enviado na requisição e usa sempre o do cadastro,
	// então divergir aqui não dá erro na hora — dá um callback que nunca chega.
	RedirectURI string

	// CallbackPort é a porta LOCAL onde o servidor efêmero do login escuta.
	// Fica separada do RedirectURI porque o redirect cadastrado pode ser um
	// túnel público (https://algo.trycloudflare.com/callback) cujo host esta
	// máquina não consegue bindar.
	CallbackPort int

	AuthURL   string
	TokenURL  string
	RevokeURL string
	APIBase   string

	// RateLimitRPS é o freio local. O teto do Bling é 3 req/s POR CONTA
	// somando TODOS os apps do lojista, e a API não devolve header de cota
	// nenhum (medido) — não há como reconciliar, só como prever. Default 2.
	RateLimitRPS float64

	// AllowWrite libera comandos de escrita. Default FALSE: o Bling não tem
	// sandbox, então todo experimento roda contra a conta real do lojista.
	AllowWrite bool

	// AllowedCompanyID é a allowlist de contas em que a escrita é permitida,
	// conferida contra o data.id de GET /empresas/me/dados-basicos.
	AllowedCompanyID []string

	HooksPort int
	StateDir  string

	root string
}

// RequireCredentials é conferido só pelos comandos que falam com o ERP; a
// ponte de webhooks sobe sem credencial — mas sem o secret ela não consegue
// VALIDAR a assinatura, e avisa.
func (c *Config) RequireCredentials() error {
	if c.ClientID == "" || c.ClientSecret == "" {
		return fmt.Errorf("BLING_CLIENT_ID e BLING_CLIENT_SECRET são obrigatórios — defina-os em %s/.env", c.root)
	}
	return nil
}

func (c *Config) Root() string { return c.root }

func (c *Config) TokensPath() string { return filepath.Join(c.StateDir, "tokens.json") }
func (c *Config) AuditPath() string  { return filepath.Join(c.StateDir, "audit.jsonl") }
func (c *Config) HooksDir() string   { return filepath.Join(c.StateDir, "webhooks") }

// CallbackPath é o caminho do redirect cadastrado, que o servidor efêmero do
// login precisa servir. Vazio vira "/".
func (c *Config) CallbackPath() string {
	u, err := url.Parse(c.RedirectURI)
	if err != nil || u.Path == "" {
		return "/"
	}
	return u.Path
}

// RedirectIsLocal diz se dá para escutar direto no host do redirect. Quando
// não é local, o login sobe na CallbackPort e o túnel faz a ponte.
func (c *Config) RedirectIsLocal() bool {
	u, err := url.Parse(c.RedirectURI)
	if err != nil {
		return false
	}
	h := u.Hostname()
	return h == "localhost" || h == "127.0.0.1" || h == "::1"
}

// ListenPort é onde o servidor de callback do login sobe de fato.
func (c *Config) ListenPort() int {
	if c.RedirectIsLocal() {
		if u, err := url.Parse(c.RedirectURI); err == nil {
			if p := u.Port(); p != "" {
				if n, err := strconv.Atoi(p); err == nil {
					return n
				}
			}
		}
	}
	return c.CallbackPort
}

func Load() (*Config, error) {
	root, err := moduleRoot()
	if err != nil {
		return nil, err
	}
	loadDotEnv(filepath.Join(root, ".env"))

	c := &Config{
		ClientID:     os.Getenv("BLING_CLIENT_ID"),
		ClientSecret: os.Getenv("BLING_CLIENT_SECRET"),
		RedirectURI:  envOr("BLING_REDIRECT_URI", "http://localhost:8790/callback"),
		AuthURL:      envOr("BLING_AUTH_URL", DefaultAuthURL),
		TokenURL:     envOr("BLING_TOKEN_URL", DefaultTokenURL),
		RevokeURL:    envOr("BLING_REVOKE_URL", DefaultRevokeURL),
		APIBase:      strings.TrimRight(envOr("BLING_API_BASE", DefaultAPIBase), "/"),
		StateDir:     envOr("BLING_STATE_DIR", filepath.Join(root, ".bling-lab")),
		AllowWrite:   strings.EqualFold(os.Getenv("BLING_ALLOW_WRITE"), "true"),
	}

	for _, raw := range strings.Split(os.Getenv("BLING_ALLOWED_COMPANY_ID"), ",") {
		if v := strings.TrimSpace(raw); v != "" {
			c.AllowedCompanyID = append(c.AllowedCompanyID, v)
		}
	}

	if c.CallbackPort, err = atoiEnv("BLING_CALLBACK_PORT", 8790); err != nil {
		return nil, err
	}
	if c.HooksPort, err = atoiEnv("BLING_HOOKS_PORT", 8791); err != nil {
		return nil, err
	}

	rps := envOr("BLING_RATE_LIMIT_RPS", "2")
	if c.RateLimitRPS, err = strconv.ParseFloat(rps, 64); err != nil || c.RateLimitRPS <= 0 {
		return nil, fmt.Errorf("BLING_RATE_LIMIT_RPS inválido: %q", rps)
	}

	c.root = root
	if err := os.MkdirAll(c.StateDir, 0o700); err != nil {
		return nil, fmt.Errorf("criando %s: %w", c.StateDir, err)
	}
	return c, nil
}

func atoiEnv(key string, def int) (int, error) {
	v := envOr(key, strconv.Itoa(def))
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("%s inválido: %q", key, v)
	}
	return n, nil
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// loadDotEnv só preenche chaves ausentes: o ambiente real sempre ganha do arquivo.
func loadDotEnv(path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		val = strings.TrimSpace(val)
		if len(val) >= 2 && (val[0] == '"' || val[0] == '\'') && val[len(val)-1] == val[0] {
			val = val[1 : len(val)-1]
		}
		if _, exists := os.LookupEnv(key); !exists {
			_ = os.Setenv(key, val)
		}
	}
}

func moduleRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if b, err := os.ReadFile(filepath.Join(dir, "go.mod")); err == nil &&
			strings.Contains(string(b), "livecart/bling-lab") {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("go.mod do bling-lab não encontrado a partir do diretório atual")
		}
		dir = parent
	}
}
