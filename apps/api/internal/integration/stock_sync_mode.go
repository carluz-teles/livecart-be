package integration

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

	// Reserva ativa numa live: reduções do lojista no Tiny durante a
	// transmissão são legítimas e devem refletir. O que não pode é SUBIR, que é
	// a direção capaz de inventar oferta.
	if guarded {
		return false, true
	}

	return false, false
}
