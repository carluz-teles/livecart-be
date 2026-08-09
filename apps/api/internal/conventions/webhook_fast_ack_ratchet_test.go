package conventions

// A borda do webhook responde 200 antes de trabalhar. Este ratchet existe
// porque a regra já foi quebrada duas vezes no mesmo arquivo.
//
// A Meta exige 200 em ≤5s e desinscreve o app após 1 hora de falha contínua
// ("Webhooks Disabled"). O caminho de DM processava tudo dentro do request:
// dois lookups, refresh de token e até dois POSTs à Graph. Com o timeout de
// 30s que o client usava, o pior caso passava de 90 segundos — e em 09/08/2026
// isso foi para produção sem ninguém perceber, porque na média rodava em 1s.
//
// O caminho de comentário já estava certo (grava no outbox e volta). A correção
// deixou os dois simétricos. O que este teste trava é a simetria: nenhum
// handler de webhook do Instagram pode chamar método que faça I/O externo antes
// de responder.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

// Métodos de serviço que fazem trabalho pesado (Graph, ERP, múltiplas queries)
// e por isso pertencem ao consumidor, nunca à borda HTTP.
var proibidosNaBordaDoWebhook = map[string]string{
	"ProcessInstagramComment":       "roda no consumidor via comment.received",
	"HandleMessageReceived":         "roda no consumidor via message.received",
	"SendInstagramDM":               "chamada à Graph — sai do request",
	"SendDirectMessage":             "chamada à Graph — sai do request",
	"ReplyToInstagramComment":       "chamada à Graph — sai do request",
	"PublicReplyToInstagramComment": "chamada à Graph — sai do request",
	"deliverPendingCartOnDM":        "consulta + Graph + escrita — sai do request",
	"processStoryReply":             "pipeline inteiro incluindo ERP — sai do request",
	"ReserveStockInERP":             "chamada ao ERP — sai do request",
}

func TestBordaDoWebhookDoInstagramNaoFazTrabalhoPesado(t *testing.T) {
	caminho := filepath.Join(internalRoot(t), "integration", "instagram_handler.go")

	fset := token.NewFileSet()
	arquivo, err := parser.ParseFile(fset, caminho, nil, 0)
	if err != nil {
		t.Fatalf("parseando %s: %v", caminho, err)
	}

	var achados []string
	ast.Inspect(arquivo, func(n ast.Node) bool {
		chamada, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := chamada.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		motivo, proibido := proibidosNaBordaDoWebhook[sel.Sel.Name]
		if !proibido {
			return true
		}
		pos := fset.Position(sel.Pos())
		achados = append(achados, sel.Sel.Name+" em "+filepath.Base(pos.Filename)+":"+
			itoa(pos.Line)+" — "+motivo)
		return true
	})

	if len(achados) > 0 {
		t.Errorf("a borda do webhook do Instagram voltou a trabalhar antes de responder 200.\n"+
			"A Meta exige resposta em ≤5s e desinscreve o app após 1h de falha contínua.\n"+
			"Despache um evento para o outbox e faça isto no consumidor.\n\n  %s",
			strings.Join(achados, "\n  "))
	}
}

// O despacho tem de existir de verdade — sem ele o teste acima passaria num
// handler que simplesmente não faz nada com a DM, e a DM sumiria em silêncio.
func TestBordaDoWebhookDespachaAMensagem(t *testing.T) {
	caminho := filepath.Join(internalRoot(t), "integration", "instagram_handler.go")

	fset := token.NewFileSet()
	arquivo, err := parser.ParseFile(fset, caminho, nil, 0)
	if err != nil {
		t.Fatalf("parseando %s: %v", caminho, err)
	}

	esperados := map[string]bool{
		"DispatchCommentReceived": false, // comentário
		"DispatchMessageReceived": false, // DM
	}
	ast.Inspect(arquivo, func(n ast.Node) bool {
		if sel, ok := n.(*ast.SelectorExpr); ok {
			if _, quero := esperados[sel.Sel.Name]; quero {
				esperados[sel.Sel.Name] = true
			}
		}
		return true
	})

	for nome, achou := range esperados {
		if !achou {
			t.Errorf("%s não aparece no handler — o webhook responde 200 mas o fato "+
				"não vai para o outbox, e o evento se perde em silêncio", nome)
		}
	}
}

// itoa local para não arrastar strconv só por causa de uma mensagem de erro.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var d []byte
	for n > 0 {
		d = append([]byte{byte('0' + n%10)}, d...)
		n /= 10
	}
	return string(d)
}
