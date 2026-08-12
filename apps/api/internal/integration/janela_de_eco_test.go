package integration

// A janela de eco: separar "o Tiny está me contando algo novo" de "o Tiny está
// repetindo o que eu acabei de fazer".
//
// O webhook do Tiny traz sempre o saldo ABSOLUTO, e os dois casos chegam
// idênticos — um número menor que o nosso. Sem distinguir, sobram dois extremos
// ruins: aplicar sempre, e o eco do nosso próprio movimento corrompe o contador
// (foi o Gabinete Gamer indo a zero em 12/08); ou nunca aplicar enquanto houver
// reserva viva, e ficar cego para os outros canais do lojista.
//
// A supressão por reserva durava o carrinho inteiro — trinta minutos. O eco
// dura segundos: 1 a 3 no caminho normal, até ~50 quando o estorno entra em
// retentativa. Carimbar o instante do envio troca meia hora de cegueira por um
// minuto.
//
// Importa para o cliente que vende o mesmo SKU em vários canais: durante uma
// live de uma hora, o LiveCart ignorava tudo que acontecia no Mercado Livre.

import (
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
			"e um saldo do Tiny aqui é notícia legítima de outro canal")
	}
}

func TestLogoAposNossoMovimentoOSaldoEhSuspeito(t *testing.T) {
	s := servicoComEco(t)

	s.NoteERPMovementSent("EXT-1")

	if !s.erpMovementEchoing("EXT-1") {
		t.Error("acabamos de mandar um movimento — o próximo saldo do Tiny pode ser " +
			"o eco dele, e aplicá-lo por cima corrompe o contador")
	}
	// Produto diferente não é afetado: o carimbo é por SKU.
	if s.erpMovementEchoing("EXT-2") {
		t.Error("o carimbo vazou para outro produto — cada SKU tem a sua janela")
	}
}

func TestAJanelaExpiraEOSaldoVoltaAValer(t *testing.T) {
	s := servicoComEco(t)

	// Carimbo antigo: mais velho que a janela.
	s.erpMovementSentAt.Store("EXT-1", time.Now().Add(-erpMovementEchoWindow-time.Second))

	if s.erpMovementEchoing("EXT-1") {
		t.Error("a janela não expirou — se ela não expirar, a cegueira volta a ser " +
			"permanente e o multi-canal nunca sincroniza")
	}
}

// A janela precisa cobrir o pior caso medido em produção, e não pode ser tão
// longa a ponto de recriar a cegueira que ela veio resolver.
func TestJanelaCobreOPiorCasoMedidoSemExagerar(t *testing.T) {
	const piorRetentativaMedida = 50 * time.Second
	if erpMovementEchoWindow < piorRetentativaMedida {
		t.Errorf("janela de %v não cobre a retentativa de %v medida em 12/08 — "+
			"um eco atrasado seria lido como venda em outro canal e derrubaria o "+
			"contador", erpMovementEchoWindow, piorRetentativaMedida)
	}
	// A reserva vive ~30 minutos. Se a janela chegasse perto disso, não teríamos
	// ganho nada em relação à supressão anterior.
	if erpMovementEchoWindow > 5*time.Minute {
		t.Errorf("janela de %v é longa demais — o ponto da mudança é justamente "+
			"deixar de ignorar os outros canais do lojista durante a live",
			erpMovementEchoWindow)
	}
}

// O modo de sincronização passa a ser decidido pelo eco, não pela reserva. Com
// reserva viva mas nenhum movimento nosso recente, o saldo do Tiny vale.
func TestModoDeSyncSegueOEcoENaoAReserva(t *testing.T) {
	// Sem eco: aplica normalmente, e é assim que a venda no marketplace chega.
	skip, downgrade := stockSyncMode(false, false, false, false)
	if skip || downgrade {
		t.Errorf("sem movimento nosso ecoando, o saldo do Tiny tem de ser aplicado "+
			"(skip=%v downgrade=%v)", skip, downgrade)
	}

	// Com eco: suprime, porque o número pode ser o nosso próprio movimento.
	skip, downgrade = stockSyncMode(true, false, false, false)
	if !skip {
		t.Error("com movimento nosso ecoando, aplicar o saldo é copiar o próprio " +
			"rastro por cima do contador")
	}
	if downgrade {
		t.Error("downgrade-only deixa passar justamente a direção do eco")
	}
}
