package integration

// Separar "o Tiny está me contando algo novo" de "o Tiny está repetindo o que eu
// acabei de fazer".
//
// O webhook traz sempre o SALDO ABSOLUTO, e os dois casos chegam idênticos. Sem
// distinguir, sobram dois extremos ruins:
//
//   - aplicar sempre: o saldo pode ser uma foto tirada ANTES de a nossa saída
//     chegar ao ERP, e aplicá-la devolve o local ao valor antigo, apagando a
//     reserva do comprador;
//   - nunca aplicar enquanto houver reserva viva: cega o LiveCart para os outros
//     canais do lojista pela live inteira. Se ele vender no Mercado Livre nesse
//     intervalo, ficamos oferecendo unidade que já foi.
//
// A fresta real é curta e tem começo e fim conhecidos: vai de baixarmos o
// estoque local (que é o porteiro, e vem primeiro) até o ERP registrar a saída.
// Por isso o mecanismo CONTA chamadas em voo em vez de cronometrar um prazo
// chutado — mais uma folga curta para o webhook viajar.
//
// Histórico do parâmetro, que é o próprio argumento: a supressão já foi "enquanto
// houver reserva ativa" (até 30 minutos) e depois uma janela fixa de 60 segundos.
// Nos dois casos o buraco era o mesmo — venda em outro canal dentro do intervalo
// era perdida, e só se corrigia no webhook seguinte daquele produto, que pode
// nunca vir.

import (
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"
)

func servicoComEco(t *testing.T) *Service {
	t.Helper()
	return &Service{logger: zap.NewNop()}
}

func TestSemMovimentoNossoOSaldoDoERPEhConfiavel(t *testing.T) {
	s := servicoComEco(t)

	if s.erpMovementEchoing("EXT-1") {
		t.Error("sem nunca termos mexido neste produto, nada pode estar ecoando — " +
			"um saldo do Tiny aqui é notícia legítima de outro canal e tem de ser aplicado")
	}
}

// Durante a chamada ao ERP o saldo dele ainda é o de antes. Aplicá-lo devolveria
// o estoque local ao valor anterior e apagaria a reserva.
func TestDuranteAChamadaAoERPOSaldoEhSuspeito(t *testing.T) {
	s := servicoComEco(t)

	s.NoteERPMovementStarted("EXT-1")
	if !s.erpMovementEchoing("EXT-1") {
		t.Error("com chamada ao ERP em voo, o saldo dele é foto do meio do caminho")
	}

	// Produto diferente não é afetado: a contagem é por SKU.
	if s.erpMovementEchoing("EXT-2") {
		t.Error("a contagem vazou para outro produto")
	}
}

// Terminada a chamada e passada a folga, o saldo volta a mandar — é assim que
// uma venda no marketplace chega até nós.
func TestDepoisDaChamadaEDaFolgaOSaldoVoltaAValer(t *testing.T) {
	s := servicoComEco(t)

	s.NoteERPMovementStarted("EXT-1")
	s.NoteERPMovementSent("EXT-1")

	if !s.erpMovementEchoing("EXT-1") {
		t.Error("logo após a chamada o webhook dela ainda não chegou; a folga cobre isso")
	}

	// Envelhece o carimbo além da folga.
	s.erpMovementSentAt.Store("EXT-1", time.Now().Add(-erpMovementEchoGrace-time.Second))

	if s.erpMovementEchoing("EXT-1") {
		t.Error("passada a folga, o saldo do ERP tem de voltar a ser aplicado — " +
			"senão a venda em outro canal é perdida e o estoque fica errado")
	}
}

// Movimentos concorrentes no mesmo produto: a supressão só cai quando o ÚLTIMO
// termina. Um contador (e não um booleano) é o que garante isso.
func TestVariosMovimentosConcorrentesNoMesmoProduto(t *testing.T) {
	s := servicoComEco(t)

	const n = 8
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		s.NoteERPMovementStarted("EXT-1")
	}
	if !s.erpMovementEchoing("EXT-1") {
		t.Fatal("com 8 chamadas em voo, o saldo não pode ser aplicado")
	}

	for i := 0; i < n-1; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); s.NoteERPMovementSent("EXT-1") }()
	}
	wg.Wait()

	if !s.erpMovementEchoing("EXT-1") {
		t.Error("ainda resta uma chamada em voo — a supressão não pode cair antes dela")
	}

	s.NoteERPMovementSent("EXT-1")
	s.erpMovementSentAt.Store("EXT-1", time.Now().Add(-erpMovementEchoGrace-time.Second))
	if s.erpMovementEchoing("EXT-1") {
		t.Error("todas terminaram e a folga passou: o saldo tem de voltar a valer")
	}
}

// A folga cobre a viagem do webhook sem virar cegueira.
//
// O trecho "ERP processa e o webhook chega" levou de 1 a 3 segundos em produção.
// Cinco cobre com margem. O que NÃO pode é crescer: cada segundo aqui é um
// segundo em que uma venda no Mercado Livre não é vista.
func TestFolgaCobreAViagemDoWebhookSemVirarCegueira(t *testing.T) {
	const viagemMedida = 3 * time.Second
	if erpMovementEchoGrace < viagemMedida {
		t.Errorf("folga de %v não cobre a viagem de %v medida em produção — o eco "+
			"chegaria depois e seria lido como venda externa", erpMovementEchoGrace, viagemMedida)
	}
	if erpMovementEchoGrace > 15*time.Second {
		t.Errorf("folga de %v é longa demais: enquanto ela vale, venda do lojista em "+
			"outro canal não é aplicada e o estoque fica errado", erpMovementEchoGrace)
	}
}

// A decisão de sync segue o eco, não a reserva.
func TestModoDeSyncSegueOEcoENaoAReserva(t *testing.T) {
	skip, downgrade := stockSyncMode(false, false, false, false)
	if skip || downgrade {
		t.Errorf("sem movimento nosso em trânsito, o saldo do Tiny tem de ser aplicado "+
			"(skip=%v downgrade=%v) — é a única notícia que temos dos outros canais",
			skip, downgrade)
	}

	skip, downgrade = stockSyncMode(true, false, false, false)
	if !skip {
		t.Error("com movimento nosso em trânsito, aplicar o saldo é copiar o próprio rastro")
	}
	if downgrade {
		t.Error("downgrade-only deixa passar justamente a direção do eco")
	}
}
