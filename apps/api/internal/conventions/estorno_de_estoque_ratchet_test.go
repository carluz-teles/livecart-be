package conventions

// Catraca do estorno de estoque.
//
// Sobrou UMA operação de estoque no sistema, e ela é perigosa de um jeito que
// não se enxerga lendo o nome: `estornar-estoque`.
//
// Num pedido cujo estoque foi lançado, ela desfaz a baixa — é o que se espera.
// Num pedido que apenas RESERVOU, ela não é no-op: devolve 204 e INFLA a
// reserva pela quantidade do pedido, a cada chamada, sem teto. Medido contra a
// conta real em 26/08/2026, num pedido de 2 unidades:
//
//	reservado 5 → 7 → 9        disponível 1 → −2 → −4
//
// E como o `GET /pedidos/{id}` não conta se o estoque foi lançado, não há como
// saber de antemão em qual dos dois casos se está. A única evidência é a própria
// API recusando a edição com `400 motivosBloqueio: "estoque lançado"`.
//
// Daí a regra, e daí esta catraca: o estorno só pode ser chamado DEPOIS dessa
// recusa. Um estorno especulativo — "vai que estava lançado" — corrompe a
// reserva de forma irreversível, e o estrago só aparece no extrato do ERP.
//
// A catraca anterior policiava `ReverseStockReservation`, o estorno da saída
// manual. Aquela operação deixou de existir junto com o modelo que a usava: hoje
// quem segura a peça é o pedido de venda, não um lançamento avulso de estoque.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// arquivosQuePodemEstornarEstoque são os únicos autorizados a invocar
// ReverseOrderStock.
//
// RATCHET: esta lista só encolhe. Um arquivo novo aqui é um estorno novo, e todo
// estorno novo é uma chance nova de inflar a reserva de alguém.
var arquivosQuePodemEstornarEstoque = map[string]string{
	// O ponto único: a recuperação dentro de applyCartGridToOrder, e SÓ depois
	// de o ERP ter recusado a edição com ErrOrderStockLaunched.
	"erp/order_lifecycle.go": "a recuperação do pedido travado por lançamento manual",

	// A declaração da interface ERPProvider. Não é chamada, é assinatura.
	"integration/providers/types.go": "declaração da interface",

	// O cliente do ERP: implementa a operação, não a orquestra.
	"integration/providers/erp/tiny.go": "implementação do provider",
}

func TestEstornoDeEstoqueSoAconteceNaRecuperacao(t *testing.T) {
	root := internalRoot(t)

	var infratores []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		src, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		if !strings.Contains(string(src), "ReverseOrderStock(") {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		if _, autorizado := arquivosQuePodemEstornarEstoque[rel]; !autorizado {
			infratores = append(infratores, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("varrendo internal/: %v", err)
	}

	for _, f := range infratores {
		t.Errorf("%s chama ReverseOrderStock.\n"+
			"Estornar um pedido que só reservou INFLA a reserva (medido: 5 → 7 → 9 num "+
			"pedido de 2 unidades), e não há como saber de antemão se o estoque foi "+
			"lançado — o GET do pedido não conta.\n"+
			"O único momento seguro é depois de o ERP recusar a edição com "+
			"providers.ErrOrderStockLaunched, que é o que applyCartGridToOrder faz.\n"+
			"Se este arquivo realmente precisa da chamada, acrescente-o a "+
			"arquivosQuePodemEstornarEstoque com a justificativa.", f)
	}
}

// O estorno só é legítimo colado ao erro que o autoriza. Este teste lê o arquivo
// autorizado e exige que a chamada apareça no mesmo trecho de
// ErrOrderStockLaunched — se alguém mover o estorno para outro ponto do arquivo,
// a proximidade se perde e o teste acusa.
func TestEstornoFicaColadoNaRecusaQueOAutoriza(t *testing.T) {
	root := internalRoot(t)
	src, err := os.ReadFile(filepath.Join(root, "erp", "order_lifecycle.go"))
	if err != nil {
		t.Fatalf("lendo order_lifecycle.go: %v", err)
	}
	linhas := strings.Split(string(src), "\n")

	iRecusa, iEstorno := -1, -1
	for i, l := range linhas {
		if strings.Contains(l, "ErrOrderStockLaunched") && strings.Contains(l, "errors.Is") {
			iRecusa = i
		}
		if strings.Contains(l, "ReverseOrderStock(") {
			iEstorno = i
		}
	}
	if iRecusa < 0 {
		t.Fatal("não achei o teste de ErrOrderStockLaunched — sem ele o estorno " +
			"passou a ser especulativo")
	}
	if iEstorno < 0 {
		t.Fatal("não achei a chamada de ReverseOrderStock")
	}
	if iEstorno < iRecusa {
		t.Errorf("o estorno (linha %d) vem ANTES da recusa que o autoriza (linha %d)",
			iEstorno+1, iRecusa+1)
	}
	if iEstorno-iRecusa > 15 {
		t.Errorf("o estorno (linha %d) ficou %d linhas depois da recusa (linha %d) — "+
			"longe demais para se ler que um depende do outro",
			iEstorno+1, iEstorno-iRecusa, iRecusa+1)
	}
}

// A lista de exceções não pode apodrecer.
func TestListaDeExcecoesDoEstornoEstaLimpa(t *testing.T) {
	root := internalRoot(t)

	for rel, motivo := range arquivosQuePodemEstornarEstoque {
		caminho := filepath.Join(root, filepath.FromSlash(rel))
		src, err := os.ReadFile(caminho)
		if err != nil {
			t.Errorf("exceção %q (%s) aponta para arquivo que não existe mais — remova a entrada", rel, motivo)
			continue
		}
		if !strings.Contains(string(src), "ReverseOrderStock(") {
			t.Errorf("exceção %q (%s) não chama mais ReverseOrderStock — remova a entrada, a catraca aperta", rel, motivo)
		}
	}
}

// A operação que sumiu não pode voltar. `POST /estoque` era a saída manual que
// baixava o saldo FÍSICO a cada comentário da live, disparava o webhook de
// estoque de volta na nossa própria fila e deixava reserva órfã sempre que
// qualquer passo falhava no meio.
func TestMovimentoManualDeEstoqueNaoRessuscita(t *testing.T) {
	root := internalRoot(t)

	// Assinaturas inequívocas. `ReserveStock` sozinho não serve: existe um
	// CartSettings.ReserveStock() no domínio de loja que é outra coisa — a
	// configuração de "este carrinho reserva estoque?" —, e casar com ele
	// transformaria a catraca em ruído.
	proibidos := []string{
		"ReserveStock(ctx",
		"ReverseStockReservation(",
		"LaunchOrderStock(",
		"lancar-estoque",
	}
	// A drenagem é a exceção com prazo: ela devolve as saídas manuais que já
	// existiam no instante do corte, e para isso precisa da entrada tipo E. Sai
	// junto com a tabela stock_reservations. Ver erp/drenagem.go.
	daDrenagem := map[string]bool{
		"erp/drenagem.go":                            true,
		"integration/providers/erp/tiny.go":          true,
		"integration/providers/types.go":             true,
		"integration/erp_order_status_repository.go": true,
		"integration/drenagem_handler.go":            true,
	}
	var infratores []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		// Testes ficam de fora: o simulador do ERP implementa o lançamento de
		// propósito, para poder encenar o LOJISTA lançando pelo painel — que é o
		// cenário que trava a edição do pedido e é preciso cobrir.
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		src, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		// Só CÓDIGO. Um comentário que descreve a medição antiga — "3×
		// lancar-estoque simultâneos devolveram 204/400/204" — é justamente o
		// registro de por que a operação saiu, e uma catraca que o proíbe apaga
		// a memória em vez de proteger o sistema.
		codigo := semComentarios(string(src))
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		if daDrenagem[rel] {
			return nil
		}
		for _, p := range proibidos {
			if strings.Contains(codigo, p) {
				infratores = append(infratores, rel+" → "+strings.TrimSuffix(p, "("))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("varrendo internal/: %v", err)
	}
	for _, f := range infratores {
		t.Errorf("%s.\n"+
			"Reserva manual de estoque e lançamento de estoque saíram do sistema. "+
			"Quem segura a peça é o PEDIDO DE VENDA, criado no primeiro comentário; a "+
			"baixa física é o faturamento do lojista, não nossa. Lançar durante a live "+
			"baixa o saldo e trava toda edição seguinte do pedido.", f)
	}
}

// semComentarios devolve o fonte sem comentários de linha nem de bloco, para as
// catracas casarem contra código e não contra prosa. Não é um lexer de Go: não
// entende comentário dentro de string literal, o que aqui não muda nada — as
// assinaturas procuradas não aparecem em literais.
func semComentarios(src string) string {
	var b strings.Builder
	b.Grow(len(src))
	for i := 0; i < len(src); {
		switch {
		case strings.HasPrefix(src[i:], "//"):
			j := strings.IndexByte(src[i:], '\n')
			if j < 0 {
				return b.String()
			}
			i += j
		case strings.HasPrefix(src[i:], "/*"):
			j := strings.Index(src[i+2:], "*/")
			if j < 0 {
				return b.String()
			}
			i += j + 4
		default:
			b.WriteByte(src[i])
			i++
		}
	}
	return b.String()
}
