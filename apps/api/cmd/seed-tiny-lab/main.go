// seed-tiny-lab cria (ou atualiza) a integração Tiny de uma loja no banco LOCAL,
// a partir dos tokens que o `apps/tiny-lab` já obteve por OAuth.
//
// Existe para que os webhooks reais do Tiny possam ser exercitados pela
// aplicação de verdade num ambiente local, sem depender de staging nem de
// produção. Usa o encryptor do próprio app, para o blob ficar no formato exato
// que o serviço espera ler.
//
// Recusa rodar contra qualquer banco que não seja local — o alvo é um ambiente
// de laboratório, e escrever credencial em staging/produção por engano seria o
// pior resultado possível.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"livecart/apps/api/internal/integration/providers"
	"livecart/apps/api/lib/config"
	"livecart/apps/api/lib/crypto"
)

type tokensNoDisco struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	TokenType    string    `json:"token_type"`
	ExpiresAt    time.Time `json:"expires_at"`
	AccountCNPJ  string    `json:"account_cnpj"`
	AccountName  string    `json:"account_name"`
}

func main() {
	var (
		storeID    = flag.String("store", "", "UUID da loja (obrigatório)")
		tokensPath = flag.String("tokens", "apps/tiny-lab/.tiny-lab/tokens.json", "tokens.json do tiny-lab")
		envPath    = flag.String("env", "apps/tiny-lab/.env", ".env do tiny-lab (client_id/secret)")
	)
	flag.Parse()

	if err := run(*storeID, *tokensPath, *envPath); err != nil {
		fmt.Fprintf(os.Stderr, "erro: %v\n", err)
		os.Exit(1)
	}
}

func run(storeID, tokensPath, envPath string) error {
	if storeID == "" {
		return fmt.Errorf("-store é obrigatório")
	}

	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		return fmt.Errorf("DATABASE_URL não definida")
	}
	// Guard: só banco local. A alternativa é semear credencial em produção.
	if !strings.Contains(dsn, "localhost") && !strings.Contains(dsn, "127.0.0.1") {
		return fmt.Errorf("recusando rodar: DATABASE_URL não é local (%s…)", dsn[:min(len(dsn), 40)])
	}

	toks, err := lerTokens(tokensPath)
	if err != nil {
		return err
	}
	clientID, clientSecret, err := lerCredenciaisDoEnv(envPath)
	if err != nil {
		return err
	}

	chave := config.EncryptionKey.String()
	if chave == "" {
		return fmt.Errorf("ENCRYPTION_KEY não definida (o app usa a mesma para ler)")
	}
	enc, err := crypto.NewEncryptor(chave)
	if err != nil {
		return fmt.Errorf("montando o encryptor: %w", err)
	}

	creds := providers.Credentials{
		AccessToken:  toks.AccessToken,
		RefreshToken: toks.RefreshToken,
		TokenType:    orDefault(toks.TokenType, "Bearer"),
		ExpiresAt:    toks.ExpiresAt,
		Extra: map[string]any{
			"client_id":     clientID,
			"client_secret": clientSecret,
		},
	}
	blob, err := enc.EncryptJSON(creds)
	if err != nil {
		return fmt.Errorf("cifrando as credenciais: %w", err)
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return fmt.Errorf("conectando: %w", err)
	}
	defer pool.Close()

	var id string
	err = pool.QueryRow(ctx, `
		INSERT INTO integrations (store_id, type, provider, status, credentials, token_expires_at)
		VALUES ($1, 'erp', 'tiny', 'active', $2, $3)
		ON CONFLICT DO NOTHING
		RETURNING id`, storeID, blob, toks.ExpiresAt).Scan(&id)
	if err != nil {
		// Sem índice único, ON CONFLICT não dispara; atualiza a que já existe.
		err = pool.QueryRow(ctx, `
			UPDATE integrations SET credentials=$2, token_expires_at=$3, status='active'
			WHERE store_id=$1 AND type='erp' AND provider='tiny'
			RETURNING id`, storeID, blob, toks.ExpiresAt).Scan(&id)
		if err != nil {
			return fmt.Errorf("gravando a integração: %w", err)
		}
	}

	fmt.Printf("integração Tiny pronta\n")
	fmt.Printf("  id            %s\n", id)
	fmt.Printf("  loja          %s\n", storeID)
	fmt.Printf("  conta         %s (%s)\n", orDefault(toks.AccountName, "—"), orDefault(toks.AccountCNPJ, "—"))
	fmt.Printf("  token vence   %s\n", toks.ExpiresAt.Local().Format(time.RFC3339))
	return nil
}

func lerTokens(path string) (*tokensNoDisco, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("lendo %s (rode `tiny-lab auth login` antes): %w", path, err)
	}
	var t tokensNoDisco
	if err := json.Unmarshal(b, &t); err != nil {
		return nil, fmt.Errorf("%s não é o JSON esperado: %w", path, err)
	}
	if t.AccessToken == "" {
		return nil, fmt.Errorf("%s não tem access_token", path)
	}
	return &t, nil
}

func lerCredenciaisDoEnv(path string) (string, string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", "", fmt.Errorf("lendo %s: %w", path, err)
	}
	var id, secret string
	for _, linha := range strings.Split(string(b), "\n") {
		linha = strings.TrimSpace(linha)
		if linha == "" || strings.HasPrefix(linha, "#") {
			continue
		}
		k, v, ok := strings.Cut(linha, "=")
		if !ok {
			continue
		}
		v = strings.Trim(strings.TrimSpace(v), `"'`)
		switch strings.TrimSpace(k) {
		case "TINY_CLIENT_ID":
			id = v
		case "TINY_CLIENT_SECRET":
			secret = v
		}
	}
	if id == "" || secret == "" {
		return "", "", fmt.Errorf("%s sem TINY_CLIENT_ID/TINY_CLIENT_SECRET", path)
	}
	return id, secret, nil
}

func orDefault(v, def string) string {
	if strings.TrimSpace(v) == "" {
		return def
	}
	return v
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
