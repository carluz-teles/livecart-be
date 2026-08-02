package migrationtest

// Teste da migration 000106 sobre dados LEGADOS: aplica ate a 000103, semeia
// linhas com os valores que a 000106 tem de converter (0 e a faixa 1..14),
// aplica a 000106 e confere conversao, CHECKs e o down.
//
// Precisa ser separado do harness de pacote porque aquele roda todas as
// migrations no TestMain — nao ha janela para semear dado legado no meio.

import (
	"context"
	"fmt"
	neturl "net/url"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/postgres"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	"github.com/jackc/pgx/v5/pgxpool"
)

// migrationsURL resolve o diretorio de migrations relativo a ESTE arquivo, no
// mesmo padrao do harness de apps/api/internal/billing.
func migrationsURL(t *testing.T) string {
	t.Helper()
	_, thisFile, _, _ := runtime.Caller(0)
	return "file://" + filepath.Join(filepath.Dir(thisFile), "..", "migrations")
}

func mustEnv(t *testing.T) string {
	t.Helper()
	u := os.Getenv("TEST_DATABASE_URL")
	if u == "" {
		t.Skip("TEST_DATABASE_URL nao setada")
	}
	return u
}

// freshDB cria um database descartavel e devolve a URL + cleanup.
func freshDB(t *testing.T, adminURL string) (string, func()) {
	t.Helper()
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, adminURL)
	if err != nil {
		t.Fatalf("admin pool: %v", err)
	}
	name := fmt.Sprintf("lc_mig104_%d", os.Getpid())
	_, _ = admin.Exec(ctx, "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatalf("create database: %v", err)
	}
	u, perr := neturl.Parse(adminURL)
	if perr != nil {
		t.Fatalf("parse TEST_DATABASE_URL: %v", perr)
	}
	u.Path = "/" + name
	url := u.String()
	return url, func() {
		_, _ = admin.Exec(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
		admin.Close()
	}
}

func TestMigration000106OverLegacyData(t *testing.T) {
	adminURL := mustEnv(t)
	url, cleanup := freshDB(t, adminURL)
	defer cleanup()

	m, err := migrate.New(migrationsURL(t), url)
	if err != nil {
		t.Fatalf("migrate.New: %v", err)
	}
	defer m.Close()

	// 1. Sobe ate a versao ANTERIOR a nossa.
	if err := m.Migrate(103); err != nil {
		t.Fatalf("migrate ate 103: %v", err)
	}

	pool, err := pgxpool.New(context.Background(), url)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer pool.Close()
	ctx := context.Background()

	// 2. Semeia exatamente os valores que a migration tem de tratar.
	type seed struct {
		slug string
		mins int
	}
	seeds := []seed{
		{"legacy-zero", 0},     // "sem expiracao" -> 1440
		{"legacy-cinco", 5},    // faixa 1..14     -> 15
		{"legacy-catorze", 14}, // faixa 1..14   -> 15
		{"legacy-trinta", 30},  // ja valido     -> intocado
	}
	storeIDs := map[string]string{}
	for _, s := range seeds {
		var id string
		if err := pool.QueryRow(ctx,
			`INSERT INTO stores (name, slug, cart_expiration_minutes)
			 VALUES ($1, $1, $2) RETURNING id::text`, s.slug, s.mins,
		).Scan(&id); err != nil {
			t.Fatalf("seed store %s: %v", s.slug, err)
		}
		storeIDs[s.slug] = id
	}

	// Evento com override 0 e outro com NULL (herda da loja).
	var evZero, evNull string
	if err := pool.QueryRow(ctx,
		`INSERT INTO live_events (store_id, status, title, cart_expiration_minutes)
		 VALUES ($1, 'ended', 'legado zero', 0) RETURNING id::text`, storeIDs["legacy-trinta"],
	).Scan(&evZero); err != nil {
		t.Fatalf("seed evento zero: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO live_events (store_id, status, title, cart_expiration_minutes)
		 VALUES ($1, 'ended', 'legado null', NULL) RETURNING id::text`, storeIDs["legacy-trinta"],
	).Scan(&evNull); err != nil {
		t.Fatalf("seed evento null: %v", err)
	}

	// 3. Aplica a 000106.
	if err := m.Migrate(106); err != nil {
		t.Fatalf("migrate ate 104: %v", err)
	}

	// 4. Conversoes.
	want := map[string]int{
		"legacy-zero":    1440,
		"legacy-cinco":   15,
		"legacy-catorze": 15,
		"legacy-trinta":  30,
	}
	for slug, exp := range want {
		var got int
		if err := pool.QueryRow(ctx,
			`SELECT cart_expiration_minutes FROM stores WHERE slug = $1`, slug,
		).Scan(&got); err != nil {
			t.Fatalf("ler store %s: %v", slug, err)
		}
		if got != exp {
			t.Errorf("store %s: cart_expiration_minutes = %d, quero %d", slug, got, exp)
		}
	}

	var evZeroMins int
	if err := pool.QueryRow(ctx,
		`SELECT cart_expiration_minutes FROM live_events WHERE id = $1::uuid`, evZero,
	).Scan(&evZeroMins); err != nil {
		t.Fatalf("ler evento zero: %v", err)
	}
	if evZeroMins != 1440 {
		t.Errorf("evento com override 0: %d, quero 1440", evZeroMins)
	}

	var evNullMins *int
	if err := pool.QueryRow(ctx,
		`SELECT cart_expiration_minutes FROM live_events WHERE id = $1::uuid`, evNull,
	).Scan(&evNullMins); err != nil {
		t.Fatalf("ler evento null: %v", err)
	}
	if evNullMins != nil {
		t.Errorf("evento com override NULL virou %d — NULL tem de continuar significando 'herda'", *evNullMins)
	}

	// 5. Default do prazo estendido.
	var ext int
	if err := pool.QueryRow(ctx,
		`SELECT cart_extended_expiration_minutes FROM stores WHERE slug = 'legacy-trinta'`,
	).Scan(&ext); err != nil {
		t.Fatalf("ler prazo estendido: %v", err)
	}
	if ext != 10080 {
		t.Errorf("cart_extended_expiration_minutes = %d, quero 10080 (7 dias)", ext)
	}

	// 6. Os CHECKs seguram de verdade.
	rejects := []struct {
		name string
		sql  string
	}{
		{"loja com 0", `UPDATE stores SET cart_expiration_minutes = 0 WHERE slug = 'legacy-trinta'`},
		{"loja com 14", `UPDATE stores SET cart_expiration_minutes = 14 WHERE slug = 'legacy-trinta'`},
		{"evento com 0", fmt.Sprintf(`UPDATE live_events SET cart_expiration_minutes = 0 WHERE id = '%s'::uuid`, evZero)},
		{"prazo estendido com 0", `UPDATE stores SET cart_extended_expiration_minutes = 0 WHERE slug = 'legacy-trinta'`},
	}
	for _, r := range rejects {
		if _, err := pool.Exec(ctx, r.sql); err == nil {
			t.Errorf("%s: o CHECK deixou passar", r.name)
		}
	}

	// NULL no evento continua aceito.
	if _, err := pool.Exec(ctx,
		fmt.Sprintf(`UPDATE live_events SET cart_expiration_minutes = NULL WHERE id = '%s'::uuid`, evZero),
	); err != nil {
		t.Errorf("NULL no evento devia ser aceito: %v", err)
	}

	// 7. Down remove colunas e CHECKs.
	if err := m.Migrate(103); err != nil {
		t.Fatalf("down para 103: %v", err)
	}
	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM information_schema.columns
		 WHERE table_name IN ('stores','live_events') AND column_name = 'cart_extended_expiration_minutes'`,
	).Scan(&n); err != nil {
		t.Fatalf("checar colunas apos down: %v", err)
	}
	if n != 0 {
		t.Errorf("down deixou %d coluna(s) cart_extended_expiration_minutes para tras", n)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE stores SET cart_expiration_minutes = 0 WHERE slug = 'legacy-trinta'`,
	); err != nil {
		t.Errorf("apos o down o CHECK devia ter sumido: %v", err)
	}
}
