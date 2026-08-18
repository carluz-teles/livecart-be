package notification

// A mensagem de fila precisa dizer QUANTO ficou na fila.
//
// Caso real, live de 17/08. @karinafragahoelz comentou "1368 x10". O estoque do
// Papai Noel em Pé – 40cm era 2: duas unidades entraram no carrinho dela e oito
// foram para a fila. A mensagem que ela recebeu era esta:
//
//	Oi @karinafragahoelz! ⏳
//	Papai Noel em Pé – 40cm entrou na fila de espera — o estoque acabou...
//	Carrinho: 24 itens — R$ 1393.60
//
// Ela respondeu na live, dois segundos depois:
//
//	"É está estranho adicionou 24 noéis de 40cm no meu carrinho"
//	"Por mais que eu digitei errado o código, a quantidade não bate"
//	"Coloquei x10"
//
// Ela leu as duas linhas juntas, que é como se lê: o nome do produto e, logo
// abaixo, um número. Os 24 eram o carrinho INTEIRO dela — 14 itens que já
// estavam lá mais os 10 deste pedido. A mensagem nunca disse 10, nunca disse 2,
// nunca disse 8.
//
// O dado existia o tempo todo: WaitlistedQty já viajava até o notificador, e
// era usado só para ESCOLHER o template. Nunca chegava dentro dele.

import (
	"strings"
	"testing"
)

// pedidoDaKarina reproduz os números exatos daquela noite.
func pedidoDaKarina() TemplateVariables {
	return TemplateVariables{
		Handle:             "@karinafragahoelz",
		Produto:            "Papai Noel em Pé – 40cm",
		Quantidade:         10, // o que ela pediu
		QuantidadeCarrinho: 2,  // o que o estoque cobriu
		QuantidadeFila:     8,  // o que ficou aguardando
		TotalItens:         24, // o CARRINHO INTEIRO dela
		Total:              "R$ 1393.60",
		Link:               "https://app.livecart.com.br/cart/abc",
	}
}

func templateDeFila(t *testing.T) string {
	t.Helper()
	tpl := DefaultSettings().WaitlistJoined
	if tpl == nil || tpl.Template == "" {
		t.Fatal("template padrão de waitlist_joined não encontrado")
	}
	return tpl.Template
}

func TestMensagemDeFilaDizAQuebraDoPedido(t *testing.T) {
	msg := RenderTemplate(templateDeFila(t), pedidoDaKarina())

	for _, n := range []struct{ valor, porque string }{
		{"10", "quanto ela pediu — sem isso ela não confere o próprio pedido"},
		{"2", "quanto entrou no carrinho de fato"},
		{"8", "quanto ficou na fila; é a informação que a mensagem existe para dar"},
	} {
		if !strings.Contains(msg, n.valor) {
			t.Errorf("a mensagem não cita %q (%s):\n%s", n.valor, n.porque, msg)
		}
	}
}

// O total do carrinho não pode encostar no nome do produto sem dizer que é o
// carrinho inteiro. Foi essa colagem que fez ela ler 24 unidades de um produto.
func TestTotalDoCarrinhoNaoSeConfundeComOProduto(t *testing.T) {
	msg := RenderTemplate(templateDeFila(t), pedidoDaKarina())

	i := strings.Index(msg, "24")
	if i < 0 {
		t.Fatalf("o total do carrinho sumiu da mensagem:\n%s", msg)
	}
	// A linha do 24 precisa se identificar como o carrinho inteiro.
	inicio := strings.LastIndex(msg[:i], "\n") + 1
	fim := strings.Index(msg[i:], "\n")
	if fim < 0 {
		fim = len(msg) - i
	}
	linha := msg[inicio : i+fim]

	if !strings.Contains(strings.ToUpper(linha), "CARRINHO") {
		t.Errorf("a linha do total (%q) não diz que é o carrinho — foi assim que "+
			"a compradora leu 24 unidades de um produto só", linha)
	}
	if strings.Contains(linha, "Papai Noel") {
		t.Errorf("o total do carrinho está na mesma linha do produto: %q", linha)
	}
}

// Pedido que não coube em nada: a mensagem continua verdadeira.
func TestPedidoInteiroNaFila(t *testing.T) {
	v := pedidoDaKarina()
	v.Quantidade, v.QuantidadeCarrinho, v.QuantidadeFila = 3, 0, 3

	msg := RenderTemplate(templateDeFila(t), v)
	if !strings.Contains(msg, "0") || !strings.Contains(msg, "3") {
		t.Errorf("com nada entrando no carrinho a mensagem precisa dizer 0 e 3:\n%s", msg)
	}
}

// A quebra é oferecida ao lojista que quiser editar o texto. Sem estar no
// escopo, a variável não aparece no editor de templates e ninguém a usa.
func TestQuebraEhOferecidaNoEditorDeTemplate(t *testing.T) {
	oferecidas := map[string]bool{}
	for _, v := range VariablesForTemplate("waitlist_joined") {
		oferecidas[v.Name] = true
	}
	for _, n := range []string{"{quantidade}", "{quantidade_carrinho}", "{quantidade_fila}"} {
		if !oferecidas[n] {
			t.Errorf("%s não é oferecida no template de fila", n)
		}
	}
}
