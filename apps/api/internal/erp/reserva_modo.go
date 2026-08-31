package erp

import "fmt"

// ModoDeReserva diz QUEM segura a peça entre o comentário e o pagamento.
//
// A pergunta existe porque os dois ERPs resolvem isso de formas diferentes, e
// porque no Bling a capacidade depende de uma configuração da CONTA do lojista
// que o LiveCart não consegue ler nem ligar pela API (varredura nos 162 paths:
// os únicos endpoints de configuração são GET|PUT /nfse/configuracoes).
type ModoDeReserva string

const (
	// ReservaNativaDoERP — o ERP reserva sozinho, e o LiveCart não escreve
	// movimento de estoque nenhum.
	//
	// No Bling a reserva é efeito colateral da SITUAÇÃO do pedido, quando a conta
	// tem ligada a opção "Considerar situações de vendas para obter o saldo
	// atual (Reserva de estoque)". Doc oficial: "Uma venda recém-criada tem a
	// situação de origem 'Em aberto' e, mesmo que ainda não tenha seu estoque
	// lançado, o item pode ser retirado do saldo de estoque e considerado como
	// 'Reservado'."
	//
	// É o modo RECOMENDADO, e resolve o problema que motivou tudo isto: como não
	// emitimos movimento de estoque, não existe estorno para receber de volta, e
	// some a classe de bug em que um webhook de estorno reabria a fila de um
	// produto que já tinha dono.
	ReservaNativaDoERP ModoDeReserva = "nativa"

	// ReservaSomenteLocal — o contador do LiveCart segura a peça sozinho, e o
	// pedido só nasce no ERP quando o pagamento entra.
	//
	// É o modo SEGURO POR PADRÃO: funciona em qualquer conta, ligada ou não a
	// reserva. O preço está escrito em PrecoParaOLojista.
	ReservaSomenteLocal ModoDeReserva = "local"
)

// ModoDeReservaPadrao é o padrão genérico: local.
//
// Local, e não nativa, porque a nativa depende de uma configuração da conta que
// não conseguimos VERIFICAR por API. Assumir que está ligada quando não está
// significa o LiveCart achar que o ERP está segurando a peça enquanto ninguém
// está — e a peça é vendida duas vezes. O default erra para o lado recuperável.
const ModoDeReservaPadrao = ReservaSomenteLocal

// ModoDeReservaPadraoDoProvider é o padrão de um ERP específico.
//
// O Tiny é NATIVO por definição e não por escolha: lá o pedido de venda É a
// reserva (módulo `possuiReserva`), e é isso que o LiveCart faz desde sempre —
// cria o pedido no primeiro comentário e deixa o ERP segurar a peça. Aplicar o
// padrão genérico a ele mudaria o comportamento de quem fatura hoje, que é
// exatamente o que esta função existe para impedir.
//
// O Bling cai no padrão genérico: lá a reserva depende de uma configuração da
// conta que só a sonda confirma.
func ModoDeReservaPadraoDoProvider(provider string) ModoDeReserva {
	if provider == "tiny" {
		return ReservaNativaDoERP
	}
	return ModoDeReservaPadrao
}

// ModoDeReservaDaIntegracao lê o modo escolhido, caindo no padrão DO PROVIDER.
//
// É deliberadamente a ÚNICA forma de ler o modo. Existiu aqui um
// ModoDeReservaDoMetadata que não recebia o provider, e ele devolvia "local"
// para o Tiny — o que faria a tela dizer que o pedido nasce no pagamento
// enquanto o fluxo o cria no comentário. Pior: o lojista que salvasse o que a
// tela mostra gravaria "local" e o Tiny pararia de reservar na live.
func ModoDeReservaDaIntegracao(provider string, metadata map[string]any) ModoDeReserva {
	if metadata != nil {
		if v, _ := metadata[ChaveModoDeReserva].(string); v != "" {
			// Metadata malformado NÃO liga um comportamento que muda o que a
			// loja vende. Mesma regra do MetadataBool.
			if m := ModoDeReserva(v); m.Valido() {
				return m
			}
		}
	}
	return ModoDeReservaPadraoDoProvider(provider)
}

// ChaveModoDeReserva é onde o modo vive no metadata da integração.
const ChaveModoDeReserva = "modo_reserva_estoque"

func (m ModoDeReserva) Valido() bool {
	return m == ReservaNativaDoERP || m == ReservaSomenteLocal
}

// PrecoParaOLojista é o texto que a tela mostra. Fica no domínio de propósito:
// a consequência de escolher errado é de NEGÓCIO, e um lojista que não entende
// o preço não escolhe — ele adivinha.
func (m ModoDeReserva) PrecoParaOLojista() string {
	switch m {
	case ReservaNativaDoERP:
		return "O pedido nasce no ERP assim que a compradora comenta, e o ERP tira a peça " +
			"do saldo na hora. Seus outros canais de venda param de oferecer a mesma peça " +
			"imediatamente. Exige a Reserva de estoque LIGADA na sua conta do ERP."
	case ReservaSomenteLocal:
		return "O LiveCart segura a peça e o pedido só vai para o ERP quando o pagamento " +
			"entra. Nunca escrevemos no seu estoque. Em troca, se você vende a mesma peça " +
			"em outro canal ligado ao mesmo ERP, esse canal não enxerga a venda da live até " +
			"o pagamento."
	default:
		return ""
	}
}

// ═══════════════════════════════════════════════════════════════════════════
// A CAPACIDADE DA CONTA — observada, não perguntada
// ═══════════════════════════════════════════════════════════════════════════
//
// Se o ERP reserva ou não é configuração do LOJISTA, no ERP dele. O LiveCart
// não liga, não desliga e não deveria fingir que decide. O que ele pode fazer é
// SABER — e saber sem perguntar nada, porque a resposta chega sozinha em todo
// webhook de estoque:
//
//	disponível < físico  ⇒  alguma coisa está reservada  ⇒  a conta RESERVA
//
// É prova positiva e de graça. Não gasta requisição, não depende de haver
// pedido em aberto no instante em que o lojista abre a tela, e não expira: uma
// conta que reservou uma vez tem o módulo ligado.
//
// O caminho contrário — "não vi reserva, logo não reserva" — NÃO é prova. Pode
// não haver pedido aberto agora; e o saldo demora de 9 a 22 segundos para
// refletir um pedido recém-criado (medido em 29/08/2026). Por isso a ausência
// de sinal nunca derruba uma confirmação.

// ChaveCapacidadeDeReserva é onde o veredito vive no metadata da integração.
const ChaveCapacidadeDeReserva = "reserva_capacidade_confirmada"

// CapacidadeConfirmada diz se JÁ foi observado que esta conta reserva.
//
// O Tiny não passa por aqui: lá o pedido de venda É a reserva, por desenho do
// produto, e o que o lojista precisa conferir no painel do Tiny nós já dizemos
// a ele. Exigir prova para o Tiny quebraria quem fatura hoje.
func CapacidadeConfirmada(provider string, metadata map[string]any) bool {
	if provider == "tiny" {
		return true
	}
	if metadata == nil {
		return false
	}
	v, _ := metadata[ChaveCapacidadeDeReserva].(bool)
	return v
}

// ObservacaoDeReserva é o que uma leitura de saldo diz sobre a conta.
type ObservacaoDeReserva int

const (
	// NadaAObservar: a leitura não decide nada. É o caso mais comum e não é
	// notícia — sem pedido em aberto, disponível == físico é o esperado.
	NadaAObservar ObservacaoDeReserva = iota
	// ContaReserva: prova positiva. O ERP está segurando peça.
	ContaReserva
)

// ObservarCapacidade lê um saldo e diz o que ele prova.
//
// Deliberadamente só sabe dizer "sim" ou "não sei". Um "não" precisaria
// distinguir "a conta não reserva" de "não há nada reservado agora" e de "o ERP
// ainda não refletiu o pedido de 5 segundos atrás" — e errar nessa distinção
// derrubaria um lojista do modo nativo no meio de uma live.
func ObservarCapacidade(fisico, disponivel int) ObservacaoDeReserva {
	if fisico > 0 && disponivel < fisico {
		return ContaReserva
	}
	return NadaAObservar
}

// ResultadoDaSonda é o que a sonda de capacidade descobriu sobre a conta.
type ResultadoDaSonda struct {
	// ReservaLigada diz se a conta reserva de verdade. Só é conclusivo quando
	// Conclusiva é true.
	ReservaLigada bool
	// Conclusiva é falsa quando não deu para decidir — por exemplo quando não há
	// pedido em aberto para observar. Um "não sei" honesto vale mais do que um
	// "não reserva" que faria o lojista escolher o modo errado.
	Conclusiva bool
	Explicacao string
}

// SondaDeReserva descobre EMPIRICAMENTE se a conta do ERP reserva estoque.
//
// Existe porque a configuração não é legível por API. O sinal é a diferença
// entre o saldo físico e o disponível de um produto que tem pedido em aberto:
// se a conta reserva, o disponível é menor.
//
// Deliberadamente NÃO escreve nada. Uma sonda que criasse um pedido para
// descobrir a resposta mexeria no ERP real do lojista sem ele pedir — e o Bling
// não tem sandbox.
type SondaDeReserva struct {
	// SaldoFisico e SaldoDisponivel de um produto que TEM pedido em aberto.
	SaldoFisico     int
	SaldoDisponivel int
	// UnidadesEmPedidoAberto é quanto os pedidos em aberto daquele produto somam.
	UnidadesEmPedidoAberto int
}

func (s SondaDeReserva) Avaliar() ResultadoDaSonda {
	if s.UnidadesEmPedidoAberto <= 0 {
		return ResultadoDaSonda{
			Conclusiva: false,
			Explicacao: "nenhum pedido em aberto para observar — sem reserva pendente, " +
				"os dois saldos coincidem com ou sem a configuração ligada",
		}
	}
	reservado := s.SaldoFisico - s.SaldoDisponivel
	if reservado >= s.UnidadesEmPedidoAberto {
		return ResultadoDaSonda{
			ReservaLigada: true, Conclusiva: true,
			Explicacao: fmt.Sprintf("o disponível está %d unidade(s) abaixo do físico, "+
				"cobrindo as %d em pedido aberto — a conta reserva",
				reservado, s.UnidadesEmPedidoAberto),
		}
	}
	if reservado == 0 {
		return ResultadoDaSonda{
			ReservaLigada: false, Conclusiva: true,
			Explicacao: fmt.Sprintf("há %d unidade(s) em pedido aberto e o disponível é "+
				"IGUAL ao físico — a conta NÃO reserva", s.UnidadesEmPedidoAberto),
		}
	}
	// Reservou algo, mas menos do que os pedidos em aberto. Pode ser situação
	// fora da lista configurada, pode ser pedido já faturado. Não afirmar.
	return ResultadoDaSonda{
		Conclusiva: false,
		Explicacao: fmt.Sprintf("o disponível está %d abaixo do físico mas há %d em pedido "+
			"aberto — reserva parcial, provavelmente porque só ALGUMAS situações reservam",
			reservado, s.UnidadesEmPedidoAberto),
	}
}

// EscolherModo devolve o modo que o LiveCart deve usar, e o motivo.
//
// A regra dura: o modo NATIVO só vale se a sonda CONFIRMOU que a conta reserva.
// Escolher nativo com a conta sem reserva é o LiveCart acreditar que o ERP está
// segurando a peça enquanto ninguém está — e a peça é vendida duas vezes.
func EscolherModo(escolhido ModoDeReserva, sonda ResultadoDaSonda) (ModoDeReserva, string) {
	if escolhido != ReservaNativaDoERP {
		return ReservaSomenteLocal, "modo local (padrão seguro)"
	}
	if !sonda.Conclusiva {
		return ReservaSomenteLocal, "modo nativo pedido, mas a sonda não conseguiu confirmar " +
			"que a conta reserva (" + sonda.Explicacao + ") — caindo no local para não vender duas vezes"
	}
	if !sonda.ReservaLigada {
		return ReservaSomenteLocal, "modo nativo pedido, mas a conta NÃO reserva (" +
			sonda.Explicacao + ") — ligue a Reserva de estoque no ERP e tente de novo"
	}
	return ReservaNativaDoERP, "modo nativo confirmado (" + sonda.Explicacao + ")"
}
