package product

// O produto que o lojista acrescenta ao pedido pelo painel do ERP entra na loja
// automaticamente — e tem de entrar COMPLETO.
//
// Não é um produto fantasma: ele vai aparecer no carrinho da compradora, no
// checkout e na tela do lojista. Sem keyword ele não pode ser pedido na próxima
// live; sem imagem ele aparece como um retângulo vazio no meio da compra.
//
// Estes testes exercitam o MESMO caminho da importação manual — o
// ProductSyncerAdapter.ImportProduct que a tela chama — porque é ele que o
// reflexo do pedido reusa (integration/reflexo_colaboradores.go).

import (
	"context"
	"fmt"
	"testing"
	"time"

	"go.uber.org/zap"

	"livecart/apps/api/db/sqlc"
	"livecart/apps/api/internal/integration/providers"
	vo "livecart/apps/api/lib/valueobject"
)

func lojaVazia(t *testing.T) vo.StoreID {
	t.Helper()
	var id string
	if err := buscaPool.QueryRow(context.Background(),
		`INSERT INTO stores (name, slug) VALUES ('Import', 'import-'||$1) RETURNING id::text`,
		fmt.Sprintf("%d", time.Now().UnixNano()),
	).Scan(&id); err != nil {
		t.Fatalf("semear loja: %v", err)
	}
	sid, err := vo.NewStoreID(id)
	if err != nil {
		t.Fatalf("store id: %v", err)
	}
	return sid
}

func adaptadorDeImport(t *testing.T) *ProductSyncerAdapter {
	t.Helper()
	return NewProductSyncerAdapter(NewService(NewRepository(sqlc.New(buscaPool), buscaPool), zap.NewNop()))
}

// A keyword é o código que a compradora digita na live. Um produto sem ela não
// pode ser pedido — e um produto que entrou pelo pedido do lojista vai querer ser
// pedido na live seguinte.
func TestProdutoImportadoPeloPedidoRecebeKeyword(t *testing.T) {
	requireBuscaDB(t)
	loja := lojaVazia(t)

	id, err := adaptadorDeImport(t).ImportProduct(context.Background(), loja.String(), "tiny", providers.ERPProduct{
		ID:        "999001",
		Name:      "Produto que o lojista somou ao pedido",
		Price:     4990,
		Stock:     7,
		ImageURL:  "https://cdn.tiny/primeira.jpg",
		ImageURLs: []string{"https://cdn.tiny/primeira.jpg", "https://cdn.tiny/segunda.jpg"},
	})
	if err != nil {
		t.Fatalf("importando: %v", err)
	}

	var keyword, imagem string
	if err := buscaPool.QueryRow(context.Background(),
		`SELECT COALESCE(keyword,''), COALESCE(image_url,'') FROM products WHERE id = $1::uuid`, id,
	).Scan(&keyword, &imagem); err != nil {
		t.Fatalf("lendo o produto: %v", err)
	}
	if keyword == "" {
		t.Error("o produto entrou sem keyword — ninguém consegue pedi-lo na live")
	}
	if imagem != "https://cdn.tiny/primeira.jpg" {
		t.Errorf("imagem = %q, quero a PRIMEIRA dos anexos — sem ela o item aparece "+
			"como retângulo vazio no carrinho", imagem)
	}
}

// As keywords não colidem: cada importação pega a próxima livre da loja.
func TestKeywordsDosImportadosNaoColidem(t *testing.T) {
	requireBuscaDB(t)
	loja := lojaVazia(t)
	ad := adaptadorDeImport(t)

	vistas := map[string]bool{}
	for i := 0; i < 5; i++ {
		id, err := ad.ImportProduct(context.Background(), loja.String(), "tiny", providers.ERPProduct{
			ID:       fmt.Sprintf("99900%d", i),
			Name:     fmt.Sprintf("Produto %d", i),
			Price:    1000,
			ImageURL: "https://cdn.tiny/x.jpg",
		})
		if err != nil {
			t.Fatalf("importando %d: %v", i, err)
		}
		var kw string
		if err := buscaPool.QueryRow(context.Background(),
			`SELECT keyword FROM products WHERE id = $1::uuid`, id).Scan(&kw); err != nil {
			t.Fatalf("lendo keyword: %v", err)
		}
		if vistas[kw] {
			t.Fatalf("keyword %q saiu duas vezes — duas coisas diferentes com o mesmo "+
				"código na live", kw)
		}
		vistas[kw] = true
	}
}

// Produto do ERP sem anexo nenhum entra com imagem vazia, e isso é honesto:
// inventar uma imagem seria pior do que não ter.
func TestProdutoSemAnexoEntraSemImagem(t *testing.T) {
	requireBuscaDB(t)
	loja := lojaVazia(t)

	id, err := adaptadorDeImport(t).ImportProduct(context.Background(), loja.String(), "tiny", providers.ERPProduct{
		ID: "999500", Name: "Sem foto no ERP", Price: 2500,
	})
	if err != nil {
		t.Fatalf("importando: %v", err)
	}
	var keyword, imagem string
	if err := buscaPool.QueryRow(context.Background(),
		`SELECT COALESCE(keyword,''), COALESCE(image_url,'') FROM products WHERE id = $1::uuid`, id,
	).Scan(&keyword, &imagem); err != nil {
		t.Fatalf("lendo o produto: %v", err)
	}
	if keyword == "" {
		t.Error("sem imagem não pode significar sem keyword")
	}
	if imagem != "" {
		t.Errorf("imagem = %q, quero vazio", imagem)
	}
}
