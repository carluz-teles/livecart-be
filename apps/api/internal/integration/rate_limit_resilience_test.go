package integration

// Rate limit é pausa, não parada — e 'error' precisa ter saída.
//
// Reconstituído do banco de produção em 12/08/2026:
//
//	09/08 ~14:40    último refresh bem-sucedido (token do Tiny dura 4h)
//	09/08 16:56:59  HTTP 429 do Tiny -> integração marcada 'error'
//	09/08 18:40:04  token vence naturalmente, 1h43 DEPOIS
//	09/08 -> 12/08  três dias de ERP parado
//
// Quem derrubou foi o 429, não a expiração. E 'error' era terminal: os únicos
// pontos que escreviam 'active' eram os fluxos de conexão, então nada se
// recuperava sozinho. O lojista via o botão de sincronizar desaparecer sem
// explicação, e reconectar à mão — o único caminho — devolvia 500.
//
// Com pico medido de 287 comentários por minuto numa live real, 429 do ERP não
// é hipótese: é rotina. Se cada um matar a integração, o estoque para de
// sincronizar no meio da venda.

import (
	"context"
	"encoding/base64"
	"errors"
	"testing"
	"time"

	"go.uber.org/zap"

	"livecart/apps/api/internal/integration/providers"
	"livecart/apps/api/lib/crypto"
	"livecart/apps/api/lib/ratelimit"
)

func resilienceTestService(t *testing.T) *Service {
	t.Helper()
	enc, err := crypto.NewEncryptor(base64.StdEncoding.EncodeToString([]byte("chave-de-teste-com-32-bytes!!!!!")))
	if err != nil {
		t.Fatalf("encryptor de teste: %v", err)
	}
	return &Service{repo: testRepo, encryptor: enc, logger: zap.NewNop()}
}

func seedERPIntegration(t *testing.T, svc *Service) (integrationID, storeID string) {
	t.Helper()
	storeID = seedStoreForReconnect(t)
	out, err := svc.Create(context.Background(), CreateIntegrationInput{
		StoreID:     storeID,
		Type:        string(providers.ProviderTypeERP),
		Provider:    "tiny",
		Credentials: &providers.Credentials{AccessToken: "t"},
	})
	if err != nil {
		t.Fatalf("seed integração: %v", err)
	}
	return out.ID, storeID
}

func statusDe(t *testing.T, integrationID string) string {
	t.Helper()
	var s string
	if err := testPool.QueryRow(context.Background(),
		`SELECT status FROM integrations WHERE id = $1::uuid`, integrationID).Scan(&s); err != nil {
		t.Fatalf("lendo status: %v", err)
	}
	return s
}

func TestRateLimitNaoDerrubaAIntegracao(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	svc := resilienceTestService(t)
	id, _ := seedERPIntegration(t, svc)

	if err := testRepo.UpdateStatus(ctx, id, "active"); err != nil {
		t.Fatalf("deixando ativa: %v", err)
	}

	// Exatamente o que o Tiny devolveu às 16:56:59.
	svc.handleProviderError(ctx, id, "search_products",
		&ratelimit.ErrRateLimited{RetryAfter: 30 * time.Second})

	if got := statusDe(t, id); got != "active" {
		t.Errorf("status virou %q depois de um 429 — rate limit é transitório, e "+
			"marcar estado permanente a partir dele custou 3 dias de ERP parado", got)
	}
}

// A saída que não existia: uma chamada boa cura a integração.
func TestChamadaBemSucedidaCuraAIntegracao(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	svc := resilienceTestService(t)
	id, _ := seedERPIntegration(t, svc)

	if err := testRepo.UpdateStatus(ctx, id, "error"); err != nil {
		t.Fatalf("simulando o estado travado: %v", err)
	}

	// É assim que toda chamada HTTP de provider reporta (providers/base.go).
	if err := svc.LogIntegrationOperation(ctx, providers.IntegrationLog{
		IntegrationID: id,
		Direction:     "outbound",
		Status:        "success",
	}); err != nil {
		t.Fatalf("registrando operação: %v", err)
	}

	if got := statusDe(t, id); got != "active" {
		t.Errorf("status continuou %q depois de uma chamada bem-sucedida — sem cura "+
			"automática o lojista fica preso até reconectar à mão", got)
	}
}

// Curar não pode atropelar estado deliberado. 'pending_auth' é autorização em
// andamento e 'disconnected' é o lojista tendo desligado de propósito; virar
// qualquer um dos dois em 'active' seria mentir sobre o que está configurado.
func TestCuraNaoAtropelaEstadoDeliberado(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	svc := resilienceTestService(t)

	for _, estado := range []string{"pending_auth", "disconnected"} {
		id, _ := seedERPIntegration(t, svc)
		if err := testRepo.UpdateStatus(ctx, id, estado); err != nil {
			t.Fatalf("montando estado %q: %v", estado, err)
		}

		if err := svc.LogIntegrationOperation(ctx, providers.IntegrationLog{
			IntegrationID: id,
			Direction:     "outbound",
			Status:        "success",
		}); err != nil {
			t.Fatalf("registrando operação: %v", err)
		}

		if got := statusDe(t, id); got != estado {
			t.Errorf("estado %q virou %q — a cura só pode agir sobre 'error'", estado, got)
		}
	}
}

// Uma falha comum (não rate limit) também não deve derrubar por este caminho —
// handleProviderError só reagia a rate limit e continua assim.
func TestErroComumNaoMudaStatusPorEsteCaminho(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	svc := resilienceTestService(t)
	id, _ := seedERPIntegration(t, svc)

	if err := testRepo.UpdateStatus(ctx, id, "active"); err != nil {
		t.Fatalf("deixando ativa: %v", err)
	}

	svc.handleProviderError(ctx, id, "search_products", errors.New("HTTP 500: boom"))

	if got := statusDe(t, id); got != "active" {
		t.Errorf("status virou %q por um erro comum", got)
	}
}

// O ciclo inteiro, que é o que o lojista vive: 429 no meio da live, chamada
// seguinte volta a funcionar, e o sistema segue sem intervenção humana.
func TestCicloCompletoDe429ARecuperacao(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	svc := resilienceTestService(t)
	id, _ := seedERPIntegration(t, svc)

	if err := testRepo.UpdateStatus(ctx, id, "active"); err != nil {
		t.Fatalf("deixando ativa: %v", err)
	}

	svc.handleProviderError(ctx, id, "reserve_stock",
		&ratelimit.ErrRateLimited{RetryAfter: 5 * time.Second})
	if got := statusDe(t, id); got != "active" {
		t.Fatalf("o 429 derrubou a integração no meio da live: status = %q", got)
	}

	// Mesmo que algo mais a tivesse marcado, a próxima resposta boa resolve.
	if err := testRepo.UpdateStatus(ctx, id, "error"); err != nil {
		t.Fatalf("forçando error: %v", err)
	}
	if err := svc.LogIntegrationOperation(ctx, providers.IntegrationLog{
		IntegrationID: id, Direction: "outbound", Status: "success",
	}); err != nil {
		t.Fatalf("registrando operação: %v", err)
	}
	if got := statusDe(t, id); got != "active" {
		t.Errorf("não se recuperou sozinha: status = %q", got)
	}
}
