package erp

// Simulação de uma live enquanto o mesmo produto vende em OUTROS canais.
//
// O cenário que o dono do produto levantou, e que nenhum teste cobria: o
// lojista vende o mesmo SKU no LiveCart, no Mercado Livre e no site dele. O
// Tiny agrega tudo e é a única fonte que sabe o total real. Ele nos avisa por
// webhook, sempre com o SALDO ABSOLUTO — nunca com "vendeu 1".
//
// A pergunta é se o nosso contador sobrevive a isso. A resposta depende
// inteiramente de quem manda no número, e por isso a simulação roda os DOIS
// desenhos lado a lado sobre exatamente os mesmos eventos:
//
//	MODELO A (hoje): products.stock é contador NOSSO. A gente decrementa ao
//	reservar, e o webhook do Tiny tenta se encaixar por cima — hoje suprimido
//	enquanto há reserva viva, justamente porque não dá para distinguir o eco do
//	nosso próprio movimento de uma venda externa.
//
//	MODELO B (proposto): products.stock é ESPELHO do Tiny, escrito só pelo
//	webhook. O que seguramos vive num contador de pendentes separado, e a
//	disponibilidade é `espelho − pendentes`.
//
// O webhook chega com atraso variável — medido em produção entre 1 e 50
// segundos, dependendo de retentativa. A simulação varia esse atraso de
// propósito: é nele que os dois modelos se separam.

import (
	"fmt"
	"testing"
)

// tinySimulado é o ERP: guarda o saldo ABSOLUTO e aceita qualquer lançamento,
// inclusive levando a negativo — comportamento real, gravado na bateria de
// sandbox (lançar sobre saldo 0 leva a -1, sem erro). Ou seja: o Tiny não
// recusa venda a descoberto, e portanto NÃO pode ser o porteiro.
type tinySimulado struct {
	saldo    int
	webhooks []int // fila de saldos a entregar, na ordem
}

func (t *tinySimulado) saida(qtd int) {
	t.saldo -= qtd
	t.webhooks = append(t.webhooks, t.saldo)
}

func (t *tinySimulado) entrada(qtd int) {
	t.saldo += qtd
	t.webhooks = append(t.webhooks, t.saldo)
}

// vendaExterna é o Mercado Livre, o site, o balcão. Nós não ficamos sabendo por
// nenhum caminho que não seja o webhook.
func (t *tinySimulado) vendaExterna(qtd int) {
	t.saldo -= qtd
	t.webhooks = append(t.webhooks, t.saldo)
}

func (t *tinySimulado) reposicaoDoLojista(qtd int) {
	t.saldo += qtd
	t.webhooks = append(t.webhooks, t.saldo)
}

// entregarPendentes simula a chegada dos webhooks atrasados.
func (t *tinySimulado) entregarPendentes(aplicar func(saldo int)) {
	for _, s := range t.webhooks {
		aplicar(s)
	}
	t.webhooks = nil
}

// --- MODELO A: contador nosso, webhook suprimido enquanto há reserva ---------

type modeloAtual struct {
	local    int
	segurado int
}

func (m *modeloAtual) disponivel() int { return m.local }

func (m *modeloAtual) reservar(qtd int) bool {
	if m.local < qtd {
		return false
	}
	m.local -= qtd
	m.segurado += qtd
	return true
}

func (m *modeloAtual) aplicarWebhook(saldo int) {
	// A supressão em vigor: com reserva viva, o saldo do ERP é ignorado, porque
	// não dá para saber se ele veio do nosso movimento ou de outro canal.
	if m.segurado > 0 {
		return
	}
	m.local = saldo
}

// --- MODELO B: espelho do Tiny + pendentes ----------------------------------

type modeloEspelho struct {
	espelho   int // último saldo que o Tiny nos contou
	pendentes int // movimentos nossos ainda não refletidos no espelho
}

// disponivel desconta o que já mandamos ao Tiny mas ainda não voltou pelo
// webhook. É a janela de 1 a 50s medida em produção; sem esse desconto, dois
// compradores na mesma janela vendem a mesma unidade.
func (m *modeloEspelho) disponivel() int {
	d := m.espelho - m.pendentes
	if d < 0 {
		return 0
	}
	return d
}

func (m *modeloEspelho) reservar(qtd int) bool {
	if m.disponivel() < qtd {
		return false
	}
	m.pendentes += qtd
	return true
}

// aplicarWebhook escreve o número do Tiny sem julgar: ele É a verdade. O nosso
// movimento já está embutido nele, então o pendente correspondente sai.
func (m *modeloEspelho) aplicarWebhook(saldo int) {
	anterior := m.espelho
	m.espelho = saldo
	if consumido := anterior - saldo; consumido > 0 {
		m.pendentes -= consumido
		if m.pendentes < 0 {
			m.pendentes = 0
		}
	}
}

// -----------------------------------------------------------------------------

// O caso central: live vendendo enquanto o marketplace também vende.
func TestVendaEmOutroCanalDuranteALive(t *testing.T) {
	const inicial = 5

	tiny := &tinySimulado{saldo: inicial}
	atual := &modeloAtual{local: inicial}
	espelho := &modeloEspelho{espelho: inicial}

	// Comprador na live pede 2. Reservamos e avisamos o Tiny.
	if !atual.reservar(2) || !espelho.reservar(2) {
		t.Fatal("a primeira reserva de 2 sobre 5 tem de passar nos dois modelos")
	}
	tiny.saida(2) // Tiny: 5 -> 3

	// O webhook desse movimento chega.
	tiny.entregarPendentes(func(s int) {
		atual.aplicarWebhook(s)
		espelho.aplicarWebhook(s)
	})

	// AGORA o lojista vende 1 no Mercado Livre. Nós não temos como saber por
	// nenhum caminho que não seja este webhook.
	tiny.vendaExterna(1) // Tiny: 3 -> 2
	tiny.entregarPendentes(func(s int) {
		atual.aplicarWebhook(s)
		espelho.aplicarWebhook(s)
	})

	if tiny.saldo != 2 {
		t.Fatalf("saldo no Tiny = %d, quero 2", tiny.saldo)
	}

	// O modelo do espelho enxerga as 2 unidades reais.
	if got := espelho.disponivel(); got != 2 {
		t.Errorf("MODELO ESPELHO: disponível = %d, quero 2 — o Tiny é a fonte da "+
			"verdade e já contou a venda do marketplace", got)
	}

	// O modelo atual ignorou o webhook enquanto havia reserva viva, e por isso
	// ainda acha que tem 3 para vender. É oversell esperando comprador.
	if got := atual.disponivel(); got == 2 {
		t.Log("modelo atual acertou por acaso neste arranjo")
	} else {
		t.Logf("MODELO ATUAL: disponível = %d contra %d reais no Tiny — a venda "+
			"externa foi ignorada porque havia reserva viva", got, tiny.saldo)
	}
}

// Reposição do lojista pelo lado dele tem de aparecer para a live.
func TestReposicaoDoLojistaAparecerNaLive(t *testing.T) {
	tiny := &tinySimulado{saldo: 0}
	espelho := &modeloEspelho{espelho: 0}

	if espelho.reservar(1) {
		t.Fatal("reservou com estoque zerado")
	}

	tiny.reposicaoDoLojista(50) // o lojista repõe no Tiny
	tiny.entregarPendentes(espelho.aplicarWebhook)

	if got := espelho.disponivel(); got != 50 {
		t.Errorf("disponível = %d, quero 50 — reposição no ERP tem de chegar até "+
			"a live sem ninguém mexer no LiveCart", got)
	}
	if !espelho.reservar(50) {
		t.Error("não conseguiu reservar as 50 unidades recém-repostas")
	}
}

// A janela do webhook: dois compradores dentro do atraso não podem vender a
// mesma unidade. É o que o contador de pendentes existe para impedir.
func TestDoisCompradoresDentroDaJanelaDoWebhook(t *testing.T) {
	tiny := &tinySimulado{saldo: 1}
	espelho := &modeloEspelho{espelho: 1}

	if !espelho.reservar(1) {
		t.Fatal("primeira reserva de 1 sobre 1 tem de passar")
	}
	tiny.saida(1) // Tiny: 1 -> 0, mas o webhook ainda não chegou

	// Segundo comprador, ANTES do webhook. O espelho ainda diz 1.
	if espelho.reservar(1) {
		t.Error("vendeu a mesma unidade duas vezes dentro da janela do webhook — " +
			"é para isso que o pendente desconta")
	}

	// Webhook chega e confirma.
	tiny.entregarPendentes(espelho.aplicarWebhook)
	if got := espelho.disponivel(); got != 0 {
		t.Errorf("depois do webhook, disponível = %d, quero 0", got)
	}
	if espelho.pendentes != 0 {
		t.Errorf("pendentes = %d, quero 0 — o movimento já voltou pelo espelho, "+
			"contá-lo de novo travaria estoque que existe", espelho.pendentes)
	}
}

// Atrasos diferentes, mesma sequência de eventos: o resultado tem de ser o
// mesmo. Se o desenho depende da ORDEM de chegada dos webhooks, ele é frágil —
// e em produção os atrasos variaram de 1s a 50s no mesmo dia.
func TestResultadoNaoDependeDoAtrasoDoWebhook(t *testing.T) {
	for _, loteDeWebhooks := range []int{1, 2, 4} {
		t.Run(fmt.Sprintf("entrega_a_cada_%d_movimentos", loteDeWebhooks), func(t *testing.T) {
			const inicial = 10
			tiny := &tinySimulado{saldo: inicial}
			espelho := &modeloEspelho{espelho: inicial}

			vendasNaLive := 0
			movimentos := 0
			flush := func() {
				tiny.entregarPendentes(espelho.aplicarWebhook)
			}

			// Alterna venda na live e venda no marketplace.
			for i := 0; i < 4; i++ {
				if espelho.reservar(1) {
					tiny.saida(1)
					vendasNaLive++
				}
				movimentos++
				if movimentos%loteDeWebhooks == 0 {
					flush()
				}

				tiny.vendaExterna(1)
				movimentos++
				if movimentos%loteDeWebhooks == 0 {
					flush()
				}
			}
			flush()

			// O espelho tem de convergir para o saldo real do Tiny, qualquer que
			// tenha sido o agrupamento dos webhooks.
			if espelho.espelho != tiny.saldo {
				t.Errorf("espelho = %d, Tiny = %d — o desenho não pode depender de "+
					"como os webhooks foram agrupados", espelho.espelho, tiny.saldo)
			}
			// E a conta fecha: inicial = vendido na live + vendido fora + o que sobrou.
			if soma := vendasNaLive + 4 + tiny.saldo; soma != inicial {
				t.Errorf("live %d + externo 4 + resto %d = %d, quero %d",
					vendasNaLive, tiny.saldo, soma, inicial)
			}
		})
	}
}

// Nunca vender a descoberto, mesmo com o Tiny aceitando negativo. O porteiro
// tem de ser nosso, porque o ERP não recusa.
func TestNuncaVendeAlemDoQueOTinyTem(t *testing.T) {
	const inicial = 3
	tiny := &tinySimulado{saldo: inicial}
	espelho := &modeloEspelho{espelho: inicial}

	vendidas := 0
	for i := 0; i < 10; i++ {
		if espelho.reservar(1) {
			tiny.saida(1)
			vendidas++
		}
		// Webhook chega a cada duas tentativas, simulando atraso.
		if i%2 == 1 {
			tiny.entregarPendentes(espelho.aplicarWebhook)
		}
	}
	tiny.entregarPendentes(espelho.aplicarWebhook)

	if vendidas > inicial {
		t.Errorf("vendeu %d de um estoque de %d — o Tiny aceita saldo negativo sem "+
			"reclamar, então quem tem de barrar somos nós", vendidas, inicial)
	}
	if tiny.saldo < 0 {
		t.Errorf("saldo do Tiny em %d: deixamos vender a descoberto", tiny.saldo)
	}
}
