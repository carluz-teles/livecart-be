package live

// RN-19 — o vocabulário oficial é EVENTO. /events é o prefixo novo e /lives
// continua no ar porque o frontend inteiro o chama, e trocar a rota seria um
// deploy acoplado entre dois repositórios.
//
// O risco desse arranjo não é a rota nova: é a próxima. Uma rota acrescentada
// só num dos prefixos responde 404 no outro, e o único jeito de descobrir é um
// lojista clicando. Este teste compara os dois conjuntos e falha na hora.

import (
	"sort"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v2"
)

func TestEventsELivesExpoemExatamenteAsMesmasRotas(t *testing.T) {
	app := fiber.New()
	// Service nil é suficiente: RegisterRoutes só monta a árvore, nada é
	// executado aqui.
	h := &Handler{}
	h.RegisterRoutes(app.Group("/api/v1/stores/:storeId"))

	byPrefix := map[string][]string{"/events": {}, "/lives": {}}
	for _, r := range app.GetRoutes() {
		for prefix := range byPrefix {
			marker := prefix
			idx := strings.Index(r.Path, marker)
			if idx < 0 {
				continue
			}
			// Guarda a rota SEM o prefixo, para os dois conjuntos ficarem
			// comparáveis: o que importa é o sufixo e o método.
			suffix := r.Method + " " + r.Path[idx+len(marker):]
			byPrefix[prefix] = append(byPrefix[prefix], suffix)
		}
	}

	events := byPrefix["/events"]
	lives := byPrefix["/lives"]

	if len(events) == 0 {
		t.Fatal("nenhuma rota registrada sob /events")
	}
	sort.Strings(events)
	sort.Strings(lives)

	if len(events) != len(lives) {
		t.Fatalf("/events tem %d rotas e /lives tem %d — alguma foi registrada só num prefixo", len(events), len(lives))
	}
	for i := range events {
		if events[i] != lives[i] {
			t.Errorf("divergência na posição %d: /events tem %q e /lives tem %q", i, events[i], lives[i])
		}
	}
}
