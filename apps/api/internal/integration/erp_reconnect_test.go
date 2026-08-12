package integration

// Reconectar o MESMO ERP, que era o caminho de recuperação e estava quebrado.
//
// Medido em produção em 12/08/2026: o token do Tiny venceu em 09/08 18:40, o
// refresh falhou, `refreshToken` marcou a integração como 'error' e o lojista
// ficou sem saída. O botão de sincronizar desaparece — o front exige status
// 'active' — e quatro tentativas de reconectar devolveram 500:
//
//	duplicate key value violates unique constraint
//	"uniq_integrations_store_one_erp" (SQLSTATE 23505)
//
// A guarda em Create() só barrava ERP DIFERENTE ("desconecte o outro antes"),
// então reconectar o mesmo passava por ela e ia direto para o INSERT, contra um
// índice único parcial de um ERP por loja.
//
// O índice não é o problema — a regra de um ERP por loja é intencional. O
// problema era tratar reconexão como criação.

import (
	"context"
	"encoding/base64"
	"fmt"
	"testing"
	"time"

	"go.uber.org/zap"

	"livecart/apps/api/internal/integration/providers"
	"livecart/apps/api/lib/crypto"
)

func reconnectTestService(t *testing.T) *Service {
	t.Helper()
	// Chave de teste: 32 bytes, só para o ciclo encrypt/decrypt do teste.
	enc, err := crypto.NewEncryptor(base64.StdEncoding.EncodeToString([]byte("chave-de-teste-com-32-bytes!!!!!")))
	if err != nil {
		t.Fatalf("encryptor de teste: %v", err)
	}
	return &Service{repo: testRepo, encryptor: enc, logger: zap.NewNop()}
}

func seedStoreForReconnect(t *testing.T) string {
	t.Helper()
	var storeID string
	slug := fmt.Sprintf("reconn-%d", time.Now().UnixNano())
	if err := testPool.QueryRow(context.Background(),
		`INSERT INTO stores (name, slug) VALUES ('Loja Reconexão', $1) RETURNING id::text`, slug,
	).Scan(&storeID); err != nil {
		t.Fatalf("seed store: %v", err)
	}
	return storeID
}

func TestReconectarMesmoERPReaproveitaALinha(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	svc := reconnectTestService(t)
	storeID := seedStoreForReconnect(t)

	primeira, err := svc.Create(ctx, CreateIntegrationInput{
		StoreID:     storeID,
		Type:        string(providers.ProviderTypeERP),
		Provider:    "tiny",
		Credentials: &providers.Credentials{AccessToken: "token-velho", RefreshToken: "refresh-velho"},
	})
	if err != nil {
		t.Fatalf("primeira conexão: %v", err)
	}

	// O estado real em que o lojista fica preso: o refresh falhou e a integração
	// foi marcada como 'error'.
	if err := testRepo.UpdateStatus(ctx, primeira.ID, "error"); err != nil {
		t.Fatalf("simulando o estado de erro: %v", err)
	}

	segunda, err := svc.Create(ctx, CreateIntegrationInput{
		StoreID:     storeID,
		Type:        string(providers.ProviderTypeERP),
		Provider:    "tiny",
		Credentials: &providers.Credentials{AccessToken: "token-novo", RefreshToken: "refresh-novo"},
	})
	if err != nil {
		t.Fatalf("reconectar o mesmo ERP falhou: %v\n"+
			"É o caminho de recuperação: token vencido, integração em 'error', "+
			"e o lojista precisa reconectar.", err)
	}

	// MESMA linha: integration_logs e webhook_events referenciam este id, e a URL
	// de webhook que o lojista cadastrou no ERP carrega ele.
	if segunda.ID != primeira.ID {
		t.Errorf("reconexão criou linha nova (%s != %s) — a trilha de auditoria e a "+
			"URL de webhook já cadastrada apontam para o id antigo",
			segunda.ID, primeira.ID)
	}

	// E o status precisa sair de 'error', senão o botão de sincronizar continua
	// escondido e nada muda para o lojista.
	if segunda.Status == "error" {
		t.Errorf("status continuou %q depois de reconectar — o front esconde o botão "+
			"de sincronizar em tudo que não seja 'active'", segunda.Status)
	}

	// Uma linha ERP por loja, ainda.
	var n int
	if err := testPool.QueryRow(ctx,
		`SELECT count(*) FROM integrations WHERE store_id = $1::uuid AND type = 'erp'`, storeID,
	).Scan(&n); err != nil {
		t.Fatalf("contando integrações: %v", err)
	}
	if n != 1 {
		t.Errorf("loja ficou com %d integrações ERP, quero 1", n)
	}
}

// As credenciais NOVAS precisam valer, senão a reconexão é decorativa: o status
// volta a 'pending_auth' e o ERP segue recusando com o token velho.
func TestReconexaoGravaAsCredenciaisNovas(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	svc := reconnectTestService(t)
	storeID := seedStoreForReconnect(t)

	if _, err := svc.Create(ctx, CreateIntegrationInput{
		StoreID:     storeID,
		Type:        string(providers.ProviderTypeERP),
		Provider:    "tiny",
		Credentials: &providers.Credentials{AccessToken: "token-velho"},
	}); err != nil {
		t.Fatalf("primeira conexão: %v", err)
	}

	novoPrazo := time.Now().Add(4 * time.Hour).UTC().Truncate(time.Second)
	out, err := svc.Create(ctx, CreateIntegrationInput{
		StoreID:  storeID,
		Type:     string(providers.ProviderTypeERP),
		Provider: "tiny",
		Credentials: &providers.Credentials{
			AccessToken: "token-novo",
			ExpiresAt:   novoPrazo,
		},
	})
	if err != nil {
		t.Fatalf("reconectar: %v", err)
	}

	row, err := testRepo.GetByID(ctx, out.ID, storeID)
	if err != nil {
		t.Fatalf("relendo a integração: %v", err)
	}
	creds, err := svc.decryptCredentials(row.Credentials)
	if err != nil {
		t.Fatalf("lendo credenciais gravadas: %v", err)
	}
	if creds.AccessToken != "token-novo" {
		t.Errorf("token gravado = %q, quero o novo — reconectar sem trocar a "+
			"credencial deixa o ERP recusando igual", creds.AccessToken)
	}

	var expira *time.Time
	if err := testPool.QueryRow(ctx,
		`SELECT token_expires_at FROM integrations WHERE id = $1::uuid`, out.ID,
	).Scan(&expira); err != nil {
		t.Fatalf("lendo token_expires_at: %v", err)
	}
	if expira == nil || !expira.UTC().Truncate(time.Second).Equal(novoPrazo) {
		t.Errorf("token_expires_at = %v, quero %v — com o prazo velho o refresh "+
			"dispara de novo na primeira chamada", expira, novoPrazo)
	}
}

// Trocar de ERP continua barrado com mensagem, e não com violação de constraint.
// A regra de um ERP por loja é intencional; o que estava errado era confundir
// reconexão com criação.
func TestTrocarDeERPContinuaBarrado(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	svc := reconnectTestService(t)
	storeID := seedStoreForReconnect(t)

	if _, err := svc.Create(ctx, CreateIntegrationInput{
		StoreID:     storeID,
		Type:        string(providers.ProviderTypeERP),
		Provider:    "tiny",
		Credentials: &providers.Credentials{AccessToken: "t"},
	}); err != nil {
		t.Fatalf("primeira conexão: %v", err)
	}

	_, err := svc.Create(ctx, CreateIntegrationInput{
		StoreID:     storeID,
		Type:        string(providers.ProviderTypeERP),
		Provider:    "bling",
		Credentials: &providers.Credentials{AccessToken: "t"},
	})
	if err == nil {
		t.Fatal("conectar um ERP diferente passou — a loja ficaria com dois ERPs")
	}
	// Mensagem para o lojista, não SQLSTATE 23505 vazando como 500.
	if got := err.Error(); !contemTiny(got) {
		t.Errorf("erro = %q; esperava mensagem legível citando o ERP já conectado", got)
	}
}

func contemTiny(s string) bool {
	for i := 0; i+4 <= len(s); i++ {
		if s[i:i+4] == "tiny" {
			return true
		}
	}
	return false
}
