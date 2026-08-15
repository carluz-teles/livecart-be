package live

// A whitelist da transmissão confia na FK, e a FK só garante que o produto
// EXISTE — não que ele é do lojista que está pedindo.
//
// Enquanto os produtos entravam um a um, por rota autenticada e ancorada na
// sessão, o buraco era teórico. A criação de sessão passou a aceitar a lista
// inteira de uma vez (para o painel poder escolher os produtos no mesmo passo),
// e aí um uuid copiado de outra loja entraria na lista de venda desta.

import (
	"context"
	"fmt"
	"math/rand"
	"testing"
)

// seedLojaComProduto cria uma loja e um produto dela, devolvendo os dois ids.
func seedLojaComProduto(t *testing.T) (storeID, productID string) {
	t.Helper()
	ctx := context.Background()
	n := fmt.Sprintf("%d", rand.Int63())

	if err := testPool.QueryRow(ctx,
		`INSERT INTO stores (name, slug) VALUES ('Posse', 'posse-'||$1) RETURNING id::text`,
		n).Scan(&storeID); err != nil {
		t.Fatalf("seed store: %v", err)
	}
	if err := testPool.QueryRow(ctx,
		`INSERT INTO products (store_id, name, external_source, external_id, keyword, price, stock)
		 VALUES ($1, 'P '||$2, 'manual', 'EXT-'||$2, $3, 1000, 5) RETURNING id::text`,
		storeID, n, fmt.Sprintf("K%03d", rand.Intn(1000))).Scan(&productID); err != nil {
		t.Fatalf("seed product: %v", err)
	}
	return storeID, productID
}

func TestProdutoDeOutraLojaNaoPassa(t *testing.T) {
	requireDB(t)
	ctx := context.Background()

	minhaLoja, meuProduto := seedLojaComProduto(t)
	_, produtoAlheio := seedLojaComProduto(t)

	t.Run("produto proprio passa", func(t *testing.T) {
		ok, err := testRepo.AllProductsBelongToStore(ctx, minhaLoja, []string{meuProduto})
		if err != nil {
			t.Fatalf("erro: %v", err)
		}
		if !ok {
			t.Error("recusou um produto que é da própria loja")
		}
	})

	t.Run("produto de outra loja e recusado", func(t *testing.T) {
		ok, err := testRepo.AllProductsBelongToStore(ctx, minhaLoja, []string{produtoAlheio})
		if err != nil {
			t.Fatalf("erro: %v", err)
		}
		if ok {
			t.Error("aceitou produto de OUTRA loja — ele entraria na lista de venda " +
				"desta transmissão, e a FK não impede porque o produto existe")
		}
	})

	t.Run("um alheio no meio dos proprios contamina a lista inteira", func(t *testing.T) {
		// O caso realista: o lojista manda a lista dele e um id vem errado.
		// Aceitar os válidos e descartar o resto criaria a transmissão vendendo
		// algo diferente do que ele escolheu, em silêncio.
		ok, err := testRepo.AllProductsBelongToStore(ctx, minhaLoja,
			[]string{meuProduto, produtoAlheio})
		if err != nil {
			t.Fatalf("erro: %v", err)
		}
		if ok {
			t.Error("aceitou a lista com um produto alheio no meio")
		}
	})

	t.Run("lista vazia passa", func(t *testing.T) {
		// Vazia significa "vende qualquer produto ativo da loja". Não há o que
		// conferir, e recusar aqui quebraria toda sessão criada sem seleção —
		// que é como todas nasciam antes.
		ok, err := testRepo.AllProductsBelongToStore(ctx, minhaLoja, nil)
		if err != nil {
			t.Fatalf("erro: %v", err)
		}
		if !ok {
			t.Error("recusou lista vazia")
		}
	})

	t.Run("uuid malformado e recusado sem ir ao banco", func(t *testing.T) {
		ok, err := testRepo.AllProductsBelongToStore(ctx, minhaLoja, []string{"nao-sou-uuid"})
		if err != nil {
			t.Fatalf("id malformado devolveu erro em vez de recusa: %v", err)
		}
		if ok {
			t.Error("aceitou um id que não é uuid")
		}
	})

	t.Run("uuid valido que nao existe e recusado", func(t *testing.T) {
		// Sem a checagem, este caso derrubava a transação na FK e virava 500 —
		// "erro interno" para um pedido que é só inválido.
		ok, err := testRepo.AllProductsBelongToStore(ctx, minhaLoja,
			[]string{"00000000-0000-0000-0000-000000000000"})
		if err != nil {
			t.Fatalf("erro: %v", err)
		}
		if ok {
			t.Error("aceitou um produto que não existe")
		}
	})
}
