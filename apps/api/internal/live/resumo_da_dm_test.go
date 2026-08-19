package live

// A DM de um comentário com VÁRIOS produtos, quando só um deles vai para a fila.
//
// Uma mensagem por comentário é a decisão certa: o carrinho é um e o link é o
// mesmo. Mas ela precisa dizer a verdade sobre CADA coisa que aconteceu, e o
// caso misto é onde isso quebra.
//
// "1000 x2 1005 x3" com o 1005 quase esgotado: o Vaso entra inteiro, o Prato
// entra 1 e fica 2 na fila. A mensagem que sai é a de fila (qualquer parte na
// fila troca o assunto — é regra, e está certa), e ela nomeia {produto}. Se
// {produto} for "Vaso e Prato" com "Na fila 2", a compradora não tem como saber
// qual dos dois está esperando. Ela olha o Vaso, que está no carrinho, e conclui
// que o pedido dela está incompleto no produto errado.

import "testing"

func itemDoResumo(nome, kw string, pedida, naFila int) resultadoDoItem {
	return resultadoDoItem{
		produto: &ProductRow{ID: "p-" + kw, Keyword: kw, Name: nome},
		pedida:  pedida,
		naFila:  naFila,
	}
}

// O caso misto: só o Prato ficou na fila.
func TestResumoDaFilaNomeiaSoOQueEstaNaFila(t *testing.T) {
	r := resumoDosItens([]resultadoDoItem{
		itemDoResumo("Vaso", "1000", 2, 0),
		itemDoResumo("Prato", "1005", 3, 2),
	})

	if r.nomes != "Prato" {
		t.Errorf("nomes = %q; a mensagem é de FILA e só o Prato está na fila. "+
			"Citar o Vaso junto faz a compradora achar que o Vaso é que está esperando",
			r.nomes)
	}
	if r.pedida != 3 {
		t.Errorf("pedida = %d; esperava 3 — o que ela pediu DO PRATO, não a soma "+
			"do comentário", r.pedida)
	}
	if r.noCarrinho != 1 {
		t.Errorf("noCarrinho = %d; esperava 1 — do Prato, uma unidade entrou", r.noCarrinho)
	}
	if r.naFila != 2 {
		t.Errorf("naFila = %d; esperava 2", r.naFila)
	}
}

// Sem fila nenhuma, a mensagem é de item adicionado e fala de tudo.
func TestResumoSemFilaFalaDeTodosOsProdutos(t *testing.T) {
	r := resumoDosItens([]resultadoDoItem{
		itemDoResumo("Vaso", "1000", 5, 0),
		itemDoResumo("Prato", "1005", 3, 0),
	})

	if r.nomes != "Vaso e Prato" {
		t.Errorf("nomes = %q; esperava os dois — os dois entraram", r.nomes)
	}
	if r.pedida != 8 || r.noCarrinho != 8 || r.naFila != 0 {
		t.Errorf("pedida/carrinho/fila = %d/%d/%d; esperava 8/8/0", r.pedida, r.noCarrinho, r.naFila)
	}
}

// Tudo na fila: nomeia tudo, porque tudo está esperando.
func TestResumoComTudoNaFilaNomeiaTudo(t *testing.T) {
	r := resumoDosItens([]resultadoDoItem{
		itemDoResumo("Vaso", "1000", 2, 2),
		itemDoResumo("Prato", "1005", 3, 3),
	})
	if r.nomes != "Vaso e Prato" {
		t.Errorf("nomes = %q; os dois estão na fila", r.nomes)
	}
	if r.pedida != 5 || r.noCarrinho != 0 || r.naFila != 5 {
		t.Errorf("pedida/carrinho/fila = %d/%d/%d; esperava 5/0/5", r.pedida, r.noCarrinho, r.naFila)
	}
}

// Um produto só, parcial — o caso da @karinafragahoelz. Não pode ter regredido.
func TestResumoDeUmProdutoParcial(t *testing.T) {
	r := resumoDosItens([]resultadoDoItem{itemDoResumo("Papai Noel em Pé – 40cm", "1368", 10, 8)})
	if r.nomes != "Papai Noel em Pé – 40cm" || r.pedida != 10 || r.noCarrinho != 2 || r.naFila != 8 {
		t.Errorf("resumo = %+v; esperava 10 pedidas, 2 no carrinho, 8 na fila", r)
	}
}
