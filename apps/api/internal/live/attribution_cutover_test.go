package live

// O marcador de corte da atribuicao (D26 / migration 000119) tem de CHEGAR na
// resposta da metrica.
//
// Por que este teste existe: um marcador que so e gravado no banco e nunca lido
// nao avisa ninguem — e o mesmo destino de order_items.session_id e de
// live_sessions.publish_at, que passaram meses escritos e sem leitor. O corte
// so cumpre a funcao dele quando a tela consegue dizer "antes desta data, este
// numero significava outra coisa".

import (
	"context"
	"testing"

	"go.uber.org/zap"
)

func TestSessionMetricsCarregaOMarcadorDeCorte(t *testing.T) {
	requireDB(t)
	ctx := context.Background()

	eventID := seedEvent(t)
	storeID := storeOf(t, eventID)
	seedSession(t, eventID, 1)

	svc := NewService(testRepo, zap.NewNop())
	out, err := svc.GetSessionMetrics(ctx, eventID, storeID)
	if err != nil {
		t.Fatalf("GetSessionMetrics: %v", err)
	}

	if out.AttributionCutoverAt == nil {
		t.Fatal("a resposta veio sem o instante do corte — a 000119 grava metric_cutovers e ninguem le")
	}
	if out.AttributionCutoverNote == "" {
		t.Error("o corte veio sem nota; a nota E o produto aqui, o timestamp sozinho nao explica nada")
	}

	// A sessao foi criada AGORA, depois do corte: ela nao carrega historico
	// first-touch e nao pode disparar a ressalva na tela.
	if len(out.Sessions) != 1 {
		t.Fatalf("sessoes = %d, quero 1", len(out.Sessions))
	}
	if got := out.Sessions[0].AttributionSource; got != "addition_log" {
		t.Errorf("attribution_source = %q, quero addition_log — sessao nascida depois do corte nao tem ressalva a fazer", got)
	}
}

// Uma sessao anterior ao corte tem de sair marcada, senao a tela nao tem como
// avisar em QUAL transmissao o numero mudou de definicao.
func TestSessionAnteriorAoCorteSaiMarcadaFirstTouch(t *testing.T) {
	requireDB(t)
	ctx := context.Background()

	eventID := seedEvent(t)
	storeID := storeOf(t, eventID)
	sessionID := seedSession(t, eventID, 1)

	// A 000119 marca por created_at. Envelhecer a linha reproduz exatamente o
	// que a migration faria com uma transmissao legada.
	if _, err := testPool.Exec(ctx,
		`UPDATE live_sessions SET attribution_source = 'first_touch' WHERE id = $1::uuid`, sessionID,
	); err != nil {
		t.Fatalf("envelhecer sessao: %v", err)
	}

	svc := NewService(testRepo, zap.NewNop())
	out, err := svc.GetSessionMetrics(ctx, eventID, storeID)
	if err != nil {
		t.Fatalf("GetSessionMetrics: %v", err)
	}
	if len(out.Sessions) != 1 || out.Sessions[0].AttributionSource != "first_touch" {
		t.Fatalf("a marca first_touch nao chegou na resposta: %+v", out.Sessions)
	}
}
