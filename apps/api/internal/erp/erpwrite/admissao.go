package erpwrite

// A regra de admissão, e por que ela não pode ser o saldo do ERP.
//
// Medido em 26/08/2026 contra o ambiente real: um produto com 20 unidades no
// Tiny recebeu 25 comentários numa live enquanto um canal externo vendia 8 do
// mesmo estoque. O resultado foi 25 admissões e o saldo do Tiny em -13.
//
// O portão local em si estava CORRETO — o log mostra a leitura caindo
// monotonicamente de 41 até 0 e parando. O defeito é que ele PARTIU de 41
// enquanto o ERP tinha 20, porque o espelho de estoque faz um SET ABSOLUTO com
// o saldo lido do ERP:
//
//	UPDATE products SET stock = <saldo do ERP> WHERE id = $1 AND erp_seq = $2
//
// Esse saldo é verdadeiro para o ERP e MENTIROSO para nós, por duas razões que
// se somam: ele não desconta as reservas que ainda estão em voo, e ele pode
// SUBIR o portão que a live já tinha baixado. O `erp_seq` protege a janela
// entre a leitura e a escrita, não a vida inteira da live.
//
// A regra correta é a do briefing, e ela é conservadora por construção:
//
//	admissível = saldo conhecido do ERP − reservas em voo
//
// Erra para menos, nunca para mais. Uma unidade não vendida é recuperável; uma
// unidade vendida duas vezes vira o problema do lojista com a compradora.

// Admissivel calcula quantas unidades podem ser prometidas.
//
// emVoo são as unidades que já prometemos e cujo efeito o ERP ainda não
// refletiu — reservas despachadas sem confirmação, e admissões locais ainda não
// espelhadas. Subtraí-las é o que impede o espelho de reabastecer o portão.
func Admissivel(saldoERP, emVoo int) int {
	n := saldoERP - emVoo
	if n < 0 {
		return 0
	}
	return n
}

// NovoSaldoDoPortao decide o valor do portão local quando o espelho traz uma
// leitura nova do ERP.
//
// A propriedade que ela garante — e que faltava — é: **o espelho nunca SOBE o
// portão acima do admissível**. Pode baixar à vontade (o ERP mandou), mas subir
// só até o que sobra depois de descontar o que está em voo.
func NovoSaldoDoPortao(portaoAtual, saldoERP, emVoo int) int {
	// O valor ATUAL do portão não participa da conta, e isso é deliberado.
	//
	// A primeira versão desta função ramificava em "baixar" e "subir" como se
	// fossem decisões diferentes, e os dois ramos devolviam a mesma coisa — uma
	// decisão de mentira. O admissível já é a resposta completa: ele desconta o
	// que está em voo, então subir até ele nunca promete duas vezes, e descer
	// até ele é obrigatório quando o ERP diz que há menos.
	//
	// O parâmetro fica na assinatura porque o chamador precisa dele para logar
	// o delta e para PodeSubir explicar o motivo.
	_ = portaoAtual
	return Admissivel(saldoERP, emVoo)
}

// PodeSubir diz se uma leitura do ERP autoriza aumentar o portão. Serve de
// guarda explícita nos call sites do espelho, para o motivo ficar legível.
func PodeSubir(portaoAtual, saldoERP, emVoo int) bool {
	return Admissivel(saldoERP, emVoo) > portaoAtual
}

// Nota sobre reserva NATIVA do ERP (Bling com "Considerar situações de vendas
// para obter o saldo atual" ligada), e sobre um erro que custou caro.
//
// Chegou a existir aqui um AdmissivelPorModo, para não subtrair duas vezes
// quando o saldo do ERP já desconta os nossos pedidos. Ele foi removido com o
// argumento de que a distinção não é desta função e sim de QUEM CONTA o
// `emVoo`. O argumento continua certo — e a implementação que o acompanhou
// estava errada.
//
// SumPromisedNotYetReflected decidia pelo RELÓGIO: um carrinho com pedido
// criado há menos de 45 s continuava contando, para cobrir o atraso entre o
// pedido existir e o ERP refleti-lo. Numa live TODO carrinho está dentro dos
// 45 s, então TODA reserva era descontada duas vezes — uma pelo ERP, outra por
// nós. Medido em staging em 31/08/2026: o Bling dizia 3 disponíveis e o
// LiveCart gravava 1.
//
// Hoje a query recebe o modo (`conta_com_pedido`) e responde à pergunta certa:
// "este saldo lido já desconta esta unidade?". Reserva nativa e carrinho com
// pedido: já. Qualquer outro caso: não.
//
// A lição, para a próxima vez que alguém for mexer aqui: uma janela de tempo
// parece conservadora e não é. Ela troca uma pergunta respondível — quem já
// está no saldo — por um palpite sobre latência, e o palpite erra o tempo todo
// justamente quando o sistema está sob carga.
