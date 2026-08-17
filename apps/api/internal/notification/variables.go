package notification

// Catálogo de variáveis de template ESCOPADO POR TIPO. Cada template só oferece
// as variáveis que fazem sentido no seu momento da jornada e que o envio real
// de fato preenche — {keyword}/{expira_em} não têm valor num e-mail de entrega,
// {tracking_code}/{transportadora} não existem no DM do carrinho. O editor pede
// o catálogo passando ?type=<template>; sem type, devolve tudo (compat).

// VariableSpec descreve uma variável do catálogo (nome + ajuda + exemplo).
type VariableSpec struct {
	Name        string
	Description string
	Example     string
}

// variableCatalog é a fonte única de verdade de cada variável (ordem de
// exibição preservada por templateVariableScopes, não aqui).
var variableCatalog = map[string]VariableSpec{
	"{handle}":      {"{handle}", "Nome de usuário do comprador", "@cliente_exemplo"},
	"{produto}":     {"{produto}", "Nome do produto", "Camiseta Preta M"},
	"{keyword}":     {"{keyword}", "Palavra-chave do produto", "ABCD"},
	"{quantidade}":  {"{quantidade}", "Quantidade do último item", "2"},
	"{total_itens}": {"{total_itens}", "Total de itens no carrinho", "3"},
	"{total}":       {"{total}", "Valor total formatado", "R$ 199,90"},
	"{link}":        {"{link}", "Link de checkout do carrinho", "https://sualoja.com/cart/abc123"},
	"{loja}":        {"{loja}", "Nome da loja", "Minha Loja"},
	"{expira_em}":   {"{expira_em}", "Tempo até o carrinho expirar", "48 horas"},
	"{evento}":      {"{evento}", "Nome da campanha", "Semana Black"},
	"{sessao}":      {"{sessao}", "Nome ou data da transmissão", "Live de segunda"},
	"{prazo_final}": {"{prazo_final}", "Data e hora limite para finalizar", "09/11 às 23h59"},
	"{comeca_em}":   {"{comeca_em}", "Quando a campanha começa", "03/11 às 20h"},
	"{tempo_extra}": {"{tempo_extra}", "Prazo extra ganho pela fila", "30 minutos"},
	// {live_titulo} continua no catálogo por compatibilidade com templates já
	// salvos, mas some da oferta de todo escopo: quem escrever um template novo
	// pelo editor recebe {evento}. Ver RenderTemplate — os dois apontam para o
	// mesmo valor.
	"{live_titulo}":      {"{live_titulo}", "Nome da campanha (nome antigo de {evento})", "Semana Black"},
	"{nome_cliente}":     {"{nome_cliente}", "Nome do cliente no checkout", "Ana Reis"},
	"{numero_pedido}":    {"{numero_pedido}", "Número do pedido", "1234"},
	"{lista_produtos}":   {"{lista_produtos}", "Tabela com os itens do pedido", "2× Vestido Midi — R$ 179,80…"},
	"{forma_pagamento}":  {"{forma_pagamento}", "Forma de pagamento (PIX/cartão)", "PIX"},
	"{link_pedido}":      {"{link_pedido}", "Link para acompanhar o pedido", "https://livecart.app/order/1234"},
	"{transportadora}":   {"{transportadora}", "Transportadora + serviço", "Sedex via Correios"},
	"{tracking_code}":    {"{tracking_code}", "Código de rastreio", "BR123456789BR"},
	"{prazo_entrega}":    {"{prazo_entrega}", "Prazo estimado do frete", "até 5 dias úteis"},
	"{endereco_entrega}": {"{endereco_entrega}", "Endereço de entrega em uma linha", "Rua das Flores, 123 — São Paulo/SP"},
	"{valor_frete}":      {"{valor_frete}", "Valor do frete", "R$ 18,90"},
}

// Conjuntos reaproveitados pelos escopos.
var (
	// cartVariables oferece {evento} no lugar de {live_titulo}: os dois
	// renderizam o mesmo valor, e o editor só precisa ensinar o nome novo.
	cartVariables = []string{
		"{handle}", "{produto}", "{keyword}", "{quantidade}",
		"{total_itens}", "{total}", "{link}", "{loja}", "{evento}",
	}
	orderBaseVariables = []string{
		"{nome_cliente}", "{numero_pedido}", "{total}",
		"{forma_pagamento}", "{link_pedido}", "{loja}",
	}
	shippingVariables = []string{
		"{lista_produtos}", "{transportadora}", "{tracking_code}",
		"{prazo_entrega}", "{endereco_entrega}", "{valor_frete}",
	}
)

// templateVariableScopes mapeia cada tipo de template ao seu conjunto ordenado
// de variáveis oferecidas. As chaves são os template_type que o FE já usa nas
// rotas /communications/{tipo}.
var templateVariableScopes = map[string][]string{
	// DM de carrinho (pré-pagamento) — variáveis do carrinho/live.
	"checkout_immediate": cartVariables,
	"item_added":         cartVariables,
	"checkout_reminder":  append(append([]string{}, cartVariables...), "{expira_em}"),

	// Gatilhos da RN-28. Escopo estreito de propósito: o gatilho de "fora da
	// janela" dispara ANTES de existir carrinho, então oferecer {total} ou
	// {link} ali seria anunciar variável que renderiza vazia.
	"out_of_window_scheduled":     {"{handle}", "{loja}", "{evento}", "{comeca_em}"},
	"out_of_window_session_ended": {"{handle}", "{loja}", "{evento}", "{sessao}", "{link}"},
	"out_of_window_event_ended":   {"{handle}", "{loja}", "{evento}"},
	"event_deadline_started":      append(append([]string{}, cartVariables...), "{prazo_final}"),
	"waitlist_unfulfilled":        {"{handle}", "{loja}", "{evento}", "{produto}", "{link}"},
	// A entrada na fila já tem carrinho (o item vai para lá como aguardando, e
	// no caso parcial parte dele foi mesmo levada), então o escopo é o do
	// carrinho inteiro — {total_itens}/{total} são o que conta a verdade do que
	// o comprador de fato levou.
	"waitlist_joined": cartVariables,

	// E-mails pós-venda — variáveis do pedido.
	"payment_confirmed": append(append([]string{}, orderBaseVariables...), "{lista_produtos}"),
	"payment_cancelled": orderBaseVariables,
	"payment_refunded":  orderBaseVariables,
	"shipped":           append(append([]string{}, orderBaseVariables...), shippingVariables...),
	"delivered":         append(append([]string{}, orderBaseVariables...), shippingVariables...),
}

// VariablesForTemplate devolve as variáveis oferecidas por um tipo de template,
// na ordem de exibição. templateType vazio ou desconhecido devolve o catálogo
// completo (ordem estável), preservando o comportamento antigo do endpoint.
func VariablesForTemplate(templateType string) []VariableSpec {
	names, ok := templateVariableScopes[templateType]
	if !ok {
		names = allVariableNames()
	}
	specs := make([]VariableSpec, 0, len(names))
	for _, n := range names {
		if spec, exists := variableCatalog[n]; exists {
			specs = append(specs, spec)
		}
	}
	return specs
}

// allVariableNames devolve todos os nomes do catálogo numa ordem estável
// (carrinho → pedido → frete), usada quando nenhum tipo é informado.
func allVariableNames() []string {
	seen := map[string]bool{}
	var out []string
	groups := [][]string{
		cartVariables,
		{"{expira_em}", "{sessao}", "{prazo_final}", "{comeca_em}", "{tempo_extra}"},
		orderBaseVariables,
		shippingVariables,
	}
	for _, group := range groups {
		for _, n := range group {
			if !seen[n] {
				seen[n] = true
				out = append(out, n)
			}
		}
	}
	return out
}
