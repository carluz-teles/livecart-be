package live

// Leitura do comentário: o que a compradora pediu, e quanto de cada coisa.
//
// O parser antigo respondia duas perguntas separadas — "é compra?" e "qual a
// quantidade?" — e devolvia UM número. Isso o prendia a um produto por
// comentário e o deixava cego para a forma como as pessoas realmente escrevem.
// Os defeitos abaixo saíram todos da live de 16/08, do texto real:
//
//	"1024x3"            perdido inteiro. `\b` exige fronteira de palavra, e entre
//	                    "1024" e "x3" não existe uma — nenhum código era extraído
//	                    e nenhum padrão de quantidade casava.
//	"1208 × 4"          o sinal é U+00D7 (×), não a letra x. Vinha do teclado do
//	                    celular e não casava com padrão nenhum.
//	"1000 5x 1005 3x"   dois produtos com quantidades diferentes viravam UM item:
//	                    o primeiro código, com a primeira quantidade.
//	"valor 1000"        virava pedido. "1000" é um código válido, e nenhuma das
//	                    perguntas conhecidas cobria a palavra "valor" sozinha.
//	"Quero esse de 15,90  3"
//	                    "15" era lido como quantidade — 15 unidades do produto em
//	                    destaque por um comentário que pedia 3.
//
// A leitura agora é em duas etapas: normalizar o texto para uma forma canônica,
// e depois varrer token a token juntando (código, quantidade). Um comentário
// pode pedir vários produtos.

import (
	"regexp"
	"strconv"
	"strings"
)

// PurchaseItem é UM pedido dentro do comentário.
type PurchaseItem struct {
	// Keyword é o código de 4 caracteres, em maiúsculas. Vazio quando a
	// compradora pediu sem código ("quero") — quem resolve isso é o produto em
	// destaque da transmissão.
	Keyword string
	// Quantity é o que ela pediu daquele código. 1 quando não disse.
	Quantity int
}

// precoRe reconhece dinheiro escrito: 15,90 · 3.391,90 · R$ 567,90.
//
// Existe para que preço nunca vire quantidade. Sem ela, "de 15,90 3" lia 15.
var precoRe = regexp.MustCompile(`\d+[.,]\d{2}\b`)

// palavrasDePreco marcam o comentário como conversa sobre valor.
//
// Não bastam para recusar sozinhas — ver marcadorExplicitoRe.
var palavrasDePreco = regexp.MustCompile(`(?i)(\bvalor\b|\bpre[cç]o\b|\bquanto\b|\bcusta\b|R\$)`)

// marcadorExplicitoRe é o sinal forte de que a pessoa está PEDINDO: uma
// quantidade colada ao código ("x2", "2x", "- 3").
//
// É ele que resolve o conflito com as palavras de preço. "valor 1000" não tem
// marcador e é pergunta; "1130 x 2 quanto fica?" tem, e é pedido com pergunta
// junto. Sem esse desempate, ou perderíamos a venda ou criaríamos o pedido que
// ninguém pediu — e o segundo custa estoque e pedido no ERP.
var marcadorExplicitoRe = regexp.MustCompile(`(?i)([0-9A-Za-z]{4}\s*[-x]\s*\d{1,2}|\d{1,2}\s*x\b)`)

// normalizarComentario põe o texto na forma que o scanner entende.
//
// Três trocas, todas colhidas do texto real da live:
//
//	× (U+00D7) e * viram x — vêm do teclado do celular;
//	"1024x3" ganha o espaço que faltava entre código e quantidade;
//	preço é apagado, para não ser lido como quantidade depois.
func normalizarComentario(texto string) string {
	t := strings.NewReplacer("×", "x", "✕", "x", "✖", "x", "*", "x").Replace(texto)
	t = precoRe.ReplaceAllString(t, " ")
	// Separa código colado da quantidade: 1024x3 → 1024 x3
	t = regexp.MustCompile(`(?i)([0-9A-Za-z]{4})x(\d{1,2})\b`).ReplaceAllString(t, "$1 x$2")
	// Separa palavra colada ANTES do código: "Código1485" → "Código 1485".
	// Da live de 19/08: a @mariabsales escreveu "Código1485 X2" e perdeu a
	// compra — o "ó" não é ASCII, o tokenizador quebrava em "digo1485", e
	// nenhum código sobrava. O MESMO comentário dela com espaço ("Código 1543
	// X 3"), 18 minutos antes, tinha funcionado. Só letra→dígito com exatamente
	// 4 dígitos e fronteira: não mexe em "R$3391" (o preço já foi apagado
	// acima) nem em números longos.
	t = regexp.MustCompile(`(?i)([a-zà-ú])(\d{4})\b`).ReplaceAllString(t, "$1 $2")
	return t
}

// tokenQuantidadeRe casa "2", "x2", "2x", "x 2" já tokenizado.
var tokenQuantidadeRe = regexp.MustCompile(`(?i)^x?(\d{1,2})x?$`)

// tokenizar quebra o texto em pedaços alfanuméricos, descartando o resto.
func tokenizar(texto string) []string {
	return regexp.MustCompile(`[0-9A-Za-z]+`).FindAllString(texto, -1)
}

// ParsePurchaseItems lê os pedidos de um comentário.
//
// Devolve nil quando não há intenção de compra. A ordem dos itens é a do texto,
// porque é a ordem em que a compradora pensou — e é ela que a tela mostra.
func ParsePurchaseItems(texto string) []PurchaseItem {
	texto = strings.TrimSpace(texto)
	if texto == "" {
		return nil
	}

	for _, p := range negativePatterns {
		if p.MatchString(texto) {
			return nil
		}
	}
	for _, p := range questionPatterns {
		if p.MatchString(texto) {
			return nil
		}
	}

	// Conversa sobre dinheiro só passa com marcador explícito de pedido.
	if palavrasDePreco.MatchString(texto) && !marcadorExplicitoRe.MatchString(texto) {
		return nil
	}

	// "1107 ou 1207" é pergunta de qual é o código, não pedido dos dois.
	if codigoOuCodigoRe.MatchString(texto) {
		return nil
	}

	normalizado := normalizarComentario(texto)
	tokens := tokenizar(normalizado)

	var itens []PurchaseItem
	// quantidadeSolta é a quantidade que apareceu ANTES de qualquer código.
	// Serve a duas coisas, nesta ordem: se um código vier depois, ela é dele
	// ("quero 5 1001", "2x 1130" — a pessoa disse quanto antes de dizer o quê);
	// se nenhum código vier, ela é do produto em destaque ("quero 2").
	quantidadeSolta := 0

	for _, tk := range tokens {
		up := strings.ToUpper(tk)

		if isValidKeyword(up) {
			q := 1
			if len(itens) == 0 && quantidadeSolta > 0 {
				q = quantidadeSolta
				quantidadeSolta = 0
			}
			itens = append(itens, PurchaseItem{Keyword: up, Quantity: q})
			continue
		}

		m := tokenQuantidadeRe.FindStringSubmatch(tk)
		if m == nil {
			continue
		}
		n, err := strconv.Atoi(m[1])
		if err != nil || n <= 0 {
			continue
		}
		if n > maxQuantidadePorItem {
			n = maxQuantidadePorItem
		}
		// A quantidade pertence ao ÚLTIMO código visto: é assim que as pessoas
		// escrevem ("1130 x2", "1130 2x"). Sem código antes, ela fica solta e só
		// vale se o comentário inteiro não tiver código nenhum.
		if len(itens) > 0 {
			itens[len(itens)-1].Quantity = n
		} else if quantidadeSolta == 0 {
			quantidadeSolta = n
		}
	}

	if len(itens) > 0 {
		return unificarRepetidos(itens)
	}

	if pedidoSemCodigo(texto, tokens) {
		q := quantidadeSolta
		if q == 0 {
			q = 1
		}
		return []PurchaseItem{{Quantity: q}}
	}

	return nil
}

// unificarRepetidos junta o mesmo código citado mais de uma vez no comentário.
//
// A leitura por item passou a produzir uma entrada por citação, e citações
// repetidas do MESMO código somavam: "1130 x2 1130 x3" virava cinco unidades, e
// "1130 1130" — o código digitado duas vezes — virava duas.
//
// Dentro de um único comentário, repetir o código é correção ou dedo duplo com
// muito mais frequência do que é soma: quem quer cinco escreve "1130 x5". Então
// vale a ÚLTIMA quantidade dita, que é a versão mais recente da intenção dela.
//
// A escolha segue a assimetria de custo de todo o resto deste parser: entregar
// menos do que ela quis é um comentário a mais; entregar mais é unidade que ela
// não pediu, com baixa de estoque e pedido no ERP. A ordem é a da primeira
// aparição, porque é a ordem em que ela pensou.
func unificarRepetidos(itens []PurchaseItem) []PurchaseItem {
	if len(itens) < 2 {
		return itens
	}
	posicao := make(map[string]int, len(itens))
	out := make([]PurchaseItem, 0, len(itens))
	for _, it := range itens {
		if i, visto := posicao[it.Keyword]; visto {
			out[i].Quantity = it.Quantity
			continue
		}
		posicao[it.Keyword] = len(out)
		out = append(out, it)
	}
	return out
}

// pedidoSemCodigo decide se um comentário SEM código é um pedido.
//
// Existe para uma situação só: a apresentadora diz "comenta QUERO" e as pessoas
// comentam exatamente isso. Quem escolhe o produto é o destaque da transmissão,
// uma camada acima — e é justamente por isso que a régua aqui é mais dura que a
// do comentário com código.
//
// Com código, o texto diz QUAL produto. Sem código, quem diz é o destaque, e
// esse vínculo só é confiável quando o comentário não fala de mais nada. Assim
// que existe frase em volta do verbo, a chance de a pessoa estar falando de
// outra coisa passa a dominar — e o erro aqui não é perder uma venda, é criar
// pedido do produto ERRADO. Todas as frases abaixo são da live de 16/08 e
// morrem nesta função:
//
//	"Tb quero ver a cascata de luzes"
//	"Queria ver cavalinho e brinquedos com neve em movimento"
//	"Aqui só faz sol. Nunca tem chuvaaaaa, queria chuvinha"
//	"Manda a chuva que mandamos o sol kkkkkk"
//	"Árvore de Natal de 2.10 m tem? Quero uma que fique bem cheia 😬"
func pedidoSemCodigo(texto string, tokens []string) bool {
	// Ou o verbo ("quero"), ou a unidade dita em voz alta ("5 unidades por
	// favor") — as duas formas de pedir sem nomear o produto.
	if !verboDeCompraRe.MatchString(texto) && !unidadesRe.MatchString(texto) {
		return false
	}
	// "quero ver", "queria ver": olhar não é comprar. Vale em qualquer tamanho.
	if verboDeOlharRe.MatchString(texto) {
		return false
	}
	// Negação em qualquer lugar: "Não é o galho q quero".
	if negacaoRe.MatchString(texto) {
		return false
	}
	return len(tokens) <= maxTokensPedidoNu
}

// maxTokensPedidoNu é o tamanho até o qual um comentário sem código ainda é
// "só o pedido".
//
// Quatro cobre o que as pessoas escrevem quando estão pedindo — "Quero", "Eu
// quero", "quero 2", "me manda 3", "QUERO A MANTA VERDE" — e não cobre frase
// nenhuma. É uma régua grossa de propósito: sem código, preferimos perder o
// pedido a criar o errado, porque o errado só aparece na hora de separar a
// caixa.
const maxTokensPedidoNu = 4

// verboDeOlharRe: pedir para VER é conversa com a apresentadora.
var verboDeOlharRe = regexp.MustCompile(`(?i)\b(ver|mostra|passa|repetir)\b`)

// negacaoRe pega a negação solta, além dos negativePatterns fechados.
var negacaoRe = regexp.MustCompile(`(?i)\bn[aã]o\b`)

// maxQuantidadePorItem é o teto de sanidade por linha do comentário.
const maxQuantidadePorItem = 100

// codigoOuCodigoRe pega a dúvida entre dois códigos.
//
// Da live de 17/08: "O código das velas é 1107 ou 1207. Me corrige por favor".
// A leitura por item, que existe para atender "1000 5x 1005 3x", transformava
// isso em pedido dos DOIS produtos — e ela estava perguntando qual era o certo.
//
// O "ou" é a diferença, e SÓ o "ou". Ninguém pede dois produtos dizendo "ou";
// quem pede lista. Uma pergunta mal lida aqui custa um produto que a compradora
// nunca pediu, na caixa dela.
//
// A barra estava aqui e saiu: "1130 / 1207 / 1145" é lista, não escolha, e em
// português a barra separa itens com a mesma frequência com que oferece
// alternativa. Mantê-la recusava três pedidos legítimos para pegar uma pergunta
// que o "ou" já pega.
var codigoOuCodigoRe = regexp.MustCompile(`(?i)\b\d{4}\s*ou\s*\d{4}\b`)

// unidadesRe cobre "5 unidades por favor", "1 unidade" — pedido sem verbo e sem
// código, em que a quantidade é o próprio pedido.
var unidadesRe = regexp.MustCompile(`(?i)\b\d{1,2}\s+unidades?\b`)

// verboDeCompraRe cobre o pedido sem código.
var verboDeCompraRe = regexp.MustCompile(
	`(?i)\b(quero|queria|manda|me\s+manda|separa|reserva|pega|coloca|vou\s+levar|leva)\b`)

// ─────────────────────────────────────────────────────────────────────────────
// Explicação para o log. NADA aqui participa da decisão.
// ─────────────────────────────────────────────────────────────────────────────

// MotivoDaRecusa diz, em uma expressão, por que o comentário não virou pedido.
//
// Roda DEPOIS da decisão, sobre o mesmo texto, e só quando ela foi negativa —
// não pode mudar o que já foi decidido. Existe para que "a Fulana comentou e
// não entrou" tenha resposta na hora, em vez de alguém reprocessar o comentário
// à mão. Foi assim que os defeitos da live de 16/08 apareceram: puxando 501
// comentários do log e passando um a um pelo parser.
//
// A ordem espelha a de ParsePurchaseItems, porque o primeiro portão que fecha é
// o que explica.
func MotivoDaRecusa(texto string) string {
	texto = strings.TrimSpace(texto)
	if texto == "" {
		return "texto vazio"
	}
	for _, p := range negativePatterns {
		if p.MatchString(texto) {
			return "negação ou cancelamento"
		}
	}
	for _, p := range questionPatterns {
		if p.MatchString(texto) {
			return "pergunta conhecida"
		}
	}
	if palavrasDePreco.MatchString(texto) && !marcadorExplicitoRe.MatchString(texto) {
		return "fala de preço sem quantidade explícita"
	}

	tokens := tokenizar(normalizarComentario(texto))
	temVerbo := verboDeCompraRe.MatchString(texto) || unidadesRe.MatchString(texto)
	switch {
	case !temVerbo:
		return "sem código e sem verbo de compra"
	case verboDeOlharRe.MatchString(texto):
		return "pediu para ver, não para comprar"
	case negacaoRe.MatchString(texto):
		return "negação na frase"
	case len(tokens) > maxTokensPedidoNu:
		return "verbo dentro de uma frase, sem código"
	}
	return "sem código reconhecido"
}

// DescreveItens resume o pedido lido numa string de log: "1130x2 1144x3".
//
// O log tinha a contagem de itens e a quantidade somada, mas não QUAIS. Numa
// live, saber que o comentário virou "2 itens, 5 unidades" não diz se lemos os
// produtos certos.
func DescreveItens(itens []PurchaseItem) string {
	if len(itens) == 0 {
		return ""
	}
	partes := make([]string, 0, len(itens))
	for _, it := range itens {
		kw := it.Keyword
		if kw == "" {
			kw = "destaque"
		}
		partes = append(partes, kw+"x"+strconv.Itoa(it.Quantity))
	}
	return strings.Join(partes, " ")
}
