package migrationtest

// Teste da CADEIA inteira de migrations, sem dado semeado.
//
// Cada migration nova tem teste proprio que sobe ate a versao anterior e aplica
// so a ultima. Isso nao prova que a cadeia inteira aplica do zero, nem que o
// down de uma nao deixa o schema num estado que impede a proxima subida.
//
// A 000122 (CONTRACT) mudou o que da para provar aqui, e a mudanca E o ponto:
// a partir dela o caminho de volta DEIXA DE EXISTIR. Ela dropa live_events.type
// e event_products, que sao a ORIGEM dos backfills da 000111 e da 000112 — e a
// down dela e um no-op DECLARADO, nao um esquecimento. Consequencia direta:
// descer abaixo da 000122 e subir de novo falha em "column e.type does not
// exist", porque a 000111 tentaria reler uma coluna que nao existe mais.
//
// Um teste que exigisse esse replay estaria exigindo do banco algo que o plano
// decidiu conscientemente nao ter. Entao o up/down/up passou a ser exercido
// sobre o PREFIXO REVERSIVEL, e a irreversibilidade ganhou teste proprio.

import (
	"context"
	"strings"
	"testing"

	"github.com/golang-migrate/migrate/v4"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	epicBaseVersion = 103
	// A ultima versao a partir da qual ainda se volta.
	lastReversibleVersion = 121
	contractVersion       = 122
)

// A cadeia inteira aplica do zero e termina limpa.
func TestMigrationChainUpFromScratch(t *testing.T) {
	adminURL := mustEnv(t)
	url, cleanup := freshDB(t, adminURL)
	defer cleanup()

	m, err := migrate.New(migrationsURL(t), url)
	if err != nil {
		t.Fatalf("migrate.New: %v", err)
	}
	defer m.Close()

	if err := m.Up(); err != nil {
		t.Fatalf("UP completo do zero: %v", err)
	}
	head, dirty, err := m.Version()
	if err != nil {
		t.Fatalf("version depois do up: %v", err)
	}
	if dirty {
		t.Fatalf("schema dirty depois do UP completo (versao %d)", head)
	}
	if head < contractVersion {
		t.Fatalf("HEAD=%d, esperava pelo menos %d", head, contractVersion)
	}
	t.Logf("UP completo ok, HEAD=%d", head)
}

// Up/down/up sobre o prefixo reversivel: e o caminho que um deploy ruim usaria
// enquanto o contract ainda nao subiu. Prova que nenhum down deixa residuo que
// impeca a reaplicacao (indice sobrevivente, tipo nao dropado, trigger orfa).
func TestMigrationChainUpDownUpAteOContract(t *testing.T) {
	adminURL := mustEnv(t)
	url, cleanup := freshDB(t, adminURL)
	defer cleanup()

	m, err := migrate.New(migrationsURL(t), url)
	if err != nil {
		t.Fatalf("migrate.New: %v", err)
	}
	defer m.Close()

	if err := m.Migrate(lastReversibleVersion); err != nil {
		t.Fatalf("UP ate %d: %v", lastReversibleVersion, err)
	}
	if err := m.Migrate(epicBaseVersion); err != nil {
		t.Fatalf("DOWN ate %d: %v", epicBaseVersion, err)
	}
	v, dirty, err := m.Version()
	if err != nil {
		t.Fatalf("version depois do down: %v", err)
	}
	if dirty || v != epicBaseVersion {
		t.Fatalf("depois do down esperava %d limpa, veio %d (dirty=%v)", epicBaseVersion, v, dirty)
	}
	if err := m.Migrate(lastReversibleVersion); err != nil {
		t.Fatalf("UP de novo depois do down: %v", err)
	}
	v, dirty, err = m.Version()
	if err != nil {
		t.Fatalf("version depois do segundo up: %v", err)
	}
	if dirty || v != lastReversibleVersion {
		t.Fatalf("segundo UP devia voltar a %d limpo, veio %d (dirty=%v)", lastReversibleVersion, v, dirty)
	}
}

// O contract e irreversivel, e isso e um CONTRATO — nao um bug a consertar.
//
// Tres coisas ficam provadas: (1) as colunas de nivel de evento sumiram e
// ends_at virou obrigatorio; (2) a down nao finge desfazer nada, entao ninguem
// vai acreditar que rodou um rollback e seguir usando o sistema com uma
// whitelist VAZIA — que, pela regra "lista vazia libera tudo", derrubaria a
// barreira de produtos de todas as lojas em silencio; (3) reaplicar a up
// funciona, porque um deploy que reexecuta a cadeia nao pode quebrar.
func TestContractNaoTemVolta(t *testing.T) {
	adminURL := mustEnv(t)
	url, cleanup := freshDB(t, adminURL)
	defer cleanup()

	m, err := migrate.New(migrationsURL(t), url)
	if err != nil {
		t.Fatalf("migrate.New: %v", err)
	}
	defer m.Close()

	if err := m.Migrate(contractVersion); err != nil {
		t.Fatalf("UP ate o contract: %v", err)
	}

	pool, err := pgxpool.New(context.Background(), url)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer pool.Close()
	ctx := context.Background()

	dropped := []string{"type", "media_id", "media_permalink", "media_thumbnail_url",
		"media_caption", "webhook_active", "current_active_product_id", "processing_paused"}
	countCols := func() int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM information_schema.columns
			 WHERE table_name = 'live_events' AND column_name = ANY($1)`, dropped,
		).Scan(&n); err != nil {
			t.Fatalf("contar colunas: %v", err)
		}
		return n
	}
	countEventProducts := func() int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM information_schema.tables WHERE table_name = 'event_products'`,
		).Scan(&n); err != nil {
			t.Fatalf("contar event_products: %v", err)
		}
		return n
	}

	if got := countCols(); got != 0 {
		t.Errorf("depois do contract sobraram %d colunas de nivel de evento", got)
	}
	if countEventProducts() != 0 {
		t.Error("event_products sobreviveu ao contract")
	}
	// ends_at virou obrigatorio: e ele que garante que nenhum carrinho fica sem
	// prazo, ja que expires_at fica NULL durante a campanha inteira (RN-04).
	var nullable string
	if err := pool.QueryRow(ctx,
		`SELECT is_nullable FROM information_schema.columns
		 WHERE table_name = 'live_events' AND column_name = 'ends_at'`,
	).Scan(&nullable); err != nil {
		t.Fatalf("ler ends_at: %v", err)
	}
	if nullable != "NO" {
		t.Error("ends_at continua nullable — evento sem teto e carrinho sem prazo")
	}

	// A down NAO devolve nada.
	if err := m.Migrate(lastReversibleVersion); err != nil {
		t.Fatalf("down do contract (no-op declarado) falhou: %v", err)
	}
	if got := countCols(); got != 0 {
		t.Errorf("a down do contract recriou %d coluna(s) — ela tem de ser no-op, porque coluna vazia e PIOR que coluna ausente: nao da erro, so responde errado", got)
	}
	if countEventProducts() != 0 {
		t.Error("a down do contract recriou event_products vazia — whitelist vazia libera o catalogo inteiro")
	}

	// E a up reaplica sem erro.
	if err := m.Migrate(contractVersion); err != nil {
		t.Fatalf("reaplicar o contract: %v", err)
	}

	// Subir de novo a partir da base do epico e IMPOSSIVEL, e o teste registra
	// isso explicitamente em vez de deixar alguem descobrir num incidente: a
	// 000111 rele live_events.type para backfillar a sessao, e essa coluna nao
	// existe mais.
	if err := m.Migrate(epicBaseVersion); err != nil {
		t.Fatalf("down ate a base: %v", err)
	}
	err = m.Up()
	if err == nil {
		t.Fatal("o replay completo passou — se isso mudar, o que a 000122 diz sobre irreversibilidade ficou obsoleto e o plano de rollback do epico precisa ser reescrito")
	}
	if !strings.Contains(err.Error(), "e.type does not exist") {
		t.Errorf("o replay falhou por outro motivo: %v", err)
	}
}
