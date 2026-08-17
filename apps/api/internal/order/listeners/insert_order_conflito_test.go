package listeners

// A materialização da Order estourava `duplicate key value violates unique
// constraint "orders_cart_id_key"` em TODO pagamento — 5 de 5 na live de 16/08,
// já na primeira tentativa (retried: 0).
//
// Não era falta de proteção: OnCartPaid consulta a Order antes de inserir. Era
// check-then-act sob concorrência. O gateway reporta o MESMO pagamento por dois
// objetos, order e charge, com ids diferentes (or_Vveq1nds98Hy9zGw e
// ch_0rYBbLwIE6CwOyxg no carrinho c1ec50cc). O dedup_key do evento usa o
// payment_id, então os dois viram cart.paid distintos, e a fila normal roda com
// 2 workers. Os dois chegaram separados por 7ms: ambos consultaram, nenhum
// achou Order, ambos inseriram.
//
// A checagem prévia continua valendo para o caso comum (evento reentregue mais
// tarde). Quem resolve a corrida é o ON CONFLICT: o perdedor descobre no
// próprio INSERT, sem linha devolvida, e o chamador trata como sucesso.

import (
	"os"
	"strings"
	"testing"
)

func TestInsertOrderToleraCorridaEntreWorkers(t *testing.T) {
	const gerado = "../../../db/sqlc/order_write.sql.go"

	fonte, err := os.ReadFile(gerado)
	if err != nil {
		t.Fatalf("lendo a query gerada: %v", err)
	}

	insert := recorteDaQuery(t, string(fonte), "const insertOrder = ")

	if !strings.Contains(insert, "ON CONFLICT (cart_id) DO NOTHING") {
		t.Error("InsertOrder voltou a inserir sem ON CONFLICT — dois workers " +
			"materializando o mesmo carrinho fazem o perdedor estourar a unique, " +
			"que é erro gravado em toda venda paga")
	}
}

// O ON CONFLICT só resolve a corrida porque devolve ZERO linhas ao perdedor —
// é isso que o chamador lê como "outro worker já materializou". Um DO UPDATE
// devolveria linha e sobrescreveria uma Order pronta.
func TestConflitoNaoSobrescreveOrderExistente(t *testing.T) {
	const gerado = "../../../db/sqlc/order_write.sql.go"

	fonte, err := os.ReadFile(gerado)
	if err != nil {
		t.Fatalf("lendo a query gerada: %v", err)
	}

	insert := recorteDaQuery(t, string(fonte), "const insertOrder = ")

	if strings.Contains(insert, "DO UPDATE") {
		t.Error("InsertOrder usa DO UPDATE: o perdedor da corrida sobrescreveria " +
			"a Order que o vencedor acabou de selar")
	}
}

func recorteDaQuery(t *testing.T, fonte, marcador string) string {
	t.Helper()

	i := strings.Index(fonte, marcador)
	if i < 0 {
		t.Fatalf("query %q não encontrada no arquivo gerado", marcador)
	}
	resto := fonte[i:]
	fim := strings.Index(resto, "`\n")
	if fim < 0 {
		t.Fatalf("não consegui delimitar a query %q", marcador)
	}
	return resto[:fim]
}
