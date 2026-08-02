package migrationtest

// Teste da migration 000116: motivo de nao entrega em notification_logs (RN-38).
//
// O que precisa ficar provado:
//  1. a coluna nasce NULLABLE — todo historico de envio ja gravado continua
//     valido, e "sem motivo" e o estado normal de uma mensagem entregue;
//  2. status aceita 'undelivered' — a coluna nao tem CHECK (000033), entao o
//     valor novo e aditivo puro; se alguem acrescentar um CHECK depois, este
//     teste avisa antes do primeiro envio nao entregue em producao;
//  3. o indice parcial e sobre undelivered_reason IS NOT NULL, e NAO sobre
//     status = 'undelivered'. E a diferenca que decide se a linha RECUSADA
//     pelo Instagram (que continua 'failed', porque houve tentativa real)
//     aparece ou nao na lista do lojista;
//  4. o down remove coluna e indice sem derrubar a tabela.

import (
	"context"
	"strings"
	"testing"

	"github.com/golang-migrate/migrate/v4"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestMigration000116NotificationUndeliveredReason(t *testing.T) {
	adminURL := mustEnv(t)
	url, cleanup := freshDB(t, adminURL)
	defer cleanup()

	m, err := migrate.New(migrationsURL(t), url)
	if err != nil {
		t.Fatalf("migrate.New: %v", err)
	}
	defer m.Close()

	if err := m.Migrate(115); err != nil {
		t.Fatalf("migrate ate 115: %v", err)
	}

	pool, err := pgxpool.New(context.Background(), url)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer pool.Close()
	ctx := context.Background()

	var storeID string
	if err := pool.QueryRow(ctx,
		`INSERT INTO stores (name, slug) VALUES ('RN38','rn38-116') RETURNING id::text`,
	).Scan(&storeID); err != nil {
		t.Fatalf("seed store: %v", err)
	}

	// Linha ANTERIOR a migration: prova que a coluna nova nao pode ser NOT NULL.
	if _, err := pool.Exec(ctx,
		`INSERT INTO notification_logs (store_id, platform_user_id, notification_type, channel, status)
		 VALUES ($1::uuid, 'ig-1', 'checkout_immediate', 'instagram_dm', 'sent')`, storeID,
	); err != nil {
		t.Fatalf("seed log antigo: %v", err)
	}

	if err := m.Migrate(116); err != nil {
		t.Fatalf("migrate 116: %v", err)
	}

	// 1: a linha antiga sobreviveu, com motivo NULL.
	var nullReasons int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM notification_logs WHERE undelivered_reason IS NULL`,
	).Scan(&nullReasons); err != nil {
		t.Fatalf("contar motivos nulos: %v", err)
	}
	if nullReasons != 1 {
		t.Errorf("linhas com motivo NULL = %d, quero 1 (a coluna precisa ser nullable)", nullReasons)
	}

	// 2: o status novo entra.
	if _, err := pool.Exec(ctx,
		`INSERT INTO notification_logs (store_id, platform_user_id, notification_type, channel, status, undelivered_reason)
		 VALUES ($1::uuid, 'ig-2', 'event_deadline_started', 'instagram_dm', 'undelivered', 'comment_window_expired')`,
		storeID,
	); err != nil {
		t.Fatalf("status 'undelivered' foi rejeitado — algum CHECK novo em status: %v", err)
	}

	// 3: a recusa do Instagram fica 'failed' COM motivo, e o predicado do
	// indice tem de alcanca-la. Um indice sobre status = 'undelivered' deixaria
	// justamente esta linha de fora da lista do lojista.
	if _, err := pool.Exec(ctx,
		`INSERT INTO notification_logs (store_id, platform_user_id, notification_type, channel, status, undelivered_reason, error_message)
		 VALUES ($1::uuid, 'ig-3', 'event_deadline_started', 'instagram_dm', 'failed', 'instagram_rejected', 'status 400, body: (#2534022)')`,
		storeID,
	); err != nil {
		t.Fatalf("seed log recusado: %v", err)
	}

	var pred string
	if err := pool.QueryRow(ctx,
		`SELECT indexdef FROM pg_indexes WHERE indexname = 'idx_notification_logs_undelivered'`,
	).Scan(&pred); err != nil {
		t.Fatalf("indice nao existe: %v", err)
	}
	if !strings.Contains(pred, "undelivered_reason IS NOT NULL") {
		t.Errorf("predicado do indice = %q; precisa ser sobre undelivered_reason IS NOT NULL, senao a linha recusada pelo Instagram sai da lista", pred)
	}

	var listable int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM notification_logs WHERE undelivered_reason IS NOT NULL`,
	).Scan(&listable); err != nil {
		t.Fatalf("contar listaveis: %v", err)
	}
	if listable != 2 {
		t.Errorf("linhas com motivo = %d, quero 2 (a nao tentada E a recusada)", listable)
	}

	// 4: down.
	if err := m.Migrate(115); err != nil {
		t.Fatalf("down para 115: %v", err)
	}
	var cols, idx int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM information_schema.columns
		 WHERE table_name = 'notification_logs' AND column_name = 'undelivered_reason'`,
	).Scan(&cols); err != nil {
		t.Fatalf("checar coluna apos down: %v", err)
	}
	if cols != 0 {
		t.Error("down deixou a coluna para tras")
	}
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM pg_indexes WHERE indexname = 'idx_notification_logs_undelivered'`,
	).Scan(&idx); err != nil {
		t.Fatalf("checar indice apos down: %v", err)
	}
	if idx != 0 {
		t.Error("down deixou o indice para tras")
	}
	// A tabela continua de pe com o historico.
	var rows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM notification_logs`).Scan(&rows); err != nil {
		t.Fatalf("tabela apos down: %v", err)
	}
	if rows != 3 {
		t.Errorf("linhas apos down = %d, quero 3 — o rollback nao reescreve historico de envio", rows)
	}
}
