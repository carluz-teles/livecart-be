package live

// Métrica em dois níveis — a metade PROJETADA (Fatia 5).
//
// A metade confirmada já vem pronta do banco: order_items tem session_id desde a
// 000109, então "quanto a transmissão de terça faturou" é um GROUP BY. A metade
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
	// CartEventID é o evento ÂNCORA do carrinho. Serve de destino das unidades
	// sem transmissão (session vazia) na atribuição por evento de origem.
	CartEventID string
}

// CartItemAdditionRow é uma linha do log de adições já situada no seu carrinho e
// produto. Espelha uma linha de ListCartItemEventsByEvent.
type CartItemAdditionRow struct {
	CartID    string
	ProductID string
	// SessionEventID é o evento da transmissão que fez esta adição
	// (live_sessions.event_id). Vazio quando a adição não tem sessão.
	SessionEventID string
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

// ProjectBySessionForEvent é ProjectBySession com atribuição por evento de
// ORIGEM (Clientes VIP / F3). Reparte os itens dos carrinhos abertos entre as
// transmissões, mas fica só com a fatia cujo evento de origem é targetEventID:
//
//   - unidade de uma sessão → conta para o evento DA SESSÃO (session_event_id);
//   - unidade sem sessão (posta pelo painel, ou anterior ao log) → conta para o
//     evento ÂNCORA do carrinho (cart_event_id).
//
// Para carrinho normal (um evento) TODA unidade resolve para o próprio evento,
// então o resultado é IDÊNTICO a ProjectBySession — é o que garante a paridade
// da tela de qualquer evento sem VIP. Só o carrinho VIP cross-evento se reparte.
//
// items e additions vêm da versão AMPLIADA das queries (carrinho VIP ancorado em
// outro evento também entra quando vendeu em targetEventID); o filtro por evento
// aqui é o que impede a fatia do outro evento de vazar.
func ProjectBySessionForEvent(items []OpenCartItem, additions []CartItemAdditionRow, targetEventID string) []SessionProjection {
	log := make(map[cartProduct][]CartItemAddition, len(items))
	sessionEvent := map[string]string{}
	for _, a := range additions {
		k := cartProduct{a.CartID, a.ProductID}
		log[k] = append(log[k], a.CartItemAddition)
		if a.SessionID != "" {
			sessionEvent[a.SessionID] = a.SessionEventID
		}
	}

	type acc struct {
		units   int
		revenue int64
		carts   map[string]struct{}
	}
	bySession := map[string]*acc{}
	var order []string

	for _, item := range items {
		for _, alloc := range AllocateBySession(item.Quantity, log[cartProduct{item.CartID, item.ProductID}]) {
			// Evento de origem desta fatia.
			originEvent := item.CartEventID // adição sem sessão cai no âncora
			if alloc.SessionID != "" {
				originEvent = sessionEvent[alloc.SessionID]
			}
			if originEvent != targetEventID {
				continue // fatia de outro evento (carrinho VIP cross-evento)
			}
			a := bySession[alloc.SessionID]
			if a == nil {
				a = &acc{carts: map[string]struct{}{}}
				bySession[alloc.SessionID] = a
				order = append(order, alloc.SessionID)
			}
			a.units += alloc.Quantity
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
