package idea

// Category catalog for the ideas channel. Slugs match the CHECK constraint in
// migration 000060. Labels are PT-BR for end-user display and derived from the
// product surface (event/cart/checkout/integrations/etc.) so contributors pick
// the same buckets the dev team uses internally.
type Category struct {
	Slug  string `json:"slug"`
	Label string `json:"label"`
}

var Categories = []Category{
	{"eventos_lives", "Eventos & Lives"},
	{"checkout", "Checkout"},
	{"carrinho", "Carrinho"},
	{"pagamentos", "Pagamentos"},
	{"frete_logistica", "Frete & Logística"},
	{"produtos", "Produtos & Variantes"},
	{"pedidos", "Pedidos & Fulfillment"},
	{"clientes", "Clientes"},
	{"integracoes_erp", "Integrações ERP"},
	{"integracoes_social", "Integrações Sociais"},
	{"notificacoes", "Notificações"},
	{"dashboard_relatorios", "Dashboard & Relatórios"},
	{"time_permissoes", "Time & Permissões"},
	{"onboarding", "Onboarding & Setup"},
	{"api_webhooks", "API & Webhooks"},
	{"outros", "Outros"},
}

var categorySlugSet = func() map[string]struct{} {
	m := make(map[string]struct{}, len(Categories))
	for _, c := range Categories {
		m[c.Slug] = struct{}{}
	}
	return m
}()

func IsValidCategory(slug string) bool {
	_, ok := categorySlugSet[slug]
	return ok
}

// Status values match the CHECK constraint on ideas.status.
const (
	StatusOpen        = "aberta"
	StatusUnderStudy  = "em_estudo"
	StatusInDev       = "em_desenvolvimento"
	StatusCompleted   = "concluida"
	StatusRejected    = "recusada"
)

// Tab values are filter shortcuts used by the feed UI.
const (
	TabAll        = "all"
	TabNew        = "new"
	TabMine       = "mine"
	TabUnderStudy = "under_study"
	TabCompleted  = "completed"
)

// Sort options for the feed.
const (
	SortTrending = "trending"
	SortNew      = "new"
)
