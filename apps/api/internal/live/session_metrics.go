package live

// Métrica em dois níveis — a metade PROJETADA (Fatia 5).
//
// A metade confirmada já vem pronta do banco: order_items tem session_id desde a
// 000107, então "quanto a transmissão de terça faturou" é um GROUP BY. A metade
// projetada não tem equivalente: cart_items sabe QUANTO o carrinho tem, e
// cart_item_events sabe DE ONDE veio cada unidade — juntar os dois é a mesma
// repartição que o selamento faz.
//
// Por isso este arquivo NÃO reimplementa a regra: ele arruma os dados e chama
// AllocateBySession, a função que o selamento do pedido usa. Escrever a mesma
// regra uma segunda vez (em SQL, por exemplo) faria projetado e confirmado
// divergirem no instante do pagamento — que é justamente o que a métrica em dois
// níveis não pode permitir.
//
// O invariante que sai daqui: SUM(alocado) == quantidade final do item, para
// todo item. Como a receita usa o unit_price do CARRINHO (não o do log, igual ao
// selamento), somar as sessões devolve exatamente SUM(quantity * unit_price) —
// que é cart_product_total_cents, que é o projected_revenue do evento.

// OpenCartItem é a quantidade final de um produto num carrinho que entra na
// projeção. Espelha uma linha de ListOpenCartItemsByEvent.
type OpenCartItem struct {
	CartID    string
	ProductID string
	Quantity  int
	UnitPrice int64
}

// CartItemAdditionRow é uma linha do log de adições já situada no seu carrinho e
// produto. Espelha uma linha de ListCartItemEventsByEvent.
type CartItemAdditionRow struct {
	CartID    string
	ProductID string
	CartItemAddition
}

// SessionProjection é o projetado de UMA transmissão. SessionID vazio é o balde
// "sem transmissão" — item posto pelo painel, ou carrinho montado antes de o log
// existir. Ele é parte do resultado, não resto descartável: sem devolvê-lo a
// soma das sessões deixa de bater com o total do evento.
type SessionProjection struct {
	SessionID    string
	Units        int
	RevenueCents int64
	OpenCarts    int
}

type cartProduct struct {
	cart    string
	product string
}

// ProjectBySession reparte os itens dos carrinhos abertos entre as transmissões
// que os venderam.
//
// additions precisa vir ordenada por (cart, produto, cronologia) — é a ordem que
// ListCartItemEventsByEvent devolve e a que AllocateBySession exige.
func ProjectBySession(items []OpenCartItem, additions []CartItemAdditionRow) []SessionProjection {
	log := make(map[cartProduct][]CartItemAddition, len(items))
	for _, a := range additions {
		k := cartProduct{a.CartID, a.ProductID}
		log[k] = append(log[k], a.CartItemAddition)
	}

	type acc struct {
		units   int
		revenue int64
		carts   map[string]struct{}
	}
	bySession := map[string]*acc{}
	// Ordem de primeira aparição, para o resultado ser determinístico sem
	// depender da iteração do mapa. Quem consome ordena por sequence_order.
	var order []string

	for _, item := range items {
		for _, alloc := range AllocateBySession(item.Quantity, log[cartProduct{item.CartID, item.ProductID}]) {
			a := bySession[alloc.SessionID]
			if a == nil {
				a = &acc{carts: map[string]struct{}{}}
				bySession[alloc.SessionID] = a
				order = append(order, alloc.SessionID)
			}
			a.units += alloc.Quantity
			// O preço é o do CARRINHO, não o do log. O log só registra o preço
			// praticado na hora da adição, e usá-lo aqui faria a projeção
			// divergir de cart_product_total_cents assim que o lojista mudasse
			// o preço — exatamente o que o selamento evita (on_cart_paid).
			a.revenue += int64(alloc.Quantity) * item.UnitPrice
			a.carts[item.CartID] = struct{}{}
		}
	}

	out := make([]SessionProjection, 0, len(order))
	for _, sessionID := range order {
		a := bySession[sessionID]
		out = append(out, SessionProjection{
			SessionID:    sessionID,
			Units:        a.units,
			RevenueCents: a.revenue,
			OpenCarts:    len(a.carts),
		})
	}
	return out
}
