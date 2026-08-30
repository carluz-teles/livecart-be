package erp

// O ENSAIO: um carrinho de verdade virando pedido na conta REAL do Bling.
//
// É o teste que fecha a última lacuna da integração. Tudo o mais foi medido
// contra o adaptador isolado ou contra um Bling falso; aqui quem dirige é o
// erp.Service — a mesma máquina de estados que a live usa — falando com a conta
// do lojista. O banco é o dobro (repoSimulado), porque o que está sob prova é o
// ERP, não o Postgres, e o dobro já tem os testes dele.
//
// Escreve na conta real, e por isso é duplamente trancado: só roda com
// BLING_ALLOW_WRITE=1 e sempre CANCELA o que criou. O id do pedido é impresso
// para conferência manual, porque um ensaio que sujasse o ERP do lojista em
// silêncio seria pior do que ensaio nenhum.
//
//	LIVECART_DATABASE_URL=postgres://livecart:livecart@localhost:5432/livecart?sslmode=disable \
//	BLING_CLIENT_ID=... BLING_CLIENT_SECRET=... ENCRYPTION_KEY=... BLING_ALLOW_WRITE=1 \
//	go test -run TestEnsaioBlingReal -v ./apps/api/internal/erp/

import (
	"context"
	"encoding/json"
	"fmt"
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

// blingDaConta monta o adaptador com a credencial que o LiveCart REALMENTE
// guarda — lida do banco da aplicação e decifrada com a chave da aplicação.
//
// Renova e REGRAVA quando o token venceu. Sem isso o ensaio só rodaria na
// janela de 6 horas seguinte a um login manual, o que na prática é nunca.
func blingDaConta(t *testing.T) (*providererp.Bling, func()) {
	t.Helper()

	appURL := os.Getenv("LIVECART_DATABASE_URL")
	id, secret := os.Getenv("BLING_CLIENT_ID"), os.Getenv("BLING_CLIENT_SECRET")
	chave := os.Getenv("ENCRYPTION_KEY")
	if appURL == "" || id == "" || secret == "" || chave == "" {
		t.Skip("LIVECART_DATABASE_URL / BLING_CLIENT_ID / BLING_CLIENT_SECRET / ENCRYPTION_KEY não definidos")
	}
	if os.Getenv("BLING_ALLOW_WRITE") != "1" {
		t.Skip("BLING_ALLOW_WRITE != 1 — este ensaio CRIA pedido na conta real do lojista")
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, appURL)
	if err != nil {
		t.Fatalf("conectando no banco da aplicação: %v", err)
	}

	var integrationID string
	var cifradas []byte
	if err := pool.QueryRow(ctx,
		`SELECT id::text, credentials FROM integrations
		  WHERE provider='bling' AND type='erp' AND status='active' LIMIT 1`,
	).Scan(&integrationID, &cifradas); err != nil {
		pool.Close()
		t.Skipf("nenhuma integração Bling ativa no banco da aplicação: %v", err)
	}

	enc, err := crypto.NewEncryptor(chave)
	if err != nil {
		pool.Close()
		t.Fatalf("chave de cifragem inválida: %v", err)
	}
	var creds providers.Credentials
	if err := enc.DecryptJSON(cifradas, &creds); err != nil {
		pool.Close()
		t.Fatalf("decifrando as credenciais gravadas pelo OAuth: %v", err)
	}

	novo := func(c providers.Credentials) *providererp.Bling {
		b, err := providererp.NewBling(providererp.BlingConfig{
			IntegrationID: integrationID, StoreID: "ensaio",
			ClientID: id, ClientSecret: secret,
			Credentials: &c,
			Logger:      zap.NewNop(),
			// O MESMO freio da produção. Um ensaio sem limitador gastaria a
			// cota de 3 req/s do lojista.
			RateLimiter: ratelimit.NovoFixo(providers.BlingRPSPadrao),
		})
		if err != nil {
			t.Fatal(err)
		}
		return b
	}

	b := novo(creds)
	if err := b.ValidateCredentials(ctx); err != nil {
		t.Logf("token vencido (%v) — renovando", err)
		novas, refErr := b.RefreshToken(ctx)
		if refErr != nil {
			pool.Close()
			t.Skipf("não consegui renovar o token do Bling: %v (reconecte pelo LiveCart)", refErr)
		}
		// Regrava, senão o refresh token rotacionado se perde e a próxima
		// execução falha com um erro que parece outro problema.
		if cif, encErr := enc.EncryptJSON(novas); encErr == nil {
			if _, err := pool.Exec(ctx,
				`UPDATE integrations SET credentials=$1 WHERE id=$2::uuid`, cif, integrationID); err != nil {
				t.Logf("aviso: não regravei as credenciais renovadas: %v", err)
			}
		}
		b = novo(*novas)
		if err := b.ValidateCredentials(ctx); err != nil {
			pool.Close()
			t.Fatalf("token renovado ainda não valida: %v", err)
		}
	}
	return b, pool.Close
}

// produtoEContatoDaConta acha um produto com saldo e um contato que existam de
// verdade. Deliberadamente não cria nem um nem outro: o ensaio deixa o cadastro
// do lojista como encontrou, e só o pedido é descartável.
func produtoEContatoDaConta(t *testing.T, b *providererp.Bling) (produtoID, contatoID string) {
	t.Helper()
	ctx := context.Background()

	lista, err := b.ListProducts(ctx, providers.ListProductsParams{PageSize: 20})
	if err != nil {
		t.Fatalf("listando produtos: %v", err)
	}
	for _, p := range lista.Products {
		det, err := b.GetProductStockDetail(ctx, p.ID)
		if err != nil || det.Available < 1 {
			continue
		}
		produtoID = p.ID
		break
	}
	if produtoID == "" {
		t.Skip("nenhum produto com saldo disponível na conta — o ensaio precisa de uma peça para vender")
	}

	// A busca do Bling exige um termo; não há "liste todos". Alguns termos
	// comuns em nome de pessoa cobrem qualquer cadastro brasileiro real.
	for _, termo := range []string{"a", "e", "i", "o", "consumidor"} {
		contatos, err := b.SearchContacts(ctx, providers.SearchContactsParams{Name: termo})
		if err != nil {
			t.Fatalf("buscando contatos por %q: %v", termo, err)
		}
		if len(contatos) > 0 {
			return produtoID, contatos[0].ContactID
		}
	}
	t.Skip("a conta não tem contato cadastrado — o pedido do Bling exige um")
	return "", ""
}

// O ensaio completo, no modo LOCAL (o padrão do Bling): o comentário NÃO cria
// pedido, o pagamento cria.
func TestEnsaioBlingRealDoComentarioAoPedido(t *testing.T) {
	bling, fechar := blingDaConta(t)
	defer fechar()

	produtoID, contatoID := produtoEContatoDaConta(t, bling)
	t.Logf("ensaiando com produto=%s contato=%s", produtoID, contatoID)

	ctx := context.Background()
	antes, err := bling.GetProductStockDetail(ctx, produtoID)
	if err != nil {
		t.Fatalf("lendo o saldo antes: %v", err)
	}
	t.Logf("saldo ANTES: físico=%d reservado=%d disponível=%d",
		antes.Balance, antes.Reserved, antes.Available)

	// A máquina de estados de verdade, com o ERP de verdade.
	repo := novoRepoSimulado()
	repo.provider = "bling"
	repo.metadata = map[string]any{ChaveModoDeReserva: string(ReservaSomenteLocal)}
	colab := &colabSimulado{erp: bling, repo: repo, contatoID: contatoID}
	svc := NewService(repo, colab, zap.NewNop())
	svc.SetOrderStatusRepository(repo)
	svc.SetWriteLimits(limitesAbertos())

	cartID := fmt.Sprintf("ensaio-%d", time.Now().UnixNano())
	repo.criarCarrinho(cartID, NonWaitlistedCartItem{
		ID: "ci-1", CartID: cartID, ProductID: "p-ensaio", Quantity: 1,
		UnitPrice: 1000, ProductName: "Peça do ensaio",
		ProductExternalID: produtoID, ProductKeyword: "ENSAIO",
	})

	// 1. O COMENTÁRIO. No modo local não pode criar nada no ERP.
	if err := svc.ReserveStockInERP(ctx, "ensaio", cartID, "ev-1", "p-ensaio", 1, 1000, "@ensaio"); err != nil {
		t.Fatalf("comentário: %v", err)
	}
	if c := repo.carrinho(cartID); c.externalOrderID != "" {
		t.Fatalf("o comentário criou o pedido %s — no modo local ele nasce só no pagamento",
			c.externalOrderID)
	}
	t.Log("comentário: nenhum pedido no ERP, como o modo local manda")

	// A LIMPEZA É REGISTRADA ANTES DA CRIAÇÃO, e acha o pedido pelo MARCADOR.
	//
	// A primeira versão registrava o defer depois de ler o id do repositório, e
	// a primeira falha real do ensaio — o pagamento parando na situação — saiu
	// pelo t.Fatalf antes disso, deixando um pedido vivo na conta do lojista.
	// Um ensaio que suja o ERP quando falha é pior do que ensaio nenhum, e o
	// momento em que ele falha é justamente o imprevisível.
	t.Cleanup(func() {
		limpo := context.Background()
		pedidoID, err := bling.FindOrderIDByMarker(limpo, "lc-cart-"+cartID)
		if err != nil {
			t.Errorf("não consegui procurar o pedido do ensaio (marcador lc-cart-%s): %v — confira à mão", cartID, err)
			return
		}
		if pedidoID == "" {
			return // nada foi criado
		}
		if err := bling.SetOrderSituacao(limpo, pedidoID, providers.SituacaoCancelada); err != nil {
			t.Errorf("NÃO consegui cancelar o pedido %s do ensaio: %v — cancele à mão", pedidoID, err)
			return
		}
		t.Logf("limpeza: pedido %s cancelado (apague-o pelo Bling se quiser sumir da lista)", pedidoID)
	})

	// 2. O PAGAMENTO. Aqui o pedido tem de existir, em qualquer modo.
	if err := svc.ConfirmERPOrderPayment(ctx, cartID, "ensaio", nil); err != nil {
		t.Fatalf("confirmando o pagamento: %v", err)
	}
	pedidoID := repo.carrinho(cartID).externalOrderID
	if pedidoID == "" {
		t.Fatal("carrinho PAGO ficou sem pedido no ERP — a venda não existiria para o lojista")
	}
	t.Logf("pedido criado na conta real: %s", pedidoID)

	// 3. O pedido existe MESMO, e com o que mandamos.
	itens, err := bling.GetOrderItems(ctx, pedidoID)
	if err != nil {
		t.Fatalf("relendo os itens do pedido %s: %v", pedidoID, err)
	}
	if len(itens) != 1 || itens[0].Quantity != 1 {
		b, _ := json.Marshal(itens)
		t.Errorf("o pedido voltou com %s, queria 1 item de quantidade 1", b)
	}

	// 4. E o marcador está lá — é por ele que a retomada acha um pedido órfão.
	achado, err := bling.FindOrderIDByMarker(ctx, "lc-cart-"+cartID)
	if err != nil {
		t.Errorf("procurando pelo marcador: %v", err)
	} else if achado != pedidoID {
		t.Errorf("o marcador achou %q, queria %q — sem isso um pedido órfão vira duplicado",
			achado, pedidoID)
	}

	depois, err := bling.GetProductStockDetail(ctx, produtoID)
	if err == nil {
		t.Logf("saldo DEPOIS: físico=%d reservado=%d disponível=%d",
			depois.Balance, depois.Reserved, depois.Available)
		if depois.Balance != antes.Balance {
			t.Errorf("o FÍSICO mudou de %d para %d — criar pedido não pode mexer no físico",
				antes.Balance, depois.Balance)
		}
	}
}
