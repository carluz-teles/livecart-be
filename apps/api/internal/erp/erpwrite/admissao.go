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
	teto := Admissivel(saldoERP, emVoo)
	if portaoAtual > teto {
		// O ERP diz que há menos do que o portão acredita: baixar é obrigatório.
		return teto
	}
	if portaoAtual < teto {
		// O ERP diz que há mais. Subir é permitido — foi reposição de verdade —
		// mas só até o teto conservador, nunca até o saldo cru.
		return teto
	}
	return portaoAtual
}

// PodeSubir diz se uma leitura do ERP autoriza aumentar o portão. Serve de
// guarda explícita nos call sites do espelho, para o motivo ficar legível.
func PodeSubir(portaoAtual, saldoERP, emVoo int) bool {
	return Admissivel(saldoERP, emVoo) > portaoAtual
}
