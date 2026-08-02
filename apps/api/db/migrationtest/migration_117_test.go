package migrationtest

// Teste das migrations 000117 e 000118: as FKs que orders nunca teve (D23).
//
// O que precisa ficar provado:
//  1. antes da 000117 o banco ACEITA pedido apontando para evento inexistente —
//     e a linha de base, e e o que torna a FK necessaria;
//  2. depois da 000117 esse mesmo INSERT e recusado (NOT VALID vale para linha
//     NOVA desde o primeiro instante);
//  3. a constraint entra NOT VALID e a 000118 e quem a valida — se as duas
//     virassem uma so, a varredura rodaria sob o lock forte do ADD;
//  4. RESTRICT: apagar um evento COM pedido falha, e a violacao aponta para
//     orders_event_id_fkey (live_events), nao para carts. Era esse o ganho
//     inteiro da D23 — a integridade ja existia por tabela;
//  5. apagar um evento SEM pedido continua funcionando, com o CASCADE
//     levando carrinho e sessao. A D23 bloqueia venda, nao rascunho;
//  6. o down devolve o banco ao estado permissivo.

import (
	"context"
	"strings"
	"testing"

	"github.com/golang-migrate/migrate/v4"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestMigration000117OrdersEventStoreFK(t *testing.T) {
	adminURL := mustEnv(t)
	url, cleanup := freshDB(t, adminURL)
	defer cleanup()

	m, err := migrate.New(migrationsURL(t), url)
	if err != nil {
		t.Fatalf("migrate.New: %v", err)
	}
	defer m.Close()

	if err := m.Migrate(116); err != nil {
		t.Fatalf("migrate ate 116: %v", err)
	}

	pool, err := pgxpool.New(context.Background(), url)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer pool.Close()
	ctx := context.Background()

	var storeID string
	if err := pool.QueryRow(ctx,
		`INSERT INTO stores (name, slug) VALUES ('D23','d23-117') RETURNING id::text`,
	).Scan(&storeID); err != nil {
		t.Fatalf("seed store: %v", err)
	}

	// carts.event_id e NOT NULL desde a 000020, entao o carrinho sempre tem
	// evento. O orfao possivel e o do PEDIDO: orders.event_id e um uuid solto.
	var baseEventID string
	if err := pool.QueryRow(ctx,
		`INSERT INTO live_events (store_id, status, title, type, ends_at)
		 VALUES ($1::uuid, 'ended', 'Base', 'single', now()) RETURNING id::text`, storeID,
	).Scan(&baseEventID); err != nil {
		t.Fatalf("seed evento base: %v", err)
	}

	// Um pedido APONTANDO PARA O NADA. So passa porque a FK nao existe.
	//
	// Ele e semeado ANTES da 000117 de proposito: a 000117 e NOT VALID, entao
	// ela nao o rejeita — quem rejeitaria e a 000118. Isso deixa o teste provar
	// as duas metades separadamente, que e a razao de as migrations serem duas.
	var orphanCartID string
	if err := pool.QueryRow(ctx,
		`INSERT INTO carts (event_id, platform_user_id, platform_handle, token, short_id, status, payment_status)
		 VALUES ($1::uuid, 'orfao', '@orfao', 'tok-orfao-117', 1171, 'checkout', 'paid') RETURNING id::text`, baseEventID,
	).Scan(&orphanCartID); err != nil {
		t.Fatalf("seed cart orfao: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO orders (cart_id, short_id, store_id, event_id, status)
		 VALUES ($1::uuid, 1, $2::uuid, gen_random_uuid(), 'paid')`, orphanCartID, storeID,
	); err != nil {
		t.Fatalf("1: sem a FK, pedido orfao TEM de entrar (linha de base): %v", err)
	}

	// A 000117 sozinha nao pode falhar por causa dele — e esse o ponto do NOT VALID.
	if err := m.Migrate(117); err != nil {
		t.Fatalf("3: a 000117 nao pode varrer a tabela (NOT VALID), mas falhou: %v", err)
	}

	// 3: e ela entra NOT VALID, nao validada.
	var convalidated bool
	if err := pool.QueryRow(ctx,
		`SELECT convalidated FROM pg_constraint WHERE conname = 'orders_event_id_fkey'`,
	).Scan(&convalidated); err != nil {
		t.Fatalf("constraint orders_event_id_fkey nao existe: %v", err)
	}
	if convalidated {
		t.Error("3: a constraint entrou VALIDADA na 000117 — a varredura tem de ficar na 000118, fora do lock forte do ADD")
	}

	// 2: linha NOVA ja e recusada, mesmo com a constraint ainda NOT VALID.
	var newCartID string
	if err := pool.QueryRow(ctx,
		`INSERT INTO carts (event_id, platform_user_id, platform_handle, token, short_id, status, payment_status)
		 VALUES ($1::uuid, 'novo', '@novo', 'tok-novo-117', 1172, 'checkout', 'paid') RETURNING id::text`, baseEventID,
	).Scan(&newCartID); err != nil {
		t.Fatalf("seed cart novo: %v", err)
	}
	_, err = pool.Exec(ctx,
		`INSERT INTO orders (cart_id, short_id, store_id, event_id, status)
		 VALUES ($1::uuid, 2, $2::uuid, gen_random_uuid(), 'paid')`, newCartID, storeID,
	)
	if err == nil {
		t.Error("2: pedido com event_id inexistente entrou DEPOIS da 000117 — NOT VALID tem de valer para linha nova")
	}

	// A 000118 varre a tabela e TEM de encontrar o orfao semeado no passo 1.
	// Este e o comportamento que protege producao: a validacao falha alto, com
	// a tabela intacta, em vez de o orfao seguir vivo debaixo de uma FK que
	// jura estar valendo.
	if err := m.Migrate(118); err == nil {
		t.Error("a 000118 passou com pedido orfao na tabela — a varredura nao esta acontecendo")
	} else if !strings.Contains(err.Error(), "orders_event_id_fkey") {
		t.Errorf("a 000118 falhou por outro motivo: %v", err)
	}

	// migrate marca a versao como dirty quando o up falha; limpar para seguir.
	if err := m.Force(117); err != nil {
		t.Fatalf("force 117: %v", err)
	}

	// Removido o orfao, a validacao passa.
	if _, err := pool.Exec(ctx, `DELETE FROM orders WHERE cart_id = $1::uuid`, orphanCartID); err != nil {
		t.Fatalf("limpar orfao: %v", err)
	}
	if err := m.Migrate(118); err != nil {
		t.Fatalf("000118 com a tabela limpa: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`SELECT convalidated FROM pg_constraint WHERE conname = 'orders_event_id_fkey'`,
	).Scan(&convalidated); err != nil {
		t.Fatalf("reler constraint: %v", err)
	}
	if !convalidated {
		t.Error("depois da 000118 a constraint continua NOT VALID")
	}

	// 4 e 5: o comportamento de DELETE, que e o motivo de a D23 existir.
	var eventID string
	if err := pool.QueryRow(ctx,
		`INSERT INTO live_events (store_id, status, title, type, ends_at)
		 VALUES ($1::uuid, 'ended', 'Semana Black', 'single', now()) RETURNING id::text`, storeID,
	).Scan(&eventID); err != nil {
		t.Fatalf("seed evento: %v", err)
	}
	var soldCartID string
	if err := pool.QueryRow(ctx,
		`INSERT INTO carts (event_id, platform_user_id, platform_handle, token, short_id, status, payment_status)
		 VALUES ($1::uuid, 'comprou', '@comprou', 'tok-comprou-117', 1173, 'checkout', 'paid') RETURNING id::text`, eventID,
	).Scan(&soldCartID); err != nil {
		t.Fatalf("seed cart vendido: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO orders (cart_id, short_id, store_id, event_id, status)
		 VALUES ($1::uuid, 3, $2::uuid, $3::uuid, 'paid')`, soldCartID, storeID, eventID,
	); err != nil {
		t.Fatalf("seed pedido: %v", err)
	}

	_, err = pool.Exec(ctx, `DELETE FROM live_events WHERE id = $1::uuid`, eventID)
	if err == nil {
		t.Fatal("4: evento COM pedido foi apagado — o RESTRICT nao esta valendo")
	}
	// O ganho da D23: a violacao fala de live_events, nao de carts. Antes, o
	// unico bloqueio era orders.cart_id (NO ACTION) e o erro citava uma tabela
	// que o lojista nunca ouviu falar.
	if !strings.Contains(err.Error(), "orders_event_id_fkey") {
		t.Errorf("4: o DELETE foi barrado por %v — precisa ser barrado por orders_event_id_fkey, que e o que da mensagem util ao lojista", err)
	}

	// 5: evento SEM pedido continua apagavel, com o CASCADE levando o carrinho.
	var draftID string
	if err := pool.QueryRow(ctx,
		`INSERT INTO live_events (store_id, status, title, type, ends_at)
		 VALUES ($1::uuid, 'active', 'Rascunho', 'single', now() + interval '1 day') RETURNING id::text`, storeID,
	).Scan(&draftID); err != nil {
		t.Fatalf("seed rascunho: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO carts (event_id, platform_user_id, platform_handle, token, short_id, status)
		 VALUES ($1::uuid, 'so-olhou', '@so-olhou', 'tok-rascunho-117', 1174, 'active')`, draftID,
	); err != nil {
		t.Fatalf("seed cart do rascunho: %v", err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM live_events WHERE id = $1::uuid`, draftID); err != nil {
		t.Errorf("5: evento SEM pedido tem de continuar apagavel: %v", err)
	}

	// 6: down devolve o estado permissivo.
	if err := m.Migrate(116); err != nil {
		t.Fatalf("down para 116: %v", err)
	}
	var left int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM pg_constraint WHERE conname IN ('orders_event_id_fkey','orders_store_id_fkey')`,
	).Scan(&left); err != nil {
		t.Fatalf("contar constraints apos down: %v", err)
	}
	if left != 0 {
		t.Errorf("down deixou %d constraint(s) para tras", left)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM live_events WHERE id = $1::uuid`, eventID); err != nil {
		// Continua barrado por orders.cart_id (NO ACTION), que e o estado
		// pre-D23. O que NAO pode e a mensagem citar a constraint dropada.
		if strings.Contains(err.Error(), "orders_event_id_fkey") {
			t.Errorf("6: a constraint dropada ainda esta sendo aplicada: %v", err)
		}
	}
}
