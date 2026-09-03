package productgroup

// O CADASTRO DE VARIAÇÕES, DE PONTA A PONTA.
//
// Em 03/09/2026, toda criação de grupo em staging devolvia 500:
//
//	inserting variant #1: null value in column "id" of relation "products"
//	violates not-null constraint (SQLSTATE 23502)
//
// `CreateProduct` passou a listar `id` entre as colunas do INSERT quando o
// import manual precisou devolver o id que realmente gravou. Este caminho
// nunca preencheu o campo — e não preencher não cai no DEFAULT da coluna:
// manda NULL explícito, e NULL explícito vence DEFAULT. Nenhum teste pegou
// porque não havia teste de banco neste pacote; os que existiam paravam na
// validação de entrada, antes de qualquer SQL.
//
// Este exercita o caminho inteiro contra Postgres de verdade: grupo, opções,
// valores, variações, imagens e o vínculo variação↔valor.

import (
	"context"
	"fmt"
	"testing"
	"time"

	productdomain "livecart/apps/api/internal/product/domain"
	vo "livecart/apps/api/lib/valueobject"
)

func seedStore(t *testing.T) vo.StoreID {
	t.Helper()
	var id string
	slug := fmt.Sprintf("pg-%d", time.Now().UnixNano())
	if err := testPool.QueryRow(context.Background(),
		`INSERT INTO stores (name, slug) VALUES ('Grupo Teste', $1) RETURNING id::text`, slug,
	).Scan(&id); err != nil {
		t.Fatalf("seed store: %v", err)
	}
	sid, err := vo.NewStoreID(id)
	if err != nil {
		t.Fatalf("store id: %v", err)
	}
	return sid
}

// entradaDaTela é o que o formulário de variações manda: duas opções, quatro
// combinações, sem keyword (o servidor gera) e com imagens.
func entradaDaTela(store vo.StoreID) CreateGroupInput {
	return CreateGroupInput{
		StoreID:        store,
		Name:           "Vestido Midi",
		Description:    "Tecido leve",
		ExternalSource: productdomain.ExternalSourceManual,
		GroupImages:    []string{"https://img/grupo-1.jpg"},
		Options: []OptionRequest{
			{Name: "Cor", Values: []string{"Preto", "Vermelho"}},
			{Name: "Tamanho", Values: []string{"P", "M"}},
		},
		Variants: []VariantRequest{
			{OptionValues: []string{"Preto", "P"}, Price: 12990, Stock: 3},
			{OptionValues: []string{"Preto", "M"}, Price: 12990, Stock: 2,
				Images: []string{"https://img/preto-m.jpg"}},
			{OptionValues: []string{"Vermelho", "P"}, Price: 13990, Stock: 0},
			{OptionValues: []string{"Vermelho", "M"}, Price: 13990, Stock: 5},
		},
	}
}

func TestCriarGrupoDeVariacoes(t *testing.T) {
	requireDB(t)
	ctx := context.Background()

	store := seedStore(t)
	res, err := testSvc.Create(ctx, entradaDaTela(store))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	t.Run("cada variação recebe um id de verdade", func(t *testing.T) {
		// O defeito exato: o id chegava NULL e o INSERT morria. Aqui ele tem
		// que existir, ser único e casar com a linha gravada.
		if len(res.Variants()) != 4 {
			t.Fatalf("esperado 4 variações, veio %d", len(res.Variants()))
		}
		vistos := map[string]struct{}{}
		for _, v := range res.Variants() {
			if v.ID == "" || v.ID == "00000000-0000-0000-0000-000000000000" {
				t.Fatalf("variação %v sem id: %q", v.OptionValues, v.ID)
			}
			if _, dup := vistos[v.ID]; dup {
				t.Fatalf("id repetido entre variações: %s", v.ID)
			}
			vistos[v.ID] = struct{}{}

			var existe bool
			if err := testPool.QueryRow(ctx,
				`SELECT EXISTS (SELECT 1 FROM products WHERE id = $1::uuid)`, v.ID,
			).Scan(&existe); err != nil {
				t.Fatalf("conferindo produto: %v", err)
			}
			if !existe {
				t.Fatalf("o id devolvido não corresponde a linha nenhuma: %s", v.ID)
			}
		}
	})

	t.Run("as variações ficam presas ao grupo", func(t *testing.T) {
		var n int
		if err := testPool.QueryRow(ctx,
			`SELECT count(*) FROM products WHERE group_id = $1::uuid`, res.ID(),
		).Scan(&n); err != nil {
			t.Fatalf("contando: %v", err)
		}
		if n != 4 {
			t.Fatalf("esperado 4 produtos no grupo, veio %d", n)
		}
	})

	t.Run("o nome da variação carrega a combinação", func(t *testing.T) {
		var nome string
		if err := testPool.QueryRow(ctx,
			`SELECT name FROM products WHERE group_id = $1::uuid ORDER BY keyword LIMIT 1`, res.ID(),
		).Scan(&nome); err != nil {
			t.Fatalf("lendo nome: %v", err)
		}
		if nome == "Vestido Midi" {
			t.Fatalf("o nome não distingue a combinação: %q", nome)
		}
	})

	t.Run("keyword sai automática, única e sequencial", func(t *testing.T) {
		vistas := map[string]struct{}{}
		for _, v := range res.Variants() {
			if v.Keyword == "" {
				t.Fatalf("variação %v sem keyword", v.OptionValues)
			}
			if _, dup := vistas[v.Keyword]; dup {
				t.Fatalf("keyword repetida: %s", v.Keyword)
			}
			vistas[v.Keyword] = struct{}{}
		}
	})

	t.Run("cada variação aponta para os dois valores que a definem", func(t *testing.T) {
		for _, v := range res.Variants() {
			var n int
			if err := testPool.QueryRow(ctx,
				`SELECT count(*) FROM product_variant_options WHERE product_id = $1::uuid`, v.ID,
			).Scan(&n); err != nil {
				t.Fatalf("contando vínculos: %v", err)
			}
			if n != 2 {
				t.Fatalf("variação %v: esperado 2 vínculos (Cor+Tamanho), veio %d", v.OptionValues, n)
			}
		}
	})

	t.Run("as imagens sobrevivem", func(t *testing.T) {
		var doGrupo int
		if err := testPool.QueryRow(ctx,
			`SELECT count(*) FROM product_group_images WHERE group_id = $1::uuid`, res.ID(),
		).Scan(&doGrupo); err != nil {
			t.Fatalf("imagens do grupo: %v", err)
		}
		if doGrupo != 1 {
			t.Fatalf("esperado 1 imagem de grupo, veio %d", doGrupo)
		}

		var daVariacao int
		if err := testPool.QueryRow(ctx,
			`SELECT count(*) FROM product_images pi
			 JOIN products p ON p.id = pi.product_id
			 WHERE p.group_id = $1::uuid`, res.ID(),
		).Scan(&daVariacao); err != nil {
			t.Fatalf("imagens da variação: %v", err)
		}
		if daVariacao != 1 {
			t.Fatalf("esperado 1 imagem de variação, veio %d", daVariacao)
		}
	})
}

func TestCriarGrupoNaoDeixaLixoQuandoFalha(t *testing.T) {
	requireDB(t)
	ctx := context.Background()

	store := seedStore(t)
	in := entradaDaTela(store)
	// Combinação repetida: a terceira variação é igual à primeira. A recusa
	// acontece no MEIO do laço, com grupo, opções e duas variações já
	// inseridas — é exatamente o caso em que a transação tem que valer.
	in.Variants[2].OptionValues = []string{"Preto", "P"}

	if _, err := testSvc.Create(ctx, in); err == nil {
		t.Fatal("esperado erro de combinação duplicada")
	}

	var grupos int
	if err := testPool.QueryRow(ctx,
		`SELECT count(*) FROM product_groups WHERE store_id = $1::uuid`, store.String(),
	).Scan(&grupos); err != nil {
		t.Fatalf("contando grupos: %v", err)
	}
	if grupos != 0 {
		t.Fatalf("a transação vazou: %d grupo(s) meio-criado(s) ficaram no banco", grupos)
	}

	var produtos int
	if err := testPool.QueryRow(ctx,
		`SELECT count(*) FROM products WHERE store_id = $1::uuid`, store.String(),
	).Scan(&produtos); err != nil {
		t.Fatalf("contando produtos: %v", err)
	}
	if produtos != 0 {
		t.Fatalf("a transação vazou: %d produto(s) órfão(s) ficaram no banco", produtos)
	}
}

// A keyword da variação continua de onde a loja parou.
//
// `GetMaxKeyword` é `:one` sobre um MAX(), então o sqlc devolve `interface{}`
// e o serviço faz `currentMax, _ := maxKw.(string)`. O `_` é o risco: se o
// driver devolvesse qualquer coisa que não fosse string, a asserção falharia
// EM SILÊNCIO, currentMax cairia no "0999" e a primeira variação nasceria em
// 1000 — batendo de frente com a unique (store_id, keyword) de qualquer loja
// que já tenha produto naquela faixa. O sintoma seria um 500 de chave
// duplicada, e a causa estaria a três camadas de distância.
func TestKeywordDaVariacaoContinuaDeOndeALojaParou(t *testing.T) {
	requireDB(t)
	ctx := context.Background()

	store := seedStore(t)
	if _, err := testPool.Exec(ctx,
		`INSERT INTO products (store_id, name, external_source, keyword, price, stock)
		 VALUES ($1::uuid, 'Produto antigo', 'manual', '2000', 1000, 1)`,
		store.String(),
	); err != nil {
		t.Fatalf("seed produto existente: %v", err)
	}

	res, err := testSvc.Create(ctx, entradaDaTela(store))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	for _, v := range res.Variants() {
		if v.Keyword <= "2000" {
			t.Fatalf("keyword %q não passou do máximo da loja (2000) — a leitura do máximo se perdeu", v.Keyword)
		}
	}
}
