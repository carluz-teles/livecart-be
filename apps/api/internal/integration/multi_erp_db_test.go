package integration

// UM ERP POR LOJA — quem garante, e por que a garantia importa mais do que parece.
//
// A migration 000061 criou `uniq_integrations_store_one_erp` sobre (store_id)
// WHERE type='erp'. Não é "um ERP ATIVO": é uma linha de ERP, ponto. É o banco,
// e não o código, que torna impossível uma loja com Tiny e Bling ao mesmo tempo.
//
// Este teste existe porque a garantia é INVISÍVEL de dentro do Go. Lendo
// GetActiveERPIntegration — `LIMIT 1` sem `ORDER BY` — a leitura natural é "se
// houver dois, o Postgres devolve qualquer um, e pode devolver diferente a cada
// chamada". Isso seria grave: o mesmo carrinho criaria o pedido num ERP e
// tentaria confirmá-lo no outro. A conclusão só está errada por causa de um
// índice declarado em outro arquivo, quatro anos-migration atrás.
//
// Se alguém derrubar aquele índice, este teste cai — e é o único lugar que cairia.
//
//	TEST_DATABASE_URL='postgres://livecart:livecart@localhost:5432/livecart?sslmode=disable' \
//	go test -run TestUmERPPorLoja -v ./apps/api/internal/integration/

import (
	"context"
	"fmt"
	"math/rand"
	"strings"
	"testing"
)

func TestUmERPPorLojaEhGarantidoPeloBanco(t *testing.T) {
	if testPool == nil {
		t.Skip("TEST_DATABASE_URL não definida")
	}
	ctx := context.Background()
	seedSeq++
	n := fmt.Sprintf("%d-%d", seedSeq, rand.Intn(1_000_000))

	var storeID string
	if err := testPool.QueryRow(ctx,
		`INSERT INTO stores (name, slug) VALUES ('Um ERP', 'um-erp-'||$1) RETURNING id::text`,
		n).Scan(&storeID); err != nil {
		t.Fatalf("seed store: %v", err)
	}

	var tinyID string
	if err := testPool.QueryRow(ctx,
		`INSERT INTO integrations (store_id, type, provider, status, credentials, created_at)
		 VALUES ($1,'erp','tiny','active','\x00'::bytea, now() - interval '10 days')
		 RETURNING id::text`, storeID).Scan(&tinyID); err != nil {
		t.Fatalf("seed tiny: %v", err)
	}

	// A SEGUNDA integração de ERP tem de ser recusada pelo BANCO.
	_, err := testPool.Exec(ctx,
		`INSERT INTO integrations (store_id, type, provider, status, credentials, created_at)
		 VALUES ($1,'erp','bling','active','\x00'::bytea, now())`, storeID)
	if err == nil {
		t.Fatal("o banco ACEITOU um segundo ERP na mesma loja — o índice " +
			"uniq_integrations_store_one_erp (migration 000061) caiu, e com ele a " +
			"única coisa que impede um carrinho de nascer num ERP e ser confirmado no outro")
	}
	if !strings.Contains(err.Error(), "uniq_integrations_store_one_erp") {
		t.Errorf("recusou por outro motivo (%v) — a garantia pode ter mudado de dono", err)
	}

	// E a loja continua resolvendo o ERP que ela tem.
	integ, err := testRepo.GetActiveERP(ctx, storeID)
	if err != nil {
		t.Fatalf("GetActiveERP: %v", err)
	}
	if integ.ID != tinyID {
		t.Errorf("resolveu %s, queria %s", integ.ID, tinyID)
	}
}

// Desconectar libera a vaga: sem isso o lojista não teria como TROCAR de ERP.
//
// O índice é sobre (store_id) WHERE type='erp' e ignora o status, então
// desconectar não basta — a linha antiga tem de SAIR. Se um dia alguém trocar
// 'disconnected' por um status em vez de remover a linha, a troca de ERP quebra
// e o erro vai parecer coisa do OAuth.
func TestTrocarDeERPExigeQueALinhaAntigaSaia(t *testing.T) {
	if testPool == nil {
		t.Skip("TEST_DATABASE_URL não definida")
	}
	ctx := context.Background()
	seedSeq++
	n := fmt.Sprintf("%d-%d", seedSeq, rand.Intn(1_000_000))

	var storeID string
	if err := testPool.QueryRow(ctx,
		`INSERT INTO stores (name, slug) VALUES ('Troca', 'troca-'||$1) RETURNING id::text`,
		n).Scan(&storeID); err != nil {
		t.Fatalf("seed store: %v", err)
	}
	if _, err := testPool.Exec(ctx,
		`INSERT INTO integrations (store_id, type, provider, status, credentials)
		 VALUES ($1,'erp','tiny','disconnected','\x00'::bytea)`, storeID); err != nil {
		t.Fatalf("seed tiny: %v", err)
	}

	// Mesmo DESCONECTADO, o Tiny ocupa a vaga.
	_, err := testPool.Exec(ctx,
		`INSERT INTO integrations (store_id, type, provider, status, credentials)
		 VALUES ($1,'erp','bling','active','\x00'::bytea)`, storeID)
	if err == nil {
		t.Fatal("aceitou o Bling com uma linha de Tiny desconectada presente — " +
			"o índice mudou e a regra deixou de ser 'uma linha de ERP por loja'")
	}

	// Removida a linha, a vaga abre.
	if _, err := testPool.Exec(ctx,
		`DELETE FROM integrations WHERE store_id=$1 AND type='erp'`, storeID); err != nil {
		t.Fatalf("removendo o ERP antigo: %v", err)
	}
	if _, err := testPool.Exec(ctx,
		`INSERT INTO integrations (store_id, type, provider, status, credentials)
		 VALUES ($1,'erp','bling','active','\x00'::bytea)`, storeID); err != nil {
		t.Errorf("não consegui conectar o Bling depois de remover o Tiny: %v — "+
			"o lojista ficaria preso no ERP antigo", err)
	}
}
