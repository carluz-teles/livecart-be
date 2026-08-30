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
			if got := ModoDeReservaDoMetadata(c.metadata); got != ReservaSomenteLocal {
				t.Errorf("modo = %q, queria %q — metadata malformado NÃO pode ligar "+
					"um comportamento que muda o que a loja vende", got, ReservaSomenteLocal)
			}
		})
	}
}

func TestModoValidoEhLidoDoMetadata(t *testing.T) {
	for _, m := range []ModoDeReserva{ReservaNativaDoERP, ReservaSomenteLocal} {
		got := ModoDeReservaDoMetadata(map[string]any{ChaveModoDeReserva: string(m)})
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
