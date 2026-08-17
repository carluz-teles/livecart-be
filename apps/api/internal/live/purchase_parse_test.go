package live

// A leitura do comentário, contra o texto REAL da live de 16/08.
//
// O corpus abaixo saiu dos comentários daquela transmissão — 501 comentários,
// 294 distintos. Não são frases inventadas para o parser passar: são as frases
// que as compradoras e a lojista escreveram, com o acento, o emoji, o espaço
// duplo e o erro de digitação que tinham. É por isso que a tabela é grande e
// repetitiva: cada linha é uma pessoa que teve, ou não teve, o pedido criado.
//
// A divisão importa mais que o total. Os dois lados do erro NÃO custam a mesma
// coisa:
//
//	falso NEGATIVO  a venda se perde. A compradora comentou "1024x3", nada
//	                aconteceu, e ela foi embora achando que pediu.
//	falso POSITIVO  cria pedido que ninguém fez. Some estoque, entra pedido no
//	                ERP, a lojista descobre na hora de separar a caixa. Foi o
//	                que ela relatou: "valor 1000" virou pedido.
//
// Por isso a tabela tem mais casos de NÃO-compra que de compra: o que precisa
// de mais prova é a recusa.

import (
	"reflect"
	"testing"
)

type casoDeComentario struct {
	texto    string
	esperado []PurchaseItem
}

// Casos derivados do texto real. `nil` = não é intenção de compra.
var casosDaLive = []casoDeComentario{
	{"1193", []PurchaseItem{{"1193", 1}}},
	{"1218", []PurchaseItem{{"1218", 1}}},
	{"1192", []PurchaseItem{{"1192", 1}}},
	{"1280", []PurchaseItem{{"1280", 1}}},
	{"1279", []PurchaseItem{{"1279", 1}}},
	{"1150", []PurchaseItem{{"1150", 1}}},
	{"1151", []PurchaseItem{{"1151", 1}}},
	{"1204", []PurchaseItem{{"1204", 1}}},
	{"1203", []PurchaseItem{{"1203", 1}}},
	{"1208", []PurchaseItem{{"1208", 1}}},
	{"1202", []PurchaseItem{{"1202", 1}}},
	{"1250", []PurchaseItem{{"1250", 1}}},
	{"1130", []PurchaseItem{{"1130", 1}}},
	{"1085", []PurchaseItem{{"1085", 1}}},
	{"1144", []PurchaseItem{{"1144", 1}}},
	{"1025", []PurchaseItem{{"1025", 1}}},
	{"1027", []PurchaseItem{{"1027", 1}}},
	{"1044", []PurchaseItem{{"1044", 1}}},
	{"1046", []PurchaseItem{{"1046", 1}}},
	{"1058", []PurchaseItem{{"1058", 1}}},
	{"1068", []PurchaseItem{{"1068", 1}}},
	{"1094", []PurchaseItem{{"1094", 1}}},
	{"1096", []PurchaseItem{{"1096", 1}}},
	{"1097", []PurchaseItem{{"1097", 1}}},
	{"1098", []PurchaseItem{{"1098", 1}}},
	{"1110", []PurchaseItem{{"1110", 1}}},
	{"1124", []PurchaseItem{{"1124", 1}}},
	{"1125", []PurchaseItem{{"1125", 1}}},
	{"1126", []PurchaseItem{{"1126", 1}}},
	{"1161", []PurchaseItem{{"1161", 1}}},
	{"1162", []PurchaseItem{{"1162", 1}}},
	{"1163", []PurchaseItem{{"1163", 1}}},
	{"1194", []PurchaseItem{{"1194", 1}}},
	{"1222", []PurchaseItem{{"1222", 1}}},
	{"1278", []PurchaseItem{{"1278", 1}}},
	{"1282", []PurchaseItem{{"1282", 1}}},
	{"1290", []PurchaseItem{{"1290", 1}}},
	{"1299", []PurchaseItem{{"1299", 1}}},
	{"1303", []PurchaseItem{{"1303", 1}}},
	{"1086 x 2", []PurchaseItem{{"1086", 2}}},
	{"1086 X 2", []PurchaseItem{{"1086", 2}}},
	{"1086 x 4", []PurchaseItem{{"1086", 4}}},
	{"1086 x2", []PurchaseItem{{"1086", 2}}},
	{"1086 x4", []PurchaseItem{{"1086", 4}}},
	{"1130 x 1", []PurchaseItem{{"1130", 1}}},
	{"1130 x 2", []PurchaseItem{{"1130", 2}}},
	{"1130 x 4", []PurchaseItem{{"1130", 4}}},
	{"1130 X 2", []PurchaseItem{{"1130", 2}}},
	{"1130 X 4", []PurchaseItem{{"1130", 4}}},
	{"1130 x2", []PurchaseItem{{"1130", 2}}},
	{"1130  X 3", []PurchaseItem{{"1130", 3}}},
	{"1130  X 6", []PurchaseItem{{"1130", 6}}},
	{"1144 X 3", []PurchaseItem{{"1144", 3}}},
	{"1144 x 2", []PurchaseItem{{"1144", 2}}},
	{"1144 x 3", []PurchaseItem{{"1144", 3}}},
	{"1144 x 4", []PurchaseItem{{"1144", 4}}},
	{"1144 x2", []PurchaseItem{{"1144", 2}}},
	{"1144 x 10", []PurchaseItem{{"1144", 10}}},
	{"1119 x2", []PurchaseItem{{"1119", 2}}},
	{"1121 x 2", []PurchaseItem{{"1121", 2}}},
	{"1152 x 1", []PurchaseItem{{"1152", 1}}},
	{"1204 x 1", []PurchaseItem{{"1204", 1}}},
	{"1193 x 2", []PurchaseItem{{"1193", 2}}},
	{"1193  x 2", []PurchaseItem{{"1193", 2}}},
	{"1208 x 1", []PurchaseItem{{"1208", 1}}},
	{"1208 x 3", []PurchaseItem{{"1208", 3}}},
	{"1208 x1", []PurchaseItem{{"1208", 1}}},
	{"1208 x2", []PurchaseItem{{"1208", 2}}},
	{"1212 x 1", []PurchaseItem{{"1212", 1}}},
	{"1213 x 2", []PurchaseItem{{"1213", 2}}},
	{"1213 x 4", []PurchaseItem{{"1213", 4}}},
	{"1213 x2", []PurchaseItem{{"1213", 2}}},
	{"1311 x 2", []PurchaseItem{{"1311", 2}}},
	{"1311 x 3", []PurchaseItem{{"1311", 3}}},
	{"1311 x 4", []PurchaseItem{{"1311", 4}}},
	{"1311 x2", []PurchaseItem{{"1311", 2}}},
	{"1317 x 1", []PurchaseItem{{"1317", 1}}},
	{"1130 2x", []PurchaseItem{{"1130", 2}}},
	{"1213 3x", []PurchaseItem{{"1213", 3}}},
	{"1161 2 X", []PurchaseItem{{"1161", 2}}},
	{"1207 6X", []PurchaseItem{{"1207", 6}}},
	{"1024x3", []PurchaseItem{{"1024", 3}}},
	{"1303x3", []PurchaseItem{{"1303", 3}}},
	{"1130x2", []PurchaseItem{{"1130", 2}}},
	{"1144X5", []PurchaseItem{{"1144", 5}}},
	{"1208x10", []PurchaseItem{{"1208", 10}}},
	{"1208 × 4", []PurchaseItem{{"1208", 4}}},
	{"1130 ×2", []PurchaseItem{{"1130", 2}}},
	{"1144×3", []PurchaseItem{{"1144", 3}}},
	{"1086 ✕ 2", []PurchaseItem{{"1086", 2}}},
	{"1086 * 2", []PurchaseItem{{"1086", 2}}},
	{"1086*3", []PurchaseItem{{"1086", 3}}},
	{"1121 - 1", []PurchaseItem{{"1121", 1}}},
	{"1130 - 3", []PurchaseItem{{"1130", 3}}},
	{"1203 xx", []PurchaseItem{{"1203", 1}}},
	{"1311 /  quero", []PurchaseItem{{"1311", 1}}},
	{"Quero 1280", []PurchaseItem{{"1280", 1}}},
	{"Quero 1311  3", []PurchaseItem{{"1311", 3}}},
	{"quero 1130 x 2", []PurchaseItem{{"1130", 2}}},
	{"Manda 1144 2x", []PurchaseItem{{"1144", 2}}},
	{"me manda 1208", []PurchaseItem{{"1208", 1}}},
	{"separa 1250 x3", []PurchaseItem{{"1250", 3}}},
	{"reserva 1193", []PurchaseItem{{"1193", 1}}},
	{"pega 1218 2", []PurchaseItem{{"1218", 2}}},
	{"coloca 1279 x 2", []PurchaseItem{{"1279", 2}}},
	{"1000 5x 1005 3x", []PurchaseItem{{"1000", 5}, {"1005", 3}}},
	{"1130 x2 1144 x3", []PurchaseItem{{"1130", 2}, {"1144", 3}}},
	{"1208 1213", []PurchaseItem{{"1208", 1}, {"1213", 1}}},
	{"quero 1086 x 2 e 1130 x 4", []PurchaseItem{{"1086", 2}, {"1130", 4}}},
	{"1024x3 1303x3", []PurchaseItem{{"1024", 3}, {"1303", 3}}},
	{"1144 2x 1150 1161 x5", []PurchaseItem{{"1144", 2}, {"1150", 1}, {"1161", 5}}},
	{"1311 × 2 1317 × 1", []PurchaseItem{{"1311", 2}, {"1317", 1}}},
	{"Quero", []PurchaseItem{{"", 1}}},
	{"Eu quero", []PurchaseItem{{"", 1}}},
	{"QUERO A MANTA VERDE", []PurchaseItem{{"", 1}}},
	{"quero 2", []PurchaseItem{{"", 2}}},
	{"me manda 3", []PurchaseItem{{"", 3}}},
	{"1130 x 2 quanto fica?", []PurchaseItem{{"1130", 2}}},
	{"1144 x2 qual o valor total?", []PurchaseItem{{"1144", 2}}},
	{"valor 1000", nil},
	{"Valor 1000", nil},
	{"valor 1130", nil},
	{"o valor do 1086?", nil},
	{"Qual o preço do 1086", nil},
	{"qual o valor do 1130", nil},
	{"preço 1208?", nil},
	{"quanto custa o 1144", nil},
	{"quanto é o 1193", nil},
	{"1250 quanto custa?", nil},
	{"Valor das árvores lá de cima nevadas e tamanho", nil},
	{"Valor dos cogumelos 🍄 pequenos?", nil},
	{"Valor sinos vermelho e branco?", nil},
	{"Sino valor?", nil},
	{"qual valor deste??", nil},
	{"Qual valor dela", nil},
	{"Qual valor daquele com strobo", nil},
	{"Quanto ?", nil},
	{"Gi qual o valor da bota de Natal, pendurada ali atrás?", nil},
	{"Gi, qual o valor dessa caixa com as mantas?", nil},
	{"Gi, qual o valor dos potes quebra nozes?", nil},
	{"@andressa.zanchetin olá está R$3391,90", nil},
	{"@ednacoelho___ está no vídeo ❤️ R$3315,90 / R$3392,90", nil},
	{"@limaelienetorres está no vídeo ❤️ R$3315,90 / R$3392,90", nil},
	{"@medcesar R$3391,90", nil},
	{"@marcelotilio olá R$567,90 ❤", nil},
	{"@mariana.dejesus36 olá R$761,90", nil},
	{"@cris_apolinaria o valor está no próprio vídeo flor ❤️", nil},
	{"@maria_silva0905 olá, o valor está no próprio vídeo ❤", nil},
	{"@talithamary6 olá tudo bem ? O valor está no próprio vídeo!", nil},
	{"@wilsonmatiasde_ olá, valor está no vídeo ❤️", nil},
	{"Esse cogumelo tem maior?", nil},
	{"Amo esse tronco com esquilos", nil},
	{"Aqui só faz sol. Nunca tem chuvaaaaa, queria chuvinha", nil},
	{"Esta é linda. Eu tenho", nil},
	{"Isso", nil},
	{"Acho que é as lanternas vermelhas", nil},
	{"Acho que é aquele de colocar vela.", nil},
	{"Todos lindos", nil},
	{"Amo gatos", nil},
	{"Deve ser lanternas de vidro", nil},
	{"Esses são maravilhosos, já peguei os meus 🥰", nil},
	{"Meu sonho!", nil},
	{"Tenho trauma 🤣🤣", nil},
	{"Mando foto", nil},
	{"Em cima", nil},
	{"Atrás de você", nil},
	{"Galho", nil},
	{"Galhos", nil},
	{"Flores", nil},
	{"Linda", nil},
	{"Lindo", nil},
	{"Belíssimo", nil},
	{"Maravilhosa", nil},
	{"Fofo demais", nil},
	{"Vitrini lindíssimo", nil},
	{"Sonho 😍", nil},
	{"Que lindas!😍", nil},
	{"São lindas, qualidade ótima", nil},
	{"São lindos! ✨️✨️✨️", nil},
	{"Lindas, um toque aveludada, perfeitas", nil},
	{"Boa noite", nil},
	{"Noite", nil},
	{"Tarde", nil},
	{"A tarde", nil},
	{"A noite", nil},
	{"Boa noite!🤗", nil},
	{"Boa noite 😘", nil},
	{"Boa noite …beijinhos", nil},
	{"Boa noite! Obrigada 🙏", nil},
	{"Oi boa noite!", nil},
	{"Ok", nil},
	{"Ok! Obrigada!❤️❤️", nil},
	{"Obrigada", nil},
	{"Obrigada!", nil},
	{"Obrigada, Gi", nil},
	{"Obriga", nil},
	{"Ah sim, obg", nil},
	{"Tá bom 💙", nil},
	{"Isso", nil},
	{"Bom descanso Meninas", nil},
	{"Bom descanso meninas 😘", nil},
	{"Bom descanso, até  amanhã.", nil},
	{"Hoje fez um dia lindo em SP", nil},
	{"Voaaa Gi❤️🙌🏻", nil},
	{"Vou chamar a Mi amanhã 🥰", nil},
	{".", nil},
	{"❤️", nil},
	{"🥰", nil},
	{"👏👏👏👏👏👏👏👏", nil},
	{"😔😔😔", nil},
	{"Ainda tem pisca vermelho de 1500 leds???", nil},
	{"Tem em estoque?", nil},
	{"Borboleta de led tem?", nil},
	{"Tem ovelhas de pelúcia também?", nil},
	{"Tem pisca para área externa?", nil},
	{"Tem árvore de resina?", nil},
	{"Tem as ursinhas com roupa vermelha ?", nil},
	{"Tem maior cogumelo?", nil},
	{"Tem talheres de Natal, Gi ??", nil},
	{"Tem algum cupom de desconto no site?", nil},
	{"Bolas 20cm tens?", nil},
	{"Vc teria guizo de 15 cm??", nil},
	{"Rena de ferro pequena pra decorar lareira???", nil},
	{"Gi chegará base de árvore????", nil},
	{"Gi tem casinha de vidro grande pra colocar velas dentro????", nil},
	{"Gi, tem castiçal de vidro com desenho de rena?", nil},
	{"Qual a altura das peças?", nil},
	{"Altura da ursinha?", nil},
	{"De renda?", nil},
	{"É veludo?", nil},
	{"É formato de casa", nil},
	{"Qual dessa árvore 4,20 ?", nil},
	{"Saia pra árvore de 2/10ja chegou?", nil},
	{"Árvore de Natal de 2.10 m tem? Quero uma que fique bem cheia 😬", nil},
	{"Essas luzes pisca pisca vcs vendem tb? Voltagem 220", nil},
	{"Vai vir 110 ??", nil},
	{"Ou tem que ser hoje", nil},
	{"vai mostrar decoração?", nil},
	{"Gi essas luzes no seu teto, é uma cortina ?", nil},
	{"Gi tem aquele pisca com luzes branco quente e frio junto??? Strobo?", nil},
	{"O que vc tem de cogumelos ?", nil},
	{"Oi será tem ainda as capas de almofada ? De quebra nozes e cavalinho?", nil},
	{"O trenó vermelho não desmonta né , Gi?!", nil},
	{"Gi , boa noite vai vim aquele kit de malas", nil},
	{"Gi aquelas renas de roupas de rena está aí?", nil},
	{"Arrumou o teto Gi ??", nil},
	{"Gi, para jardim tem mais opções?", nil},
	{"não quero", nil},
	{"nao quero", nil},
	{"Não é o galho q quero", nil},
	{"cancela", nil},
	{"desisto", nil},
	{"não preciso", nil},
	{"cancela o 1130", nil},
	{"desisto do 1144", nil},
	{"não quero mais", nil},
	{"tira o meu", nil},
	{"remove", nil},
	{"Mostra as almofadas vermelhas por favor", nil},
	{"Mostra as bandejas que chegaram Gi", nil},
	{"Gi, mostra noel resina", nil},
	{"Passa esse pick cogumelo", nil},
	{"Pode repetir o código..... almofada  xadrez,", nil},
	{"Queria ver cavalinho e brinquedos com neve em movimento", nil},
	{"Tb quero ver a cascata de luzes", nil},
	{"Ah, vi sim. Queria algo tipo os postes", nil},
	{"193", nil},
	{"Gi entrou somente 1", nil},
	{"Os códigos tem hiperfoco no 12 kkkkkkkkk", nil},
	{"Vi, fui fazer o pagamento e o número do meu prédio é 163 e já está registrado 123. Não estou conseguindo mudar😳", nil},
	{"0999", nil},
	{"9999999", nil},
	{"12", nil},
	{"999", nil},
	{"@anapaulabarraco78 olá não temos 😭", nil},
	{"@fepordeus esse esgotou ❤", nil},
	{"@naty_felixx esgotou ❤️", nil},
	{"@nelvadutra_personal olá esgotou ❤️", nil},
	{"@luciana202170 olá esse infelizmente já esgotou ❤️", nil},
	{"@e.moraisbarros oii vamos te chamar", nil},
	{"@eulalisueli tem q por a quantidade amore", nil},
	{"@medcesar www.cantodaart.com.br ❤️", nil},
	{"@carina.carita oiii disponível em nosso site, só pesquisar por “ avião” 😍", nil},
	{"Meu celular não aparece o xis", nil},
	{"Testar*", nil},
	{"Kkk lá vem ele", nil},
	{"Manda a chuva que mandamos o sol kkkkkk", nil},
	{"Posso finalizar amanhã", nil},
	{"Oie gi ja paguei..uhuhy", nil},
	{"Não achei no site", nil},
	{"Lá em cima gi.", nil},
	{"Os cachorrinhos", nil},
	{"Perto do presépio e mamãe Noel", nil},
	{"Ao lado da sagrada família grande", nil},
	{"Cavalinho atrás de você", nil},
	{"Então faz a noite", nil},
	{"Faz a live a noite", nil},
	{"Ahh vou esperar ❤️", nil},
	{"Pode usar como difusor", nil},
	{"esse vermelho no trenó rústico não fica bom né tem que ser rústico", nil},
	{"Eu tenho um desse , mas é cobre na árvore 🌳 aparece", nil},
	{"Aqueles piscas q a gente amava quando queimava uma lâmpada 🤡🤣🤣🤣", nil},
	{"Minha vó tinha um que tocava música.... Era irritante, mais eu amava kkkkk", nil},
	{"@isabellyeapereira minha mãe vinha com as sacolas pra testas aff 1 por 1", nil},
	{"Gi ja tinha pago..mas coloca na minha caixa aquele que pedi agora..para pagar 1 frete só ta", nil},
	{"Gi o que é vermelha metal na última prateleira perto da mamãe no", nil},
	{"Eu acho que deve ser aquelas casas de cerâmica tipo castiçal pra por velinha", nil},
	{"Esse boneco de neve ai atrás.  Você poderia mostrar", nil},
	{"Gi, já passasse as mantas, querida?", nil},
	// Quantidade ANTES do código: a pessoa diz quanto antes de dizer o quê.
	{"quero 5 1001", []PurchaseItem{{"1001", 5}}},
	{"quero 2 1130", []PurchaseItem{{"1130", 2}}},
	{"manda 3 1144", []PurchaseItem{{"1144", 3}}},
	{"me manda 2 1208", []PurchaseItem{{"1208", 2}}},
	{"separa 2 1250", []PurchaseItem{{"1250", 2}}},
	{"reserva 3 do 1130", []PurchaseItem{{"1130", 3}}},
	{"pega 4 1193", []PurchaseItem{{"1193", 4}}},
	{"2x 1130", []PurchaseItem{{"1130", 2}}},
	{"x2 1144", []PurchaseItem{{"1144", 2}}},
	{"3 1213", []PurchaseItem{{"1213", 3}}},
	{"quero 5 1000 e 1005", []PurchaseItem{{"1000", 5}, {"1005", 1}}},
	{"quero 2 unidades", []PurchaseItem{{"", 2}}},

	// Três produtos numa tacada, em cada separador que as pessoas usam. A
	// quebra de linha é a mais comum quando a lista é longa: elas escrevem um
	// por linha.
	{"1130 2x, 1207 6X, 1161 2 X", []PurchaseItem{{"1130", 2}, {"1207", 6}, {"1161", 2}}},
	{"1130 2x 1207 6X 1161 2 X", []PurchaseItem{{"1130", 2}, {"1207", 6}, {"1161", 2}}},
	{"1130 2x\n1207 6X\n1161 2 X", []PurchaseItem{{"1130", 2}, {"1207", 6}, {"1161", 2}}},
	{"1130 2x / 1207 6X / 1161 2 X", []PurchaseItem{{"1130", 2}, {"1207", 6}, {"1161", 2}}},
	{"quero 1130 2x, 1207 6X e 1161 2 X", []PurchaseItem{{"1130", 2}, {"1207", 6}, {"1161", 2}}},
	{"1024x3, 1303x3, 1208 × 4", []PurchaseItem{{"1024", 3}, {"1303", 3}, {"1208", 4}}},

	// ─── Live de 17/08, texto colhido AO VIVO ────────────────────────────────
	//
	// Nos primeiros 13 minutos daquela transmissão o parser antigo levantou 18
	// intenções de compra; UMA era real. As outras 17 estão abaixo, e nenhuma
	// virou pedido só porque não havia produto em destaque marcado — sorte, não
	// proteção.
	//
	// O acento AMPLIA o defeito e isso não tinha aparecido na análise de 16/08:
	// `\b` em Go é ASCII, então o `ã` de "paixão" conta como fronteira e "paix"
	// vira código de quatro caracteres. Idem "ótimo" → "timo".
	{"Como vc está minha linda", nil},
	{"Oii gi, tudo bem?", nil},
	{"Meninas chegou o golden com luzinhas ameeeeei de paixão", nil},
	{"?Quem esta com vc hoje", nil},
	{"Agora ninguém mais segura a Gi, Bora pra Cimaaaaa 🥰", nil},
	{"Achei ótimo", nil},
	{"como funciona  agora...conta pra min", nil},
	{"E o bom do sistema e que ele manda uma mensagem do item que foi p o carrinho", nil},
	{"Aqui é a Kitty, não tô conseguindo ver a live pela minha conta, não aparece 😭", nil},
	{"@jackliborio  agora é  pelo código,  se for mais  de um colocar código  espaço X quantidade", nil},
	{"Eu recebi a árvore Esmeralda e amei 😍", nil},
	{"Flores tem hoje?", nil},
	{"Hoje você mostrará a guirlanda de cipreste  minha flor?", nil},
	{"Essa vilas é qual material??", nil},
	{"Noel", nil},
	{"Resina", nil},
	{"Chegou calendário", nil},
	{"Vilas", nil},
	{"Estamos todas juntas!", nil},
	{"Oi poderosa", nil},
	{"É bem tranquilo", nil},
	{"Bora pra Cimaaaaa", nil},

	// O pedido REAL da noite, e a forma que a audiência está sendo ensinada a
	// usar: "código espaço X quantidade".
	{"1099 X 1", []PurchaseItem{{"1099", 1}}},
}

func TestParsePurchaseItems_CorpusDaLive(t *testing.T) {
	for _, c := range casosDaLive {
		t.Run(c.texto, func(t *testing.T) {
			obtido := ParsePurchaseItems(c.texto)
			if reflect.DeepEqual(obtido, c.esperado) {
				return
			}
			switch {
			case c.esperado == nil:
				t.Errorf("%q virou pedido %+v — falso positivo: cria pedido que "+
					"ninguém fez, some estoque e entra no ERP", c.texto, obtido)
			case obtido == nil:
				t.Errorf("%q não virou pedido — falso negativo: a compradora acha "+
					"que pediu e a venda se perde. Esperava %+v", c.texto, c.esperado)
			default:
				t.Errorf("%q → %+v; esperava %+v", c.texto, obtido, c.esperado)
			}
		})
	}
}

// Guarda o tamanho do corpus. Um teste de tabela pode ser esvaziado sem que
// nada fique vermelho — este falha se isso acontecer.
func TestCorpusDaLive_NaoEncolheu(t *testing.T) {
	const minimo = 300
	if len(casosDaLive) < minimo {
		t.Errorf("o corpus tem %d casos, abaixo do piso de %d", len(casosDaLive), minimo)
	}

	var compras, recusas int
	for _, c := range casosDaLive {
		if c.esperado == nil {
			recusas++
		} else {
			compras++
		}
	}
	if compras < 100 || recusas < 150 {
		t.Errorf("distribuição do corpus: %d compras / %d recusas — a recusa é o "+
			"lado caro e precisa continuar sendo o mais coberto", compras, recusas)
	}
}

// Os três defeitos que a lojista relatou depois da live de 16/08, nomeados.
//
// O corpus acima já cobre cada um deles no meio de outras trezentas frases.
// Estes ficam separados porque são a razão da mudança: se algum voltar, o nome
// do teste diz exatamente o que o cliente vai relatar de novo.
func TestDefeitosRelatadosPelaLojista(t *testing.T) {
	t.Run("valor 1000 não pode virar pedido", func(t *testing.T) {
		if itens := ParsePurchaseItems("valor 1000"); itens != nil {
			t.Errorf("virou pedido %+v — a compradora perguntou o preço e a lojista "+
				"achou o pedido na hora de separar a caixa", itens)
		}
	})

	t.Run("1002x2 é pedido de 2 unidades", func(t *testing.T) {
		itens := ParsePurchaseItems("1002x2")
		if len(itens) != 1 || itens[0].Keyword != "1002" || itens[0].Quantity != 2 {
			t.Errorf("leu %+v; sem espaço entre código e quantidade o comentário era "+
				"perdido inteiro e a venda não acontecia", itens)
		}
	})

	t.Run("1000 5x 1005 3x são dois produtos", func(t *testing.T) {
		itens := ParsePurchaseItems("1000 5x 1005 3x")
		if len(itens) != 2 {
			t.Fatalf("leu %d item(ns): %+v; o segundo produto sumia sem log", len(itens), itens)
		}
		if itens[0] != (PurchaseItem{"1000", 5}) || itens[1] != (PurchaseItem{"1005", 3}) {
			t.Errorf("leu %+v; esperava 1000 x5 e 1005 x3", itens)
		}
	})
}

// "N unidades": pedido sem verbo e sem código, em que a quantidade é o pedido.
func TestPedidoPorUnidades(t *testing.T) {
	for _, c := range []casoDeComentario{
		{"5 unidades por favor", []PurchaseItem{{"", 5}}},
		{"1 unidade", []PurchaseItem{{"", 1}}},
		{"1130 3 unidades", []PurchaseItem{{"1130", 3}}},
	} {
		if itens := ParsePurchaseItems(c.texto); !reflect.DeepEqual(itens, c.esperado) {
			t.Errorf("%q → %+v; esperava %+v", c.texto, itens, c.esperado)
		}
	}
}
