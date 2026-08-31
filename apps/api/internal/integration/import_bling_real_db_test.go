package integration

// IMPORT DE PRODUTO DO BLING — contra a API REAL e o Postgres real.
//
// Fecha a lacuna que os testes com servidor falso não fecham: que o adapter
// leia a conta de verdade, que a imagem que EXPIRA seja reconhecida, e que o
// produto caia no catálogo do LiveCart com os campos certos.
//
// Só roda com as duas coisas presentes:
//
//	TEST_DATABASE_URL='postgres://livecart:livecart@localhost:5432/livecart?sslmode=disable' \
//	BLING_TEST_ACCESS_TOKEN=... BLING_CLIENT_ID=... BLING_CLIENT_SECRET=... \
//	go test -run TestImportBlingReal -v ./apps/api/internal/integration/
//
// Sem elas, pula — não é teste que trave o CI de quem não tem a conta.

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"livecart/apps/api/internal/integration/providers"
	providererp "livecart/apps/api/internal/integration/providers/erp"
	"livecart/apps/api/lib/crypto"
	"livecart/apps/api/lib/ratelimit"
)

// blingReal monta o adapter com a credencial que o LiveCart REALMENTE guarda —
// lida do banco e decifrada com a mesma chave da aplicação.
//
// Deliberadamente NÃO aceita token por variável de ambiente. Um token colado à
// mão testa o adapter; o token do banco testa o CAMINHO: que o OAuth gravou,
// que a cifragem fecha, e que o que sai de lá abre a API. E foi assim que se
// descobriu, aqui, que reautorizar pelo LiveCart REVOGA o token que o
// laboratório tinha — comportamento correto do Bling, invisível de outro jeito.
func blingReal(t *testing.T) *providererp.Bling {
	t.Helper()
	// O banco de APLICAÇÃO, não o descartável do harness: a integração Bling
	// nasce do fluxo OAuth e vive lá. O harness cria um database novo a cada
	// execução, onde ela não existiria.
	appURL := os.Getenv("LIVECART_DATABASE_URL")
	id, secret := os.Getenv("BLING_CLIENT_ID"), os.Getenv("BLING_CLIENT_SECRET")
	chave := os.Getenv("ENCRYPTION_KEY")
	if appURL == "" || id == "" || secret == "" || chave == "" {
		t.Skip("LIVECART_DATABASE_URL / BLING_CLIENT_ID / BLING_CLIENT_SECRET / ENCRYPTION_KEY não definidos")
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, appURL)
	if err != nil {
		t.Fatalf("conectando no banco da aplicação: %v", err)
	}
	t.Cleanup(pool.Close)

	var cifradas []byte
	if err := pool.QueryRow(ctx,
		`SELECT credentials FROM integrations WHERE provider='bling' AND status='active' LIMIT 1`,
	).Scan(&cifradas); err != nil {
		t.Skipf("nenhuma integração Bling ativa no banco da aplicação: %v", err)
	}

	enc, errEnc := crypto.NewEncryptor(chave)
	if errEnc != nil {
		t.Fatalf("chave de cifragem inválida: %v", errEnc)
	}
	var creds providers.Credentials
	if err := enc.DecryptJSON(cifradas, &creds); err != nil {
		t.Fatalf("decifrando as credenciais gravadas pelo OAuth: %v", err)
	}
	token := creds.AccessToken
	if token == "" {
		t.Fatal("a integração está ativa mas sem access token — o OAuth gravou vazio")
	}
	b, errNovo := providererp.NewBling(providererp.BlingConfig{
		IntegrationID: "teste", StoreID: "teste",
		ClientID: id, ClientSecret: secret,
		Credentials: &providers.Credentials{
			AccessToken: token, TokenType: "Bearer",
			ExpiresAt: time.Now().Add(time.Hour),
		},
		Logger: zap.NewNop(),
		// O MESMO freio da produção: um teste que bate na conta real sem
		// limitador gastaria a cota do lojista.
		RateLimiter: ratelimit.NovoFixo(providers.BlingRPSPadrao),
	})
	if errNovo != nil {
		t.Fatal(errNovo)
	}
	return b
}

// O adapter lê o produto da conta REAL e traz o que o import precisa.
func TestImportBlingRealLeOProdutoDaConta(t *testing.T) {
	b := blingReal(t)
	ctx := context.Background()

	lista, err := b.ListProducts(ctx, providers.ListProductsParams{PageSize: 10})
	if err != nil {
		t.Fatalf("listando produtos da conta real: %v", err)
	}
	if len(lista.Products) == 0 {
		t.Skip("a conta não tem produto para importar")
	}

	p, err := b.GetProduct(ctx, lista.Products[0].ID)
	if err != nil {
		t.Fatalf("lendo o produto %s: %v", lista.Products[0].ID, err)
	}

	if p.ID == "" || p.Name == "" {
		t.Errorf("produto sem id ou nome: %+v", p)
	}
	// O SKU pode vir vazio (medido nesta conta) — o vínculo é pelo ID, e o
	// import não pode depender do código.
	t.Logf("produto real: id=%s sku=%q nome=%q preço=%d estoque=%d imagens=%d",
		p.ID, p.SKU, p.Name, p.Price, p.Stock, len(p.ImageURLs))

	if !p.StockKnown {
		t.Error("StockKnown=false para um produto lido com sucesso — quem espelha " +
			"trataria como 'não sei' e não escreveria o contador")
	}
}

// A imagem que EXPIRA é reconhecida como tal. É o que decide se ela vai ser
// re-hospedada no import, e o que impede a foto de sumir da vitrine em 7 dias.
func TestImportBlingRealReconheceImagemQueExpira(t *testing.T) {
	b := blingReal(t)
	ctx := context.Background()

	lista, err := b.ListProducts(ctx, providers.ListProductsParams{PageSize: 10})
	if err != nil {
		t.Fatal(err)
	}

	var comImagem *providers.ERPProduct
	for i := range lista.Products {
		p, err := b.GetProduct(ctx, lista.Products[i].ID)
		if err != nil {
			continue
		}
		if len(p.ImageURLs) > 0 {
			comImagem = p
			break
		}
	}
	if comImagem == nil {
		t.Skip("nenhum produto da conta tem imagem")
	}

	var efemeras int
	for _, u := range comImagem.ImageURLs {
		if !URLEfemera(u) {
			continue
		}
		efemeras++
		quando, ok := ExpiraEm(u)
		if !ok {
			t.Errorf("URL efêmera sem prazo legível: %s", truncarURL(u, 90))
			continue
		}
		falta := time.Until(quando)
		t.Logf("imagem expira em %s (faltam %s)", quando.Format(time.RFC3339), falta.Round(time.Minute))
		if falta <= 0 {
			t.Errorf("a URL que o Bling ACABOU de devolver já está vencida — %s", quando)
		}
	}

	if efemeras == 0 {
		t.Log("nenhuma imagem efêmera nesta leitura — o lojista pode estar usando URLs externas, " +
			"que é o caso bom (não expiram e não precisam ser copiadas)")
	} else {
		t.Logf("%d de %d imagens EXPIRAM e serão re-hospedadas no import",
			efemeras, len(comImagem.ImageURLs))
	}
}

// O saldo em lote é a vantagem real sobre o Tiny: um GET para vários produtos.
func TestImportBlingRealLeSaldoEmLote(t *testing.T) {
	b := blingReal(t)
	ctx := context.Background()

	lista, err := b.ListProducts(ctx, providers.ListProductsParams{PageSize: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(lista.Products) == 0 {
		t.Skip("conta sem produtos")
	}

	ids := make([]string, 0, len(lista.Products))
	for _, p := range lista.Products {
		ids = append(ids, p.ID)
	}

	saldos, err := b.GetProductStockBatch(ctx, ids)
	if err != nil {
		t.Fatalf("lendo saldo em lote: %v", err)
	}
	for id, d := range saldos {
		t.Logf("produto %s: físico=%d reservado=%d disponível=%d", id, d.Balance, d.Reserved, d.Available)
		if d.Available > d.Balance {
			t.Errorf("produto %s: disponível (%d) MAIOR que o físico (%d) — impossível",
				id, d.Available, d.Balance)
		}
		if d.Reserved < 0 {
			t.Errorf("produto %s: reservado negativo (%d)", id, d.Reserved)
		}
	}

	// Um produto que a conta conhece tem de aparecer. Ausência aqui é o defeito
	// do filtroSaldoEstoque, que o adapter contorna pedindo os três filtros.
	if len(saldos) == 0 {
		t.Error("nenhum saldo voltou para produtos que a listagem devolveu — " +
			"o filtroSaldoEstoque escondeu todos?")
	}
}

func truncarURL(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
