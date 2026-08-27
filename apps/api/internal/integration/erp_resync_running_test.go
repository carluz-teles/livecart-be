package integration

// A guarda de obsolescência da marca de varredura.
//
// A marca é escrita ao enfileirar e apagada num defer. O defer não é garantia:
// deploy no meio da varredura, OOM ou pod reciclado matam o processo antes
// dele. Sem a guarda, o botão do lojista ficaria desabilitado para sempre e ele
// não teria como destravar sozinho — só abrindo chamado.

import (
	"testing"
	"time"

	"livecart/apps/api/internal/integration/providers"
)

func marcaDe(t time.Time) map[string]any {
	return map[string]any{
		providers.MetadataResyncRunningSince: t.UTC().Format(time.RFC3339),
	}
}

func TestERPResyncRunningFromMetadata(t *testing.T) {
	casos := []struct {
		nome     string
		metadata map[string]any
		quer     bool
	}{
		{"sem metadata", nil, false},
		{"metadata vazio", map[string]any{}, false},
		{"varredura recém-iniciada", marcaDe(time.Now()), true},
		{"varredura de meia hora atrás ainda vale", marcaDe(time.Now().Add(-30 * time.Minute)), true},
		{
			// Passou do timeout da task (45min) e da guarda (60min): o processo
			// morreu sem limpar, e insistir travaria o lojista para sempre.
			"marca velha demais é abandonada",
			marcaDe(time.Now().Add(-2 * time.Hour)),
			false,
		},
		{
			// Preferimos deixar tentar de novo — a dedup por TaskID barra a
			// segunda varredura — a travar o botão por um valor indecifrável.
			"marca ilegível não trava o botão",
			map[string]any{providers.MetadataResyncRunningSince: "ontem à tarde"},
			false,
		},
		{
			"marca de outro tipo não trava o botão",
			map[string]any{providers.MetadataResyncRunningSince: 12345},
			false,
		},
		{
			// Outras chaves vivem no mesmo metadata e não podem ser confundidas
			// com varredura em andamento.
			"outra configuração no metadata não conta como varredura",
			map[string]any{"alguma_outra_configuracao": true},
			false,
		},
	}

	for _, c := range casos {
		t.Run(c.nome, func(t *testing.T) {
			if got := ERPResyncRunningFromMetadata(c.metadata); got != c.quer {
				t.Errorf("ERPResyncRunningFromMetadata = %v, quero %v", got, c.quer)
			}
		})
	}
}

// O progresso só existe enquanto a varredura existe.
//
// Números pendurados de uma varredura antiga descreveriam trabalho que já
// terminou — e o botão mostraria "154 de 154" para sempre, que é pior que não
// mostrar nada: parece travado no fim em vez de parado.
func TestERPResyncProgressFromMetadata(t *testing.T) {
	agora := time.Now()

	t.Run("varredura viva devolve o progresso", func(t *testing.T) {
		md := marcaDe(agora)
		md[providers.MetadataResyncDone] = 42
		md[providers.MetadataResyncTotal] = 154
		done, total := ERPResyncProgressFromMetadata(md)
		if done != 42 || total != 154 {
			t.Errorf("progresso = %d de %d, quero 42 de 154", done, total)
		}
	})

	t.Run("numeros que passaram pelo JSONB chegam como float", func(t *testing.T) {
		// É a forma real: o metadata volta do Postgres decodificado por
		// encoding/json, que transforma todo número em float64. Ler só `int`
		// devolveria zero para toda varredura vinda do banco — ou seja, sempre.
		md := marcaDe(agora)
		md[providers.MetadataResyncDone] = float64(42)
		md[providers.MetadataResyncTotal] = float64(154)
		done, total := ERPResyncProgressFromMetadata(md)
		if done != 42 || total != 154 {
			t.Errorf("progresso = %d de %d, quero 42 de 154", done, total)
		}
	})

	t.Run("sem varredura viva o progresso zera", func(t *testing.T) {
		md := map[string]any{
			providers.MetadataResyncDone:  154,
			providers.MetadataResyncTotal: 154,
		}
		if done, total := ERPResyncProgressFromMetadata(md); done != 0 || total != 0 {
			t.Errorf("progresso = %d de %d numa varredura que não está rodando", done, total)
		}
	})

	t.Run("marca velha demais também zera o progresso", func(t *testing.T) {
		md := marcaDe(agora.Add(-2 * time.Hour))
		md[providers.MetadataResyncDone] = 30
		md[providers.MetadataResyncTotal] = 154
		if done, total := ERPResyncProgressFromMetadata(md); done != 0 || total != 0 {
			t.Errorf("progresso = %d de %d de uma varredura abandonada", done, total)
		}
	})
}
