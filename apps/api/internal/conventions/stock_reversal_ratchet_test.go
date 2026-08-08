package conventions

// Catraca do estorno de reserva no ERP.
//
// A sequência "chama o ERP, marca a linha depois" estava copiada em cinco
// lugares, e as cinco perdiam a corrida contra a própria retentativa. Em 08/08
// isso virou estoque de verdade: a marcação da reserva f4590b1f morreu em
// "context deadline exceeded" DEPOIS de o Tiny já ter aceitado a entrada, a
// asynq retentou, e o extrato do Tiny ficou com duas entradas de 2 unidades
// para o mesmo movimento. Um produto com 5 unidades terminou com 7.
//
// O contrato do Tiny torna esse erro invisível de dentro: nós mandamos DELTAS e
// ele guarda SALDO. Delta duplicado não deixa rastro do nosso lado — só o
// extrato dele denuncia, e só se alguém for olhar.
//
// A ordem correta mora num ponto só (erp.ReverseReservationsClaimFirst:
// reivindica a linha, e só quem ganha a corrida chama o ERP). Esta catraca
// existe para não nascer uma sexta cópia com a ordem errada.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// arquivosQuePodemChamarOEstornoDireto são os únicos autorizados a invocar
// ReverseStockReservation sem passar pelo helper.
//
// RATCHET: esta lista só encolhe. Um arquivo novo aqui significa uma cópia nova
// da ordem perigosa, e é exatamente o que o teste existe para impedir.
var arquivosQuePodemChamarOEstornoDireto = map[string]string{
	// O próprio helper — é ele quem faz a chamada depois de reivindicar.
	"erp/reversal_claim.go": "o ponto único; a chamada aqui é a certa",

	// O cliente do Tiny: implementa a operação, não a orquestra.
	"integration/providers/erp/tiny.go": "implementação do provider",

	// A declaração da interface ERPProvider. Não é chamada, é assinatura.
	"integration/providers/types.go": "declaração da interface",

	// Compensação de reserva recém-criada: não existe linha de reserva ainda
	// para reivindicar — a saída acabou de ser feita e está sendo desfeita na
	// mesma chamada, sem retentativa no meio.
	"erp/stock_service.go": "compensação inline de uma saída recém-criada",
}

func TestEstornoDeReservaPassaPeloPontoUnico(t *testing.T) {
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
		if !strings.Contains(string(src), "ReverseStockReservation(") {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		if _, autorizado := arquivosQuePodemChamarOEstornoDireto[rel]; !autorizado {
			infratores = append(infratores, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("varrendo internal/: %v", err)
	}

	for _, f := range infratores {
		t.Errorf("%s chama ReverseStockReservation direto.\n"+
			"Use erp.ReverseReservationsClaimFirst: ela reivindica a reserva ANTES de mover o ERP, "+
			"e é isso que impede a retentativa de mandar a mesma entrada duas vezes.\n"+
			"Se este arquivo realmente precisa da chamada crua, acrescente-o a "+
			"arquivosQuePodemChamarOEstornoDireto com a justificativa.", f)
	}
}

// A lista de exceções não pode apodrecer: entrada que aponta para arquivo
// inexistente ou que deixou de chamar o estorno some daqui.
func TestListaDeExcecoesDoEstornoEstaLimpa(t *testing.T) {
	root := internalRoot(t)

	for rel, motivo := range arquivosQuePodemChamarOEstornoDireto {
		caminho := filepath.Join(root, filepath.FromSlash(rel))
		src, err := os.ReadFile(caminho)
		if err != nil {
			t.Errorf("exceção %q (%s) aponta para arquivo que não existe mais — remova a entrada", rel, motivo)
			continue
		}
		if !strings.Contains(string(src), "ReverseStockReservation(") {
			t.Errorf("exceção %q (%s) não chama mais ReverseStockReservation — remova a entrada, a catraca aperta", rel, motivo)
		}
	}
}
