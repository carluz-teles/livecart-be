package integration

// O deadlock do advisory lock de finalização — hold-and-wait no pool único.
//
// Quem segura o lock segura uma conexão (advisory lock é por sessão de
// Postgres) e, com ela na mão, pede uma SEGUNDA conexão do MESMO pool para cada
// query que roda sob o lock. Quando os detentores simultâneos chegam a
// MaxConns, todas as conexões estão presas por quem espera por mais uma:
// pgxpool.Acquire bloqueia sem prazo e o ctx desses caminhos é
// context.Background(). Trava permanente — e não só da fila, porque o pool é
// único para a API inteira.
//
// Era assim que TestScaleConcurrentReleasesExactCount travava. Este teste ataca
// a mecânica direto, sem depender do cenário de fila: carts DISTINTOS (nenhuma
// disputa de lock entre eles), muito mais detentores do que conexões, e cada um
// pedindo a segunda conexão sob o lock. Sem o semáforo isso não termina.

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestCartFinalisationLockDoesNotExhaustThePool(t *testing.T) {
	requireDB(t)
	ctx := context.Background()

	maxConns := int(testPool.Config().MaxConns)
	// Bem acima de MaxConns: é exatamente a condição que travava.
	workers := maxConns * 4

	var completed atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Cada goroutine trabalha num cart diferente: todas ADQUIREM o
			// lock, nenhuma perde por contenção. É o pior caso.
			release, acquired, err := testRepo.AcquireCartFinalisationLock(ctx, uuid.NewString())
			if err != nil {
				t.Errorf("AcquireCartFinalisationLock: %v", err)
				return
			}
			if !acquired {
				t.Error("carts distintos não podem disputar o mesmo advisory lock")
				return
			}
			defer release()

			// A segunda conexão, sob o lock — o que toda query do trecho
			// crítico faz (GetCartExpirySnapshot, GetProductByID, a
			// finalização ERP inteira).
			conn, err := testRepo.pool.Acquire(ctx)
			if err != nil {
				t.Errorf("segunda conexão sob o lock: %v", err)
				return
			}
			var one int
			_ = conn.QueryRow(ctx, "SELECT 1").Scan(&one)
			conn.Release()

			completed.Add(1)
		}()
	}

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(60 * time.Second):
		t.Fatalf("pool esgotado: %d de %d finalizações terminaram em 60s (MaxConns=%d)",
			completed.Load(), workers, maxConns)
	}

	if got := completed.Load(); got != int64(workers) {
		t.Errorf("terminaram %d de %d", got, workers)
	}
}

// O teto tem de sobrar metade do pool para o resto da API — o pool é
// compartilhado com todo handler HTTP. E nunca pode ser 0, senão nenhuma
// finalização roda.
func TestCartFinalisationLockSlots(t *testing.T) {
	cases := map[int32]int{
		1:  1, // piso: metade de 1 é 0, e 0 travaria tudo
		2:  1,
		8:  4,
		10: 5, // produção
		24: 12,
	}
	for maxConns, want := range cases {
		cfg, err := pgxpool.ParseConfig("postgres://u:p@localhost:5432/db?sslmode=disable")
		if err != nil {
			t.Fatalf("ParseConfig: %v", err)
		}
		cfg.MaxConns = maxConns
		// Sem conectar: só a configuração interessa.
		pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
		if err != nil {
			t.Fatalf("NewWithConfig: %v", err)
		}
		got := cartFinalisationLockSlots(pool)
		pool.Close()
		if got != want {
			t.Errorf("MaxConns=%d → slots=%d, quero %d", maxConns, got, want)
		}
	}

	// Repositório sem pool não pode devolver 0: um canal de capacidade 0
	// bloquearia para sempre na primeira finalização.
	if got := cartFinalisationLockSlots(nil); got < 1 {
		t.Errorf("sem pool → slots=%d, tem de ser >= 1", got)
	}
}
