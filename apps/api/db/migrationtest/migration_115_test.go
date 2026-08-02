package migrationtest

// Teste da migration 000115: a unique da midia vira PARCIAL.
//
// O que precisa ficar provado:
//  1. antes da 000115 a unique global existe e o reuso e IMPOSSIVEL;
//  2. depois, a midia de campanha encerrada pode ser tomada por outra campanha;
//  3. duas campanhas VIVAS na mesma midia continuam proibidas — que e a
//     ambiguidade de roteamento que a D22 existe para impedir;
//  4. reabrir uma campanha encerrada cuja midia ja foi tomada FALHA na
//     constraint, em vez de criar duas campanhas vivas na mesma midia;
//  5. o down recria a unique global.

import (
	"context"
	"strings"
	"testing"

	"github.com/golang-migrate/migrate/v4"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestMigration000115LspMediaUniqueSwap(t *testing.T) {
	adminURL := mustEnv(t)
	url, cleanup := freshDB(t, adminURL)
	defer cleanup()

	m, err := migrate.New(migrationsURL(t), url)
	if err != nil {
		t.Fatalf("migrate.New: %v", err)
	}
	defer m.Close()

	if err := m.Migrate(114); err != nil {
		t.Fatalf("migrate ate 114: %v", err)
	}

	pool, err := pgxpool.New(context.Background(), url)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer pool.Close()
	ctx := context.Background()

	var storeID string
	if err := pool.QueryRow(ctx,
		`INSERT INTO stores (name, slug) VALUES ('Reuso','reuso-115') RETURNING id::text`,
	).Scan(&storeID); err != nil {
		t.Fatalf("seed store: %v", err)
	}

	seedSession := func(title, status string) (string, string) {
		t.Helper()
		var eventID, sessionID string
		if err := pool.QueryRow(ctx,
			`INSERT INTO live_events (store_id, status, title, type) VALUES ($1,$2,$3,'post') RETURNING id::text`,
			storeID, status, title,
		).Scan(&eventID); err != nil {
			t.Fatalf("seed evento %s: %v", title, err)
		}
		if err := pool.QueryRow(ctx,
			`INSERT INTO live_sessions (event_id, status, type, sequence_order) VALUES ($1,$2,'post',1) RETURNING id::text`,
			eventID, status,
		).Scan(&sessionID); err != nil {
			t.Fatalf("seed sessao %s: %v", title, err)
		}
		return eventID, sessionID
	}
	bind := func(sessionID, mediaID string) error {
		_, err := pool.Exec(ctx,
			`INSERT INTO live_session_platforms (session_id, platform, platform_live_id) VALUES ($1,'instagram',$2)`,
			sessionID, mediaID)
		return err
	}

	// Campanha de junho, encerrada, com o post fixado.
	junhoID, junhoSession := seedSession("Junho", "active")
	if err := bind(junhoSession, "post-fixado"); err != nil {
		t.Fatalf("vincular midia a junho: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE live_events SET status='ended' WHERE id = $1::uuid`, junhoID); err != nil {
		t.Fatalf("encerrar junho: %v", err)
	}

	// 1: com a unique global, o mesmo post fixado nao pode ser reaproveitado.
	_, julhoSession := seedSession("Julho", "active")
	if err := bind(julhoSession, "post-fixado"); err == nil {
		t.Fatal("a unique global deixou reusar a midia antes da 000115 — o teste nao esta medindo o que acha")
	}

	// --- aplica a 000115 ---------------------------------------------------
	if err := m.Migrate(115); err != nil {
		t.Fatalf("migrate ate 115: %v", err)
	}

	// 2: agora o post fixado de uma campanha ENCERRADA pode ser reaproveitado.
	if err := bind(julhoSession, "post-fixado"); err != nil {
		t.Fatalf("reuso de midia de campanha encerrada foi barrado: %v", err)
	}

	// 3: duas campanhas VIVAS na mesma midia continuam proibidas.
	_, agostoSession := seedSession("Agosto", "active")
	err = bind(agostoSession, "post-fixado")
	if err == nil {
		t.Fatal("duas campanhas VIVAS na mesma midia — e exatamente a ambiguidade de roteamento que a D22 proibe")
	}
	if !strings.Contains(err.Error(), "uq_lsp_media_in_flight") {
		t.Errorf("erro veio de outra constraint: %v", err)
	}

	// 4: reabrir junho agora falha, porque julho ja tomou a midia. Falhar e a
	// resposta certa — o alternativo seria duas campanhas vivas disputando o
	// mesmo post.
	_, err = pool.Exec(ctx, `UPDATE live_events SET status='active' WHERE id = $1::uuid`, junhoID)
	if err == nil {
		t.Fatal("reabriu a campanha cuja midia ja foi tomada")
	}
	if !strings.Contains(err.Error(), "uq_lsp_media_in_flight") {
		t.Errorf("a reabertura falhou por outro motivo: %v", err)
	}

	// 5: down recria a unique global. Como julho tomou a midia, ha duas linhas
	// com o mesmo platform_live_id — o down TEM de falhar, e essa e a
	// documentacao viva do ponto de nao-retorno mole.
	if err := m.Migrate(114); err == nil {
		t.Error("o down passou com midia ja reaproveitada — a unique global nao poderia ter sido recriada")
	}
}
