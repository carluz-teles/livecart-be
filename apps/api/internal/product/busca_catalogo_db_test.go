package product

// A busca do catálogo contra o banco de verdade.
//
// O predicado é SQL montado à mão, e o pedido do lojista era exatamente sobre
// ele: achar o produto pelo SKU ou pelo código de barras. Testar a mescla de
// identificadores sem testar a consulta provaria que o valor é GRAVADO e não
// que ele é ENCONTRADO.
//
// Uma consulta só serve as seis telas de busca de produto interno — catálogo,
// seletor de produtos da sessão, upsell do evento, painel de Modo Live, o
// multi-select compartilhado e a adição de item no pedido. Passar aqui é passar
// nas seis.

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"livecart/apps/api/db/sqlc"
	"livecart/apps/api/lib/database"
	"livecart/apps/api/lib/query"
	vo "livecart/apps/api/lib/valueobject"
)

var buscaPool *pgxpool.Pool

func TestMain(m *testing.M) {
	os.Exit(buscaTestMain(m))
}

func buscaTestMain(m *testing.M) int {
	adminURL := os.Getenv("TEST_DATABASE_URL")
	if adminURL == "" {
		return m.Run()
	}

	ctx := context.Background()
	admin, err := pgxpool.New(ctx, adminURL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "TEST_DATABASE_URL inválida: %v\n", err)
		return 1
	}
	defer admin.Close()

	dbName := fmt.Sprintf("lc_busca_test_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+dbName); err != nil {
		fmt.Fprintf(os.Stderr, "criando DB de teste: %v\n", err)
		return 1
	}
	defer func() {
		_, _ = admin.Exec(context.Background(), "DROP DATABASE IF EXISTS "+dbName+" WITH (FORCE)")
	}()

	u, _ := url.Parse(adminURL)
	u.Path = "/" + dbName
	testURL := u.String()

	_, thisFile, _, _ := runtime.Caller(0)
	migrationsPath := filepath.Join(filepath.Dir(thisFile), "..", "..", "db", "migrations")
	if err := database.RunMigrations(testURL, migrationsPath); err != nil {
		fmt.Fprintf(os.Stderr, "migrations: %v\n", err)
		return 1
	}

	pool, err := pgxpool.New(ctx, testURL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "conectando no DB de teste: %v\n", err)
		return 1
	}
	defer pool.Close()
	buscaPool = pool

	return m.Run()
}

func requireBuscaDB(t *testing.T) {
	t.Helper()
	if buscaPool == nil {
		t.Skip("TEST_DATABASE_URL não setada — suba `docker compose up -d postgres` e exporte a URL")
	}
}

// semearCatalogo cria a loja e três produtos com identificadores distintos.
func semearCatalogo(t *testing.T) vo.StoreID {
	t.Helper()
	ctx := context.Background()
	n := time.Now().UnixNano()

	var storeIDText string
	if err := buscaPool.QueryRow(ctx,
		`INSERT INTO stores (name, slug) VALUES ('Busca', 'busca-'||$1) RETURNING id::text`,
		fmt.Sprintf("%d", n),
	).Scan(&storeIDText); err != nil {
		t.Fatalf("semear loja: %v", err)
	}

	produtos := []struct{ nome, keyword, sku, barcode string }{
		{"Toalha de Mesa Natalina", "1001", "PA440450000093", "7891234567895"},
		{"Papai Noel Sentado", "1002", "A0745001", "7899876543210"},
		{"Caneca sem código", "1003", "", ""},
	}
	for _, p := range produtos {
		if _, err := buscaPool.Exec(ctx,
			`INSERT INTO products (store_id, name, external_source, external_id, keyword, price, stock, sku, barcode)
			 VALUES ($1, $2, 'tiny', 'e-'||$3, $3, 1000, 5,
			         NULLIF($4,''), NULLIF($5,''))`,
			storeIDText, p.nome, p.keyword, p.sku, p.barcode,
		); err != nil {
			t.Fatalf("semear produto %s: %v", p.nome, err)
		}
	}

	sid, err := vo.NewStoreID(storeIDText)
	if err != nil {
		t.Fatalf("store id: %v", err)
	}
	return sid
}

func buscar(t *testing.T, storeID vo.StoreID, termo string) []string {
	t.Helper()
	res, err := NewRepository(sqlc.New(buscaPool), buscaPool).List(context.Background(), ListProductsParams{
		StoreID:    storeID,
		Search:     termo,
		Pagination: query.Pagination{Page: 1, Limit: 50},
		Sorting:    query.Sorting{SortBy: "created_at", SortOrder: "desc"},
	})
	if err != nil {
		t.Fatalf("List(%q): %v", termo, err)
	}
	nomes := make([]string, len(res.Products))
	for i, p := range res.Products {
		nomes[i] = p.Name()
	}
	return nomes
}

func contemProduto(lista []string, alvo string) bool {
	for _, n := range lista {
		if n == alvo {
			return true
		}
	}
	return false
}

func TestBuscaDoCatalogoAchaPorSKUeCodigoDeBarras(t *testing.T) {
	requireBuscaDB(t)
	loja := semearCatalogo(t)

	casos := []struct {
		nome   string
		termo  string
		acha   string
		porque string
	}{
		{"SKU inteiro", "PA440450000093", "Toalha de Mesa Natalina",
			"é o código colado da etiqueta, o caminho mais comum"},
		{"SKU em minúsculas", "pa440450000093", "Toalha de Mesa Natalina",
			"o SKU do Tiny mistura caixa e o lojista digita como vier"},
		{"trecho do SKU", "44045", "Toalha de Mesa Natalina",
			"ele lembra o meio do código, não o começo"},
		{"código de barras inteiro", "7899876543210", "Papai Noel Sentado",
			"é o que sai do leitor"},
		{"trecho do código de barras", "98765", "Papai Noel Sentado",
			"digitado à mão da embalagem"},
		{"nome continua funcionando", "toalha", "Toalha de Mesa Natalina",
			"a busca por nome não pode ter regredido"},
		{"keyword continua funcionando", "1002", "Papai Noel Sentado",
			"é o código que a audiência comenta na live"},
	}

	for _, tt := range casos {
		t.Run(tt.nome, func(t *testing.T) {
			achados := buscar(t, loja, tt.termo)
			if !contemProduto(achados, tt.acha) {
				t.Errorf("buscar %q não achou %q (%s) — veio %v",
					tt.termo, tt.acha, tt.porque, achados)
			}
		})
	}
}

// Produto sem SKU e sem código de barras não pode sumir da busca por nome. O
// predicado usa OR com colunas anuláveis, e em SQL `false OR NULL` é NULL — se
// a condição fosse montada errado, exatamente o produto cadastrado à mão
// deixaria de aparecer.
func TestProdutoSemCodigoContinuaAchavelPeloNome(t *testing.T) {
	requireBuscaDB(t)
	loja := semearCatalogo(t)

	achados := buscar(t, loja, "caneca")
	if !contemProduto(achados, "Caneca sem código") {
		t.Errorf("produto sem SKU nem código de barras sumiu da busca por nome: %v", achados)
	}
}

// Termo que não casa com nada continua não trazendo nada — sem isso, um
// predicado frouxo devolveria o catálogo inteiro e pareceria estar funcionando.
func TestBuscaSemCorrespondenciaNaoTrazNada(t *testing.T) {
	requireBuscaDB(t)
	loja := semearCatalogo(t)

	if achados := buscar(t, loja, "zzzzznaoexiste"); len(achados) != 0 {
		t.Errorf("busca sem correspondência devolveu %v", achados)
	}
}

// A busca é escopada na loja: o código de barras de uma loja não acha produto
// de outra.
func TestBuscaPorCodigoNaoAtravessaLojas(t *testing.T) {
	requireBuscaDB(t)
	minha := semearCatalogo(t)
	_ = semearCatalogo(t) // outra loja com os MESMOS códigos

	achados := buscar(t, minha, "7891234567895")
	if len(achados) != 1 {
		t.Errorf("busca por código de barras devolveu %d produto(s); deveria ficar "+
			"na loja do contexto: %v", len(achados), achados)
	}
}
