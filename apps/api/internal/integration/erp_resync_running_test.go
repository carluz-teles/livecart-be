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
			// A chave que liga o saldo disponível vive no mesmo metadata e não
			// pode ser confundida com varredura em andamento.
			"outra configuração no metadata não conta como varredura",
			map[string]any{providers.MetadataUseAvailableStock: true},
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
