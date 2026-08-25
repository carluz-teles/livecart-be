// Package config carrega a configuração do tiny-lab a partir do ambiente e de
// um arquivo .env local. Nenhum segredo tem valor padrão: se não vier do
// ambiente, o comando falha em vez de silenciosamente usar outra coisa.
package config

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Endpoints fixos da API v3 do Tiny/Olist. O host é ÚNICO — não existe um
// ambiente de sandbox separado do lado do fornecedor, o que torna o guard de
// conta (ver package tiny) a única proteção real contra escrever em produção.
const (
	DefaultAuthURL  = "https://accounts.tiny.com.br/realms/tiny/protocol/openid-connect/auth"
	DefaultTokenURL = "https://accounts.tiny.com.br/realms/tiny/protocol/openid-connect/token"
	DefaultAPIBase  = "https://api.tiny.com.br/public-api/v3"
)

type Config struct {
	ClientID     string
	ClientSecret string
	RedirectURI  string
	Env          string // "sandbox" habilita escrita; qualquer outro valor a proíbe.
	AuthURL      string
	TokenURL     string
	APIBase      string

	// AllowedCNPJ é a allowlist de contas em que a escrita é permitida,
	// conferida contra o cpfCnpj devolvido por GET /info. Só dígitos.
	AllowedCNPJ []string

	// PKCE liga o code_challenge S256. O fluxo do backend em produção NÃO usa
	// PKCE; mantemos ligado por padrão (é estritamente mais seguro) com um
	// interruptor para o caso de o client no Keycloak não aceitar.
	PKCE bool

	HooksPort int
	StateDir  string

	root string
}

// RequireCredentials é conferido só pelos comandos que falam com o ERP; a
// ponte de webhooks sobe sem credencial nenhuma.
func (c *Config) RequireCredentials() error {
	if c.ClientID == "" || c.ClientSecret == "" {
		return fmt.Errorf("TINY_CLIENT_ID e TINY_CLIENT_SECRET são obrigatórios — defina-os em %s/.env", c.root)
	}
	return nil
}

// Root é a raiz do módulo tiny-lab, usada nas mensagens de ajuda.
func (c *Config) Root() string { return c.root }

// TokensPath, AuditPath e HooksDir derivam do StateDir. Ficam todos sob um
// diretório único para que um só .gitignore cubra tudo que é local.
func (c *Config) TokensPath() string { return filepath.Join(c.StateDir, "tokens.json") }
func (c *Config) AuditPath() string  { return filepath.Join(c.StateDir, "audit.jsonl") }
func (c *Config) HooksDir() string   { return filepath.Join(c.StateDir, "webhooks") }

// WriteAllowed diz se comandos de escrita podem sequer ser tentados. É o
// primeiro de dois portões; o segundo confere a conta de verdade (GET /info).
func (c *Config) WriteAllowed() bool { return c.Env == "sandbox" }

// Load lê o .env (se existir) e depois o ambiente, que tem precedência.
func Load() (*Config, error) {
	root, err := moduleRoot()
	if err != nil {
		return nil, err
	}
	loadDotEnv(filepath.Join(root, ".env"))

	c := &Config{
		ClientID:     os.Getenv("TINY_CLIENT_ID"),
		ClientSecret: os.Getenv("TINY_CLIENT_SECRET"),
		RedirectURI:  envOr("TINY_REDIRECT_URI", "http://localhost:8080/oauth/tiny/callback"),
		Env:          envOr("TINY_ENV", ""),
		AuthURL:      envOr("TINY_AUTH_URL", DefaultAuthURL),
		TokenURL:     envOr("TINY_TOKEN_URL", DefaultTokenURL),
		APIBase:      strings.TrimRight(envOr("TINY_API_BASE", DefaultAPIBase), "/"),
		StateDir:     envOr("TINY_STATE_DIR", filepath.Join(root, ".tiny-lab")),
		PKCE:         !strings.EqualFold(envOr("TINY_PKCE", "true"), "false"),
	}

	for _, raw := range strings.Split(os.Getenv("TINY_ALLOWED_CNPJ"), ",") {
		if d := OnlyDigits(raw); d != "" {
			c.AllowedCNPJ = append(c.AllowedCNPJ, d)
		}
	}

	c.HooksPort, err = strconv.Atoi(envOr("TINY_HOOKS_PORT", "8787"))
	if err != nil {
		return nil, fmt.Errorf("TINY_HOOKS_PORT inválido: %w", err)
	}

	c.root = root
	if err := os.MkdirAll(c.StateDir, 0o700); err != nil {
		return nil, fmt.Errorf("criando %s: %w", c.StateDir, err)
	}
	return c, nil
}

// OnlyDigits normaliza um CNPJ/CPF para comparação — o ERP devolve formatado.
func OnlyDigits(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
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

// moduleRoot sobe a partir do executável/cwd até achar o go.mod do tiny-lab,
// para que os comandos funcionem de qualquer diretório.
func moduleRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if b, err := os.ReadFile(filepath.Join(dir, "go.mod")); err == nil &&
			strings.Contains(string(b), "livecart/tiny-lab") {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("go.mod do tiny-lab não encontrado a partir do diretório atual")
		}
		dir = parent
	}
}
