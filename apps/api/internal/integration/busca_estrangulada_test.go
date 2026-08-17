package integration

// A busca de produtos no ERP tinha dois desfechos indistinguíveis para o
// lojista, e eles pedem ações opostas:
//
//	"não achei"            → o produto não existe; procure outro nome
//	"não posso olhar agora" → o produto pode existir; espere e repita
//
// Estrangulada na LISTAGEM, a busca devolvia lista VAZIA com sucesso, para
// fugir do toast de erro interno. A tela então dizia "nada encontrado". Na live
// de 16/08 isso virou ~20 buscas em rajada entre 22:58 e 23:00 — a lojista
// concluindo que o produto não existia e tentando de novo, cada tentativa
// somando pressão no limitador que já estava recusando.
//
// Estrangulada no ENRIQUECIMENTO, o mesmo estado devolvia 503 ERP_THROTTLED com
// o texto certo. Dois caminhos, duas respostas, o mesmo problema.

import (
	"os"
	"strings"
	"testing"

	"livecart/apps/api/lib/httpx"
)

// O código existe para a UI poder tratar o caso sem ler mensagem. Perdê-lo faz
// o erro cair no ramo genérico do front, que é onde ele virava "nada encontrado".
func TestEstrangulamentoTemCodigoProprio(t *testing.T) {
	if httpx.CodeErpThrottled == "" {
		t.Fatal("CodeErpThrottled sumiu — sem código a UI não distingue estrangulamento de vazio")
	}
	if string(httpx.CodeErpThrottled) != "ERP_THROTTLED" {
		t.Errorf("CodeErpThrottled = %q; o front trata por este valor", httpx.CodeErpThrottled)
	}
}

// 503 e não 404/422: o produto pode muito bem existir, e o lojista precisa
// entender que o problema é temporário e dele não é a busca, é a espera.
func TestEstrangulamentoNaListagemNaoDevolveVazioComSucesso(t *testing.T) {
	fonte := lerFonte(t, "service.go")

	trecho := recorteEntre(t, fonte, "if allRateLimited {", "}")

	if contem(trecho, "SearchProductsOutput{Products: []ERPProductResponse{}") {
		t.Error("listagem estrangulada voltou a devolver lista vazia com sucesso — " +
			"a tela diz 'nada encontrado' e o lojista clica de novo, somando " +
			"pressão no limitador que acabou de recusar")
	}
	if !contem(trecho, "CodeErpThrottled") {
		t.Error("listagem estrangulada não devolve ERP_THROTTLED; o caminho do " +
			"enriquecimento devolve, e os dois relatam o mesmo estado")
	}
}

// A outra metade: busca que roda inteira e não acha nada CONTINUA sendo 404.
// Transformar todo vazio em estrangulamento esconderia o caso legítimo de
// produto inexistente e mandaria o lojista esperar por algo que nunca vem.
func TestBuscaVaziaLegitimaContinuaSendo404(t *testing.T) {
	fonte := lerFonte(t, "service.go")

	if !contem(fonte, `httpx.ErrNotFound("Produto não encontrado no ERP")`) {
		t.Error("o 404 de produto inexistente sumiu — o lojista que digitou um " +
			"nome errado ficaria esperando o ERP liberar")
	}
}

// --- utilitários de leitura de fonte ---

func lerFonte(t *testing.T, arquivo string) string {
	t.Helper()
	b, err := os.ReadFile(arquivo)
	if err != nil {
		t.Fatalf("lendo %s: %v", arquivo, err)
	}
	return string(b)
}

func recorteEntre(t *testing.T, fonte, inicio, fim string) string {
	t.Helper()
	i := strings.Index(fonte, inicio)
	if i < 0 {
		t.Fatalf("marcador %q não encontrado", inicio)
	}
	resto := fonte[i:]
	j := strings.Index(resto, "\n\t\t"+fim)
	if j < 0 {
		t.Fatalf("fim do bloco %q não encontrado", inicio)
	}
	return resto[:j]
}

func contem(s, sub string) bool { return strings.Contains(s, sub) }
