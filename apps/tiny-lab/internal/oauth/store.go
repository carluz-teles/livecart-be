// Package oauth implementa o fluxo authorization_code do Keycloak do Tiny e a
// persistência local do par access/refresh token, com renovação automática.
package oauth

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Tokens é o que fica em disco. O arquivo é 0600 e vive num diretório
// ignorado pelo git — nunca entra em commit, log ou fixture.
type Tokens struct {
	AccessToken      string    `json:"access_token"`
	RefreshToken     string    `json:"refresh_token"`
	TokenType        string    `json:"token_type"`
	Scope            string    `json:"scope,omitempty"`
	ExpiresAt        time.Time `json:"expires_at"`
	RefreshExpiresAt time.Time `json:"refresh_expires_at,omitempty"`
	ObtainedAt       time.Time `json:"obtained_at"`

	// Conta observada na última chamada a GET /info. Guardada para que
	// `auth status` mostre em que empresa o token manda sem gastar request.
	AccountCNPJ string `json:"account_cnpj,omitempty"`
	AccountName string `json:"account_name,omitempty"`
}

// Expired usa uma folga de 60s para não correr o risco de mandar um token que
// vence no meio do voo.
func (t *Tokens) Expired() bool {
	return t == nil || t.AccessToken == "" || time.Now().After(t.ExpiresAt.Add(-60*time.Second))
}

// RefreshUsable diz se ainda dá para renovar sem novo login interativo.
func (t *Tokens) RefreshUsable() bool {
	if t == nil || t.RefreshToken == "" {
		return false
	}
	return t.RefreshExpiresAt.IsZero() || time.Now().Before(t.RefreshExpiresAt.Add(-30*time.Second))
}

func LoadTokens(path string) (*Tokens, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("nenhum token salvo em %s — rode `tiny-lab auth login`", path)
		}
		return nil, err
	}
	var t Tokens
	if err := json.Unmarshal(b, &t); err != nil {
		return nil, fmt.Errorf("token store corrompido (%s): %w", path, err)
	}
	return &t, nil
}

// SaveTokens escreve atomicamente: um Ctrl-C no meio não pode deixar o arquivo
// pela metade e obrigar um login novo.
func SaveTokens(path string, t *Tokens) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
