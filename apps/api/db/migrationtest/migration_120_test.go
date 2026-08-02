package migrationtest

// Teste da migration 000120 (CONTRACT) com DADO semeado.
//
// O teste de cadeia prova o schema. Este prova o que acontece com as linhas
// que ja existiam — que e onde uma migration irreversivel machuca.
//
// O que precisa ficar provado:
//  1. evento legado SEM ends_at ganha o maior fim de sessao conhecido. Nao e a
//     data "certa" (ela nao existe para um evento que nunca teve teto) — o que
//     importa e que ele passe a TER uma, porque enquanto ends_at e NULL o
//     carrinho fica sem prazo e o estoque reservado junto;
//  2. evento sem NENHUMA sessao encerrada cai para updated_at, e nao fica NULL;
//  3. evento que JA tinha ends_at nao e reescrito — o backfill nao pode mexer
//     em janela que o lojista configurou;
//  4. depois disso, INSERT sem ends_at e REJEITADO;
//  5. as linhas de session_products sobrevivem ao DROP de event_products (elas
//     sao a whitelist de verdade desde a 000110).

import (
	"context"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestMigration000120ContractDropEventLevel(t *testing.T) {
	adminURL := mustEnv(t)
	url, cleanup := freshDB(t, adminURL)
	defer cleanup()

	m, err := migrate.New(migrationsURL(t), url)
	if err != nil {
		t.Fatalf("migrate.New: %v", err)
	}
	defer m.Close()

	if err := m.Migrate(119); err != nil {
		t.Fatalf("migrate ate 119: %v", err)
	}

	pool, err := pgxpool.New(context.Background(), url)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer pool.Close()
	ctx := context.Background()

	var storeID string
	if err := pool.QueryRow(ctx,
		`INSERT INTO stores (name, slug) VALUES ('Contract','contract-120') RETURNING id::text`,
	).Scan(&storeID); err != nil {
		t.Fatalf("seed store: %v", err)
	}

	seed := func(title string, endsAt *time.Time) string {
		t.Helper()
		var id string
		if err := pool.QueryRow(ctx,
			`INSERT INTO live_events (store_id, status, title, type, ends_at)
			 VALUES ($1::uuid, 'ended', $2, 'multi', $3) RETURNING id::text`,
			storeID, title, endsAt,
		).Scan(&id); err != nil {
			t.Fatalf("seed evento %s: %v", title, err)
		}
		return id
	}

	// 1: legado sem teto, com duas sessoes encerradas. O backfill tem de pegar
	// a MAIOR das duas — a campanha acabou quando a ultima transmissao acabou,
	// nao quando a primeira.
	semTeto := seed("Sem teto", nil)
	primeiraSessao := time.Now().Add(-48 * time.Hour).UTC().Truncate(time.Second)
	ultimaSessao := time.Now().Add(-12 * time.Hour).UTC().Truncate(time.Second)
	for i, at := range []time.Time{primeiraSessao, ultimaSessao} {
		if _, err := pool.Exec(ctx,
			`INSERT INTO live_sessions (event_id, status, sequence_order, type, ended_at)
			 VALUES ($1::uuid, 'ended', $2, 'live', $3)`, semTeto, i+1, at,
		); err != nil {
			t.Fatalf("seed sessao: %v", err)
		}
	}

	// 2: legado sem teto e sem sessao encerrada.
	semSessao := seed("Sem sessao encerrada", nil)

	// 3: janela ja configurada pelo lojista.
	configurado := time.Now().Add(72 * time.Hour).UTC().Truncate(time.Second)
	comTeto := seed("Com teto", &configurado)

	// A whitelist de verdade, para provar que o DROP de event_products nao a leva.
	var sessionID string
	if err := pool.QueryRow(ctx,
		`INSERT INTO live_sessions (event_id, status, sequence_order, type)
		 VALUES ($1::uuid, 'active', 1, 'live') RETURNING id::text`, comTeto,
	).Scan(&sessionID); err != nil {
		t.Fatalf("seed sessao do evento com teto: %v", err)
	}
	var productID string
	if err := pool.QueryRow(ctx,
		`INSERT INTO products (store_id, name, external_source, external_id, keyword, price, stock)
		 VALUES ($1::uuid, 'Vestido', 'manual', 'ext-120', '1201', 2500, 5) RETURNING id::text`, storeID,
	).Scan(&productID); err != nil {
		t.Fatalf("seed produto: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO session_products (session_id, product_id) VALUES ($1::uuid, $2::uuid)`,
		sessionID, productID,
	); err != nil {
		t.Fatalf("seed session_products: %v", err)
	}

	if err := m.Migrate(120); err != nil {
		t.Fatalf("migrate 120: %v", err)
	}

	readEnds := func(id string) time.Time {
		t.Helper()
		var at time.Time
		if err := pool.QueryRow(ctx,
			`SELECT ends_at FROM live_events WHERE id = $1::uuid`, id,
		).Scan(&at); err != nil {
			t.Fatalf("ler ends_at: %v", err)
		}
		return at
	}

	if got := readEnds(semTeto).UTC(); !got.Equal(ultimaSessao) {
		t.Errorf("1: ends_at do legado = %v, queria o fim da ULTIMA sessao (%v)", got, ultimaSessao)
	}
	// 2: qualquer instante serve, menos NULL — o Scan em time.Time ja falharia.
	if readEnds(semSessao).IsZero() {
		t.Error("2: evento sem sessao encerrada ficou sem teto")
	}
	if got := readEnds(comTeto).UTC(); !got.Equal(configurado) {
		t.Errorf("3: o backfill reescreveu a janela configurada pelo lojista: %v, queria %v", got, configurado)
	}

	// 4: sem ends_at o INSERT nao entra mais.
	if _, err := pool.Exec(ctx,
		`INSERT INTO live_events (store_id, status, title) VALUES ($1::uuid, 'active', 'Sem fim')`, storeID,
	); err == nil {
		t.Error("4: evento sem ends_at ainda entra — o NOT NULL nao esta valendo")
	}

	// 5: a whitelist de verdade continua la.
	var whitelisted int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM session_products WHERE session_id = $1::uuid`, sessionID,
	).Scan(&whitelisted); err != nil {
		t.Fatalf("contar session_products: %v", err)
	}
	if whitelisted != 1 {
		t.Errorf("5: session_products apos o contract = %d, quero 1", whitelisted)
	}
}
