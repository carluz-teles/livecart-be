package erp

import "testing"

// O padrão é LOCAL, e isso não é conservadorismo gratuito.
//
// A reserva nativa do Bling depende de uma configuração da CONTA que o LiveCart
// não consegue ler por API. Assumir que está ligada quando não está é o
// LiveCart acreditar que o ERP segura a peça enquanto ninguém segura — e a peça
// é vendida duas vezes. O default erra para o lado recuperável.
func TestPadraoEhLocalMesmoComMetadataAusenteOuLixo(t *testing.T) {
	casos := []struct {
		nome     string
		metadata map[string]any
	}{
		{"nil", nil},
		{"vazio", map[string]any{}},
		{"chave ausente", map[string]any{"outra": "coisa"}},
		{"valor inválido", map[string]any{ChaveModoDeReserva: "turbo"}},
		{"tipo errado", map[string]any{ChaveModoDeReserva: 42}},
		{"string vazia", map[string]any{ChaveModoDeReserva: ""}},
	}
	for _, c := range casos {
		t.Run(c.nome, func(t *testing.T) {
			if got := ModoDeReservaDaIntegracao("bling", c.metadata); got != ReservaSomenteLocal {
				t.Errorf("modo = %q, queria %q — metadata malformado NÃO pode ligar "+
					"um comportamento que muda o que a loja vende", got, ReservaSomenteLocal)
			}
		})
	}
}

func TestModoValidoEhLidoDoMetadata(t *testing.T) {
	for _, m := range []ModoDeReserva{ReservaNativaDoERP, ReservaSomenteLocal} {
		got := ModoDeReservaDaIntegracao("bling", map[string]any{ChaveModoDeReserva: string(m)})
		if got != m {
			t.Errorf("modo = %q, queria %q", got, m)
		}
	}
}

// A sonda tem de distinguir TRÊS estados, não dois. "Não sei" é uma resposta
// legítima, e tratá-la como "não reserva" faria o lojista escolher o modo errado.
func TestSondaDistingueNaoSeiDeNaoReserva(t *testing.T) {
	casos := []struct {
		nome           string
		sonda          SondaDeReserva
		querConclusiva bool
		querLigada     bool
	}{
		{
			nome:           "sem pedido em aberto — INCONCLUSIVO",
			sonda:          SondaDeReserva{SaldoFisico: 5, SaldoDisponivel: 5, UnidadesEmPedidoAberto: 0},
			querConclusiva: false,
		},
		{
			nome:           "2 em aberto e disponível igual ao físico — NÃO reserva",
			sonda:          SondaDeReserva{SaldoFisico: 5, SaldoDisponivel: 5, UnidadesEmPedidoAberto: 2},
			querConclusiva: true, querLigada: false,
		},
		{
			nome:           "2 em aberto e disponível 2 abaixo — RESERVA",
			sonda:          SondaDeReserva{SaldoFisico: 5, SaldoDisponivel: 3, UnidadesEmPedidoAberto: 2},
			querConclusiva: true, querLigada: true,
		},
		{
			nome:           "reserva parcial — INCONCLUSIVO (só algumas situações reservam)",
			sonda:          SondaDeReserva{SaldoFisico: 5, SaldoDisponivel: 4, UnidadesEmPedidoAberto: 3},
			querConclusiva: false,
		},
		{
			nome:           "reserva maior que o esperado ainda é reserva",
			sonda:          SondaDeReserva{SaldoFisico: 10, SaldoDisponivel: 4, UnidadesEmPedidoAberto: 2},
			querConclusiva: true, querLigada: true,
		},
	}

	for _, c := range casos {
		t.Run(c.nome, func(t *testing.T) {
			r := c.sonda.Avaliar()
			if r.Conclusiva != c.querConclusiva {
				t.Errorf("Conclusiva = %v, queria %v (%s)", r.Conclusiva, c.querConclusiva, r.Explicacao)
			}
			if c.querConclusiva && r.ReservaLigada != c.querLigada {
				t.Errorf("ReservaLigada = %v, queria %v (%s)", r.ReservaLigada, c.querLigada, r.Explicacao)
			}
			if r.Explicacao == "" {
				t.Error("a sonda tem de EXPLICAR — um veredito sem motivo não ajuda quem depura")
			}
		})
	}
}

// A regra dura: nativo só vale com confirmação. É o que impede o LiveCart de
// acreditar que o ERP está segurando a peça enquanto ninguém está.
func TestNativoSoValeComSondaCONFIRMANDO(t *testing.T) {
	casos := []struct {
		nome     string
		escolha  ModoDeReserva
		sonda    ResultadoDaSonda
		querModo ModoDeReserva
	}{
		{
			nome:     "nativo + conta reserva → NATIVO",
			escolha:  ReservaNativaDoERP,
			sonda:    ResultadoDaSonda{Conclusiva: true, ReservaLigada: true, Explicacao: "x"},
			querModo: ReservaNativaDoERP,
		},
		{
			nome:     "nativo + conta NÃO reserva → cai para local",
			escolha:  ReservaNativaDoERP,
			sonda:    ResultadoDaSonda{Conclusiva: true, ReservaLigada: false, Explicacao: "x"},
			querModo: ReservaSomenteLocal,
		},
		{
			nome:     "nativo + sonda inconclusiva → cai para local",
			escolha:  ReservaNativaDoERP,
			sonda:    ResultadoDaSonda{Conclusiva: false, Explicacao: "x"},
			querModo: ReservaSomenteLocal,
		},
		{
			nome:     "local pedido → local, sem consultar a sonda",
			escolha:  ReservaSomenteLocal,
			sonda:    ResultadoDaSonda{Conclusiva: true, ReservaLigada: true, Explicacao: "x"},
			querModo: ReservaSomenteLocal,
		},
		{
			nome:     "modo inválido → local",
			escolha:  ModoDeReserva("turbo"),
			sonda:    ResultadoDaSonda{Conclusiva: true, ReservaLigada: true, Explicacao: "x"},
			querModo: ReservaSomenteLocal,
		},
	}

	for _, c := range casos {
		t.Run(c.nome, func(t *testing.T) {
			modo, motivo := EscolherModo(c.escolha, c.sonda)
			if modo != c.querModo {
				t.Errorf("modo = %q, queria %q (motivo: %s)", modo, c.querModo, motivo)
			}
			if motivo == "" {
				t.Error("a decisão tem de vir com motivo — o lojista precisa saber POR QUE caiu no local")
			}
		})
	}
}

// O preço de cada modo tem de estar escrito, e ser diferente. É o que o lojista
// lê antes de escolher; sem isso ele não escolhe, adivinha.
func TestCadaModoExplicaOSeuPrecoParaOLojista(t *testing.T) {
	nativo := ReservaNativaDoERP.PrecoParaOLojista()
	local := ReservaSomenteLocal.PrecoParaOLojista()

	if nativo == "" || local == "" {
		t.Fatal("os dois modos precisam explicar o próprio preço")
	}
	if nativo == local {
		t.Error("os textos são iguais — se o preço não difere, a escolha é falsa")
	}
	// O modo local tem de dizer a consequência ruim, não só a boa.
	if !contemAlgum(local, "outro canal", "não enxerga") {
		t.Errorf("o texto do modo local não avisa sobre os outros canais: %q", local)
	}
	// O nativo tem de dizer que depende da configuração da conta.
	if !contemAlgum(nativo, "LIGADA", "Reserva de estoque") {
		t.Errorf("o texto do modo nativo não avisa que depende da config da conta: %q", nativo)
	}
}

func contemAlgum(s string, alvos ...string) bool {
	for _, a := range alvos {
		if len(a) > 0 && len(s) >= len(a) {
			for i := 0; i+len(a) <= len(s); i++ {
				if s[i:i+len(a)] == a {
					return true
				}
			}
		}
	}
	return false
}

// O TINY NÃO PODE MUDAR. Lá o pedido de venda É a reserva desde sempre, e o
// LiveCart cria o pedido no primeiro comentário. Aplicar o padrão genérico
// (local) a ele faria o pedido deixar de nascer na live — a regressão mais cara
// possível, no caminho que fatura hoje.
func TestTinyEhNativoPorPadraoEOTinyNaoPodeMudar(t *testing.T) {
	if got := ModoDeReservaPadraoDoProvider("tiny"); got != ReservaNativaDoERP {
		t.Fatalf("padrão do Tiny = %q, queria %q — com outro valor o pedido deixa "+
			"de nascer no comentário e a live do Tiny para de reservar", got, ReservaNativaDoERP)
	}
	// E sem metadata nenhum, que é o estado de TODA integração Tiny em produção.
	if got := ModoDeReservaDaIntegracao("tiny", nil); got != ReservaNativaDoERP {
		t.Errorf("Tiny sem metadata = %q, queria %q", got, ReservaNativaDoERP)
	}
	if got := ModoDeReservaDaIntegracao("tiny", map[string]any{}); got != ReservaNativaDoERP {
		t.Errorf("Tiny com metadata vazio = %q, queria %q", got, ReservaNativaDoERP)
	}
}

// O Bling cai no padrão seguro: a reserva dele depende de uma configuração da
// conta que só a sonda confirma.
func TestBlingEhLocalPorPadrao(t *testing.T) {
	if got := ModoDeReservaPadraoDoProvider("bling"); got != ReservaSomenteLocal {
		t.Errorf("padrão do Bling = %q, queria %q", got, ReservaSomenteLocal)
	}
	if got := ModoDeReservaDaIntegracao("bling", nil); got != ReservaSomenteLocal {
		t.Errorf("Bling sem metadata = %q, queria %q", got, ReservaSomenteLocal)
	}
}

// A escolha explícita do lojista vence o padrão do provider — nos dois sentidos.
func TestAEscolhaDoLojistaVenceOPadraoDoProvider(t *testing.T) {
	nativo := map[string]any{ChaveModoDeReserva: string(ReservaNativaDoERP)}
	local := map[string]any{ChaveModoDeReserva: string(ReservaSomenteLocal)}

	if got := ModoDeReservaDaIntegracao("bling", nativo); got != ReservaNativaDoERP {
		t.Errorf("Bling com escolha nativa = %q", got)
	}
	// Um lojista Tiny que escolha local tem de ser obedecido: é escolha dele.
	if got := ModoDeReservaDaIntegracao("tiny", local); got != ReservaSomenteLocal {
		t.Errorf("Tiny com escolha local = %q, queria %q — a escolha explícita vence", got, ReservaSomenteLocal)
	}
}

// Metadata malformado NÃO pode virar uma escolha: cai no padrão do provider.
func TestMetadataMalformadoCaiNoPadraoDoProvider(t *testing.T) {
	lixo := []map[string]any{
		{ChaveModoDeReserva: "turbo"},
		{ChaveModoDeReserva: 42},
		{ChaveModoDeReserva: ""},
		{ChaveModoDeReserva: nil},
	}
	for _, m := range lixo {
		if got := ModoDeReservaDaIntegracao("tiny", m); got != ReservaNativaDoERP {
			t.Errorf("Tiny com metadata %v = %q, queria o padrão %q", m, got, ReservaNativaDoERP)
		}
		if got := ModoDeReservaDaIntegracao("bling", m); got != ReservaSomenteLocal {
			t.Errorf("Bling com metadata %v = %q, queria o padrão %q", m, got, ReservaSomenteLocal)
		}
	}
}

// ─── A CAPACIDADE É OBSERVADA, NÃO PERGUNTADA ───────────────────────────────
//
// Se o ERP reserva ou não é configuração do LOJISTA, no ERP dele: o LiveCart
// não liga, não desliga e não deveria fingir que decide. O que ele pode é
// SABER — e a resposta chega de graça em toda leitura de saldo.

func TestDisponivelMenorQueFisicoProvaQueAContaReserva(t *testing.T) {
	casos := []struct {
		nome               string
		fisico, disponivel int
		quero              ObservacaoDeReserva
		porque             string
	}{
		{"há peça reservada", 5, 3, ContaReserva,
			"duas unidades estão presas em pedido: o ERP está segurando"},
		{"nada reservado", 5, 5, NadaAObservar,
			"pode simplesmente não haver pedido aberto agora — não prova nada"},
		{"estoque zerado", 0, 0, NadaAObservar,
			"sem peça não há o que reservar"},
		{"disponível maior que o físico", 5, 7, NadaAObservar,
			"número impossível; não vira prova de nada"},
		{"disponível negativo", 5, -2, ContaReserva,
			"saldo negativo é sintoma, mas ainda é menor que o físico"},
	}
	for _, c := range casos {
		t.Run(c.nome, func(t *testing.T) {
			if got := ObservarCapacidade(c.fisico, c.disponivel); got != c.quero {
				t.Errorf("físico=%d disponível=%d → %v, queria %v (%s)",
					c.fisico, c.disponivel, got, c.quero, c.porque)
			}
		})
	}
}

// A ausência de reserva NUNCA derruba uma confirmação.
//
// "Não vi reserva agora" tem três causas indistinguíveis: a conta não reserva,
// não há pedido aberto, ou o ERP ainda não refletiu o pedido de cinco segundos
// atrás (medido: 9 a 22 s). Tratar as três como "não reserva" derrubaria um
// lojista do modo nativo no meio de uma live.
func TestObservacaoSoSabeDizerSimOuNaoSei(t *testing.T) {
	for fisico := 0; fisico <= 5; fisico++ {
		for disp := -2; disp <= 7; disp++ {
			got := ObservarCapacidade(fisico, disp)
			if got != ContaReserva && got != NadaAObservar {
				t.Fatalf("físico=%d disp=%d devolveu %v — só existem 'sim' e 'não sei'",
					fisico, disp, got)
			}
		}
	}
}

// O TINY É ISENTO: lá o pedido de venda É a reserva, por desenho do produto.
// Exigir prova dele quebraria quem fatura hoje.
func TestTinyNaoPrecisaProvarCapacidade(t *testing.T) {
	if !CapacidadeConfirmada("tiny", nil) {
		t.Error("o Tiny passou a precisar de prova — o pedido dele JÁ é a reserva, " +
			"e sem isso ele pararia de poder usar o modo nativo")
	}
	if CapacidadeConfirmada("bling", nil) {
		t.Error("o Bling passou sem prova — é exatamente o caminho que vende a " +
			"mesma peça duas vezes")
	}
	if !CapacidadeConfirmada("bling", map[string]any{ChaveCapacidadeDeReserva: true}) {
		t.Error("a prova gravada não foi reconhecida")
	}
	// Metadata malformado não liga nada.
	for _, lixo := range []any{"sim", 1, nil, "true"} {
		if CapacidadeConfirmada("bling", map[string]any{ChaveCapacidadeDeReserva: lixo}) {
			t.Errorf("metadata %v (%T) foi lido como prova", lixo, lixo)
		}
	}
}
