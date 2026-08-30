package oauth

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Tokens é o que fica em disco entre execuções.
//
// RefreshExpiresAt merece nota: o Bling NÃO devolve o prazo do refresh token
// na resposta — a doc diz "30 dias" em prosa e mais nada. Guardamos a data de
// EMISSÃO e derivamos o vencimento, porque a pergunta que importa em produção
// ("a loja parada 30 dias perde a conexão?") depende de saber se o relógio
// reinicia a cada refresh ou corre desde a autorização. Só medindo se descobre;
// enquanto não se mediu, o campo Rotacionou registra o que foi observado.
type Tokens struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	TokenType    string `json:"token_type"`
	Scope        string `json:"scope,omitempty"`

	ObtainedAt time.Time `json:"obtained_at"`
	ExpiresAt  time.Time `json:"expires_at"`

	// RefreshObtainedAt é quando ESTE refresh token chegou. Se o Bling rotaciona,
	// avança a cada renovação; se não rotaciona, fica preso na autorização.
	RefreshObtainedAt time.Time `json:"refresh_obtained_at"`

	// Rotacionou registra a observação da última renovação: o refresh token
	// devolvido é diferente do enviado? Nil = ainda não renovou nenhuma vez.
	Rotacionou *bool `json:"rotacionou,omitempty"`

	// Identidade da conta, preenchida por `bling-lab empresa`.
	CompanyID   string `json:"company_id,omitempty"`
	CompanyName string `json:"company_name,omitempty"`
	CompanyDoc  string `json:"company_doc,omitempty"`
}

// RefreshValidade é o prazo que a doc do Bling declara em prosa. Não vem na
// resposta do token endpoint, então é constante nossa, não dado do provedor.
const RefreshValidade = 30 * 24 * time.Hour

func (t *Tokens) Expired() bool {
	// 60 s de folga: o exchange leva tempo e um token que vence no meio do
	// comando é pior do que um refresh a mais.
	return time.Now().Add(60 * time.Second).After(t.ExpiresAt)
}

func (t *Tokens) RefreshExpiresAt() time.Time {
	base := t.RefreshObtainedAt
	if base.IsZero() {
		base = t.ObtainedAt
	}
	return base.Add(RefreshValidade)
}

func (t *Tokens) RefreshUsable() bool {
	return t.RefreshToken != "" && time.Now().Before(t.RefreshExpiresAt())
}

func LoadTokens(path string) (*Tokens, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("nenhum token em %s — rode `bling-lab auth login` primeiro", path)
		}
		return nil, err
	}
	var t Tokens
	if err := json.Unmarshal(b, &t); err != nil {
		return nil, fmt.Errorf("tokens.json corrompido: %w", err)
	}
	return &t, nil
}

func SaveTokens(path string, t *Tokens) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o600)
}
