package integration

import (
	"sync/atomic"
	"time"
)

// A regra de como o SALDO ABSOLUTO do ERP entra no nosso contador local.
//
// Os dois lados falam línguas diferentes, e é daí que nasce toda esta classe de
// bug: nós mandamos DELTAS ("saiu 1", "entrou 2") e o Tiny devolve, no webhook,
// o SALDO. Enquanto um movimento nosso está em voo, o saldo que ele nos manda é
// uma foto do meio do caminho — ele está atrás de nós, por nossa causa. Aplicar
// essa foto por cima do local é copiar um número que já nasceu velho.
//
// Três modos, e a diferença entre eles é a DIREÇÃO que cada um deixa passar:
//
//	normal          aplica o saldo do ERP como veio
//	downgrade-only  aplica só se for MENOR que o local
//	suprimido       não toca no estoque
//
// A direção importa porque os danos são assimétricos. Estoque local a MENOS
// segura venda que existia — chato, recuperável, e o lojista vê. Estoque local
// a MAIS oferece unidade que não existe: vira promoção fantasma da fila, venda
// confirmada de produto esgotado e pedido que não tem como ser atendido.
//
// Por isso todo caminho de dúvida — inclusive erro de banco ao consultar os
// guards — cai para o lado de preservar o local.

// stockSyncMode decide como aplicar o estoque vindo do ERP.
//
// guarded: existe reserva ativa numa live ou finalização em voo para o produto.
// guardErr: a consulta do guard falhou.
// pendingReversal: existe estorno de carrinho em voo para o produto.
// pendErr: a consulta do estorno falhou.
func stockSyncMode(guarded, guardErr, pendingReversal, pendErr bool) (skipStock, downgradeOnly bool) {
	// Erro de consulta suprime tudo. Sem saber se há movimento nosso em voo,
	// aplicar o saldo do ERP é apostar — e o lado ruim da aposta cria unidade.
	if guardErr || pendErr {
		return true, false
	}

	// Estorno em voo é MAIS FORTE que o guard, e tem de ser avaliado depois
	// dele justamente por isso: downgrade-only deixa passar "ERP menor que o
	// local", que é exatamente a foto do meio do estorno.
	//
	// Cancelar um carrinho credita o local numa transação só; o estorno no ERP
	// sai um a um por HTTP, e cada um dispara um webhook. Nessa janela o Tiny
	// está atrás de nós, e "ERP menor" não é redução do lojista — é a nossa
	// própria operação pela metade. Foi o que derrubou dois produtos de 5 para
	// 4 em staging.
	if pendingReversal {
		return true, false
	}

	// Reserva ATIVA: o saldo do ERP não é fonte da verdade para o nosso contador,
	// em direção nenhuma.
	//
	// Aqui valia downgrade-only, com o argumento de que redução do lojista no
	// Tiny durante a live é legítima e deve refletir. O argumento ignora que,
	// enquanto seguramos a peça, quem mais mexe naquele saldo somos NÓS: cada
	// reserva é uma saída, cada ajuste de checkout é outro movimento, e cada um
	// volta como webhook com o valor absoluto já deflacionado. "ERP menor que o
	// local" durante um hold é, quase sempre, o eco da nossa própria operação —
	// e não dá para distinguir do lojista mexendo.
	//
	// Em 12/08/2026, às 17:21:25.869: `local_stock=1 erp_stock=0 new_stock=0
	// downgrade_only=true`. Aquele zero era do movimento que nós mesmos
	// tínhamos mandado 0,6 segundo antes. O contador local foi a zero e nunca
	// mais reconciliou, porque os webhooks seguintes vieram com skip_stock.
	//
	// Suprimir custa atraso: uma redução real do lojista durante a live só
	// reflete quando os holds saírem. É um atraso de minutos, contra um
	// contador corrompido que não se recupera sozinho — e a query do guard
	// (HasStockGuardForProduct) já afirma exatamente isto no comentário dela.
	if guarded {
		return true, false
	}

	return false, false
}

// erpMovementEchoGrace é a folga depois que a chamada ao ERP volta.
//
// A contagem de movimentos em voo cobre o essencial — a janela entre baixarmos
// o estoque local e o ERP registrar a saída. Esta folga cobre o resto do
// caminho: o ERP processa, gera o webhook e ele chega até nós. Medido em
// produção, esse trecho leva de 1 a 3 segundos.
//
// Curta de propósito. Enquanto ela vale, uma venda do lojista em OUTRO canal
// não é aplicada — e é exatamente isso que não se quer prolongar. Antes disto
// aqui a supressão durava enquanto houvesse reserva viva (trinta minutos) e
// depois virou uma janela fixa de sessenta segundos; nos dois casos o LiveCart
// ficava cego para o Mercado Livre por tempo demais.
const erpMovementEchoGrace = 5 * time.Second

// NoteERPMovementStarted marca que uma chamada de estoque ao ERP COMEÇOU.
//
// Entre baixarmos o estoque local e o ERP registrar a saída, o saldo dele ainda
// é o de antes — maior que o nosso. Um webhook nessa fresta, aplicado ao pé da
// letra, devolveria o local ao valor antigo e apagaria a reserva do comprador.
//
// Contar em vez de cronometrar: a fresta dura o que a chamada durar, e não um
// prazo fixo que alguém chutou.
func (s *Service) NoteERPMovementStarted(externalProductID string) {
	if externalProductID == "" {
		return
	}
	v, _ := s.erpMovementInFlight.LoadOrStore(externalProductID, new(int64))
	atomic.AddInt64(v.(*int64), 1)
}

// NoteERPMovementSent marca que a chamada TERMINOU (com sucesso ou não) e
// carimba o instante, para a folga do webhook correr a partir daí.
func (s *Service) NoteERPMovementSent(externalProductID string) {
	if externalProductID == "" {
		return
	}
	if v, ok := s.erpMovementInFlight.Load(externalProductID); ok {
		if n := atomic.AddInt64(v.(*int64), -1); n < 0 {
			atomic.StoreInt64(v.(*int64), 0)
		}
	}
	s.erpMovementSentAt.Store(externalProductID, time.Now())
}

// erpMovementEchoing responde se um movimento NOSSO ainda pode estar em
// trânsito, num dos dois sentidos: a chamada ao ERP não voltou, ou voltou há
// tão pouco tempo que o webhook dela ainda não chegou.
//
// Fora disso, o saldo do ERP é notícia legítima — venda em outro canal,
// reposição do lojista — e tem de ser aplicado. Ignorá-lo é ficar com estoque
// errado, que foi o defeito que este mecanismo existe para não recriar.
func (s *Service) erpMovementEchoing(externalProductID string) bool {
	if v, ok := s.erpMovementInFlight.Load(externalProductID); ok {
		if atomic.LoadInt64(v.(*int64)) > 0 {
			return true
		}
	}
	v, ok := s.erpMovementSentAt.Load(externalProductID)
	if !ok {
		return false
	}
	sentAt, ok := v.(time.Time)
	if !ok {
		return false
	}
	return time.Since(sentAt) < erpMovementEchoGrace
}

