package integration

// Ratchet: nenhuma linha NOVA de webhook_events pode nascer afirmando
// "assinatura valida" sem ter conferido nada.
//
// Por que isto merece um teste estrutural em vez de review: webhook_events.
// signature_valid nao e um campo decorativo — e o UNICO lugar consultavel que
// responde "o trafego que estamos recebendo esta assinado corretamente?", e essa
// resposta e o gate do deploy 2 do modo observacao (README secao 5.2, ordem 9):
// o 401 so entra depois de dias de trafego 100% valido. Um `true` fixo faz a
// consulta responder 100% valido POR CONSTRUCAO. Quem olhar o painel liga a
// exigencia e para de aceitar comentario da base inteira — o desastre exato que
// o modo observacao existe para evitar, agora com um painel dizendo que estava
// tudo bem.
//
// Foi assim que o bug entrou: o `true` do Mercado Pago foi copiado para o
// caminho do Instagram, onde a assinatura EXISTE e passou a ser conferida de
// verdade. O comentario ao lado dizia "signature validation could be added"
// enquanto o handler, dois arquivos adiante, ja a computava e jogava fora.
//
// A baseline congela os dois casos legitimamente sem assinatura hoje. Ela so
// pode ENCOLHER: um provider novo com `true` fixo quebra o teste, e um da lista
// que passe a gravar o resultado real tambem — para a lista nao apodrecer.

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// provider -> por que gravar `true` fixo ainda e aceitavel.
//
// Nenhum dos dois e "correto": os dois afirmam que uma verificacao passou onde
// verificacao nenhuma aconteceu. Sao divida conhecida, fora do epico do Evento
// Guarda-Chuva, e estao aqui para ficarem VISIVEIS em vez de virarem paragrafo
// num relatorio.
var hardcodedSignatureBaseline = map[string]string{
	"mercado_pago": "E1: nunca teve validacao de assinatura. O webhook do Pagar.me, na mesma familia, ja grava o resultado real (authValid) — este e o unico de pagamento que ficou para tras.",
	"tiny":         "O Tiny nao assina webhook. `true` aqui significa 'nao se aplica', e e por isso que ele engana: a coluna nao tem como dizer 'nao se aplica'.",
}

func TestNenhumWebhookNovoAfirmaAssinaturaValidaSemConferir(t *testing.T) {
	dir := packageDir(t)

	found := map[string]token.Position{}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("lendo o pacote: %v", err)
	}
	fset := token.NewFileSet()
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, perr := parser.ParseFile(fset, filepath.Join(dir, name), nil, 0)
		if perr != nil {
			t.Fatalf("parse %s: %v", name, perr)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			lit, ok := n.(*ast.CompositeLit)
			if !ok {
				return true
			}
			provider, hardcoded, pos := inspectWebhookLiteral(fset, lit)
			if hardcoded {
				found[provider] = pos
			}
			return true
		})
	}

	for provider, pos := range found {
		if _, allowed := hardcodedSignatureBaseline[provider]; !allowed {
			t.Errorf("%s: o webhook de %q grava signature_valid = true fixo.\n"+
				"Se o provider assina, grave o RESULTADO da conferencia (ver o caminho do Instagram: o handler computa e o valor desce no input).\n"+
				"Se nao assina, adicione-o a hardcodedSignatureBaseline com a razao — e assuma que o painel de assinatura passa a mentir para ele.",
				pos, provider)
		}
	}

	var stale []string
	for provider := range hardcodedSignatureBaseline {
		if _, still := found[provider]; !still {
			stale = append(stale, provider)
		}
	}
	sort.Strings(stale)
	for _, provider := range stale {
		t.Errorf("%q esta na hardcodedSignatureBaseline mas nao grava mais `true` fixo — remova-o da lista. Baseline que nao encolhe vira decoracao.", provider)
	}
}

// inspectWebhookLiteral devolve o provider de um StoreWebhookInput literal e se
// ele fixa SignatureValid em `true`. Casa pelos CAMPOS, nao pelo nome do tipo:
// o literal e escrito sem qualificacao dentro do proprio pacote, e um alias ou
// um rename futuro nao pode fazer a checagem parar de valer em silencio.
func inspectWebhookLiteral(fset *token.FileSet, lit *ast.CompositeLit) (provider string, hardcoded bool, pos token.Position) {
	var sawSignatureField bool
	for _, elt := range lit.Elts {
		kv, ok := elt.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		key, ok := kv.Key.(*ast.Ident)
		if !ok {
			continue
		}
		switch key.Name {
		case "Provider":
			if s, ok := kv.Value.(*ast.BasicLit); ok && s.Kind == token.STRING {
				provider = strings.Trim(s.Value, `"`)
			}
		case "SignatureValid":
			sawSignatureField = true
			if id, ok := kv.Value.(*ast.Ident); ok && id.Name == "true" {
				hardcoded = true
				pos = fset.Position(kv.Pos())
			}
		}
	}
	if !sawSignatureField || provider == "" {
		return "", false, token.Position{}
	}
	return provider, hardcoded, pos
}

// O resultado da assinatura atravessa a FILA, nao a pilha de chamadas: o webhook
// so despacha comment.received (outbox) e quem grava a auditoria e o consumidor,
// noutro processo, depois de um json.Marshal/Unmarshal do input inteiro
// (cmd/http-server/main.go registra exatamente isso).
//
// Se o campo perder a exportacao, ganhar `json:"-"` ou for renomeado so de um
// lado, ele volta a false para TODO comentario — e o painel passa a dizer
// "trafego 100% invalido" para trafego legitimo da Meta. Nao ha erro em lugar
// nenhum nesse cenario: os dois lados compilam. Por isso a travessia tem teste.
func TestResultadoDaAssinaturaSobreviveAoOutbox(t *testing.T) {
	for _, valid := range []bool{true, false} {
		in := ProcessInstagramCommentInput{
			CommentID:      "c-1",
			MediaID:        "m-1",
			RawPayload:     []byte(`{"object":"instagram"}`),
			SignatureValid: valid,
		}
		payload, err := json.Marshal(in)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		var out ProcessInstagramCommentInput
		if err := json.Unmarshal(payload, &out); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if out.SignatureValid != valid {
			t.Errorf("SignatureValid virou %v depois da fila, esperava %v", out.SignatureValid, valid)
		}
		// RawPayload viaja junto porque e o par: a linha de auditoria so e
		// gravada quando ele existe. Um sem o outro grava a coluna errada ou
		// nao grava linha nenhuma.
		if len(out.RawPayload) != len(in.RawPayload) {
			t.Errorf("RawPayload nao sobreviveu: %q", out.RawPayload)
		}
	}
}

func packageDir(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	return dir
}
