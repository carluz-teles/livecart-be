package checkout

// A cobrança PIX viva do carrinho, e por que a leitura precisa limpar.
//
// Gerar um PIX abre uma cobrança no gateway, e ela continua PAGÁVEL até
// expirar — o comprador que copiou o "copia e cola" pode pagar pelo app do
// banco muito depois de ter mexido no carrinho. Sumir com o QR da tela não
// resolve nada: ele já saiu da tela. O que resolve é cancelar no gateway, e
// para isso é preciso guardar o id cancelável (no Pagar.me a COBRANÇA
// `ch_...`, não a ordem `or_...` que vai em `checkout_id`).
//
// A parte delicada é não cancelar duas vezes. Três mutações podem chegar juntas
// — o comprador tirando um item enquanto o outro navegador dele aumenta a
// quantidade — e cada uma quer invalidar. Se todas lerem o mesmo id, todas
// chamam o gateway; a primeira cancela e as outras recebem erro, que fica no
// log como se o QR tivesse ficado vivo. Ler e limpar na MESMA instrução faz o
// banco escolher um vencedor.

import (
	"context"
	"fmt"
	"math/rand"
	"sync"
	"sync/atomic"
	"testing"
)

func seedCartForPix(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	n := fmt.Sprintf("%d", rand.Int63())

	var storeID, eventID, cartID string
	if err := testPool.QueryRow(ctx,
		`INSERT INTO stores (name, slug) VALUES ('Pix', 'pix-'||$1) RETURNING id::text`,
		n).Scan(&storeID); err != nil {
		t.Fatalf("seed store: %v", err)
	}
	if err := testPool.QueryRow(ctx,
		`INSERT INTO live_events (store_id, status, title, ends_at)
		 VALUES ($1, 'active', 'Pix Live', now() + interval '1 day') RETURNING id::text`,
		storeID).Scan(&eventID); err != nil {
		t.Fatalf("seed event: %v", err)
	}
	if err := testPool.QueryRow(ctx,
		`INSERT INTO carts (event_id, platform_user_id, platform_handle, token, short_id, status, payment_status)
		 VALUES ($1, 'u-'||$2, 'h'||$2, 't-'||$2, (floor(random()*2000000000))::int, 'checkout', 'unpaid')
		 RETURNING id::text`, eventID, n).Scan(&cartID); err != nil {
		t.Fatalf("seed cart: %v", err)
	}
	return cartID
}

// O ciclo básico: grava a cobrança, lê uma vez com o id, lê de novo e não vem
// nada. A segunda leitura vazia é o que impede a segunda chamada ao gateway.
func TestCobrancaPixSaiUmaVezSo(t *testing.T) {
	requireDB(t)
	ctx := context.Background()

	cartID := seedCartForPix(t)

	if err := testRepo.SetCartPixCharge(ctx, cartID, "ch_ABC123", 15990); err != nil {
		t.Fatalf("gravando cobrança: %v", err)
	}

	id, amount, err := testRepo.TakeCartPixCharge(ctx, cartID)
	if err != nil {
		t.Fatalf("primeira leitura: %v", err)
	}
	if id != "ch_ABC123" {
		t.Errorf("id = %q, quero ch_ABC123 — sem ele não há o que cancelar no gateway", id)
	}
	if amount != 15990 {
		t.Errorf("valor = %d, quero 15990 — é o que o QR na mão do comprador cobra, "+
			"e o que precisa aparecer no log quando o cancelamento falha", amount)
	}

	id, _, err = testRepo.TakeCartPixCharge(ctx, cartID)
	if err != nil {
		t.Fatalf("segunda leitura: %v", err)
	}
	if id != "" {
		t.Errorf("segunda leitura devolveu %q — a cobrança seria cancelada duas vezes, "+
			"e a segunda chamada volta em erro do gateway", id)
	}
}

// Carrinho sem PIX gerado é o caso comum: quase toda mutação acontece antes de
// o comprador chegar no pagamento. Tem de ser silêncio, não erro.
func TestCarrinhoSemPixNaoErra(t *testing.T) {
	requireDB(t)

	cartID := seedCartForPix(t)

	id, amount, err := testRepo.TakeCartPixCharge(context.Background(), cartID)
	if err != nil {
		t.Fatalf("carrinho sem cobrança devolveu erro: %v", err)
	}
	if id != "" || amount != 0 {
		t.Errorf("veio (%q, %d) de um carrinho que nunca gerou PIX", id, amount)
	}
}

// Sob concorrência: N mutações simultâneas, um vencedor.
//
// É a garantia que o comentário de `invalidatePendingPix` afirma. Sem o
// `AND pix_charge_id IS NOT NULL` dentro do UPDATE, todas as goroutines leriam o
// mesmo id e o gateway receberia N cancelamentos para a mesma cobrança.
func TestSomenteUmaMutacaoLevaACobranca(t *testing.T) {
	requireDB(t)
	ctx := context.Background()

	cartID := seedCartForPix(t)
	if err := testRepo.SetCartPixCharge(ctx, cartID, "ch_CONCORRENTE", 4200); err != nil {
		t.Fatalf("gravando cobrança: %v", err)
	}

	const mutacoes = 24
	var vencedores int64
	var wg sync.WaitGroup
	largada := make(chan struct{})

	for i := 0; i < mutacoes; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-largada
			id, _, err := testRepo.TakeCartPixCharge(ctx, cartID)
			if err != nil {
				t.Errorf("leitura concorrente: %v", err)
				return
			}
			if id != "" {
				atomic.AddInt64(&vencedores, 1)
			}
		}()
	}
	close(largada)
	wg.Wait()

	if vencedores != 1 {
		t.Errorf("%d mutações levaram a cobrança, quero exatamente 1 — cada excedente "+
			"é uma chamada de cancelamento repetida, que o gateway recusa e que vira "+
			"log de 'QR vivo' sem QR vivo nenhum", vencedores)
	}
}

// Gerar de novo troca a cobrança. O id antigo não pode sobreviver: seria o
// cancelamento apontando para uma cobrança que já não é a da tela.
func TestGerarDeNovoSubstituiACobranca(t *testing.T) {
	requireDB(t)
	ctx := context.Background()

	cartID := seedCartForPix(t)
	if err := testRepo.SetCartPixCharge(ctx, cartID, "ch_PRIMEIRA", 1000); err != nil {
		t.Fatalf("primeira cobrança: %v", err)
	}
	if err := testRepo.SetCartPixCharge(ctx, cartID, "ch_SEGUNDA", 2500); err != nil {
		t.Fatalf("segunda cobrança: %v", err)
	}

	id, amount, err := testRepo.TakeCartPixCharge(ctx, cartID)
	if err != nil {
		t.Fatalf("leitura: %v", err)
	}
	if id != "ch_SEGUNDA" || amount != 2500 {
		t.Errorf("veio (%q, %d), quero (ch_SEGUNDA, 2500) — guardar a antiga faria o "+
			"cancelamento mirar a cobrança errada e deixar viva a que está na tela",
			id, amount)
	}
}
