package migrationtest

// Teste da CADEIA inteira de migrations, sem dado semeado.
//
// Por que ele existe: cada migration nova tem teste proprio que sobe ate a
// versao anterior e aplica so a ultima. Isso nao prova que a cadeia inteira
// aplica do zero, nem que o down de uma migration nao deixa o schema num
// estado que impede a proxima subida. Este teste fecha esses dois buracos:
// up completo do zero, down ate a base do epico, up completo de novo.

import (
	"context"
	"testing"

	"github.com/golang-migrate/migrate/v4"
)

const epicBaseVersion = 103

func TestMigrationChainUpDownUp(t *testing.T) {
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
	t.Logf("UP completo ok, HEAD=%d", head)

	// Down ate a base do epico: o caminho de rollback que um deploy ruim usaria.
	if err := m.Migrate(epicBaseVersion); err != nil {
		t.Fatalf("DOWN ate %d: %v", epicBaseVersion, err)
	}
	v, dirty, err := m.Version()
	if err != nil {
		t.Fatalf("version depois do down: %v", err)
	}
	if dirty || v != epicBaseVersion {
		t.Fatalf("depois do down esperava versao %d limpa, veio %d (dirty=%v)", epicBaseVersion, v, dirty)
	}

	// E o up de novo, para provar que o down nao deixou residuo que impeca a
	// reaplicacao (indice sobrevivente, tipo nao dropado, trigger orfa).
	if err := m.Up(); err != nil {
		t.Fatalf("UP de novo depois do down: %v", err)
	}
	v, dirty, err = m.Version()
	if err != nil {
		t.Fatalf("version depois do segundo up: %v", err)
	}
	if dirty || v != head {
		t.Fatalf("segundo UP devia voltar a %d limpo, veio %d (dirty=%v)", head, v, dirty)
	}

	_ = context.Background()
}
