package live

// Repartição da quantidade final de um produto entre as sessões que a
// venderam — RN-29.
//
// O problema: cart_items diz quantas unidades de um produto o carrinho tem, mas
// não de onde vieram. cart_item_events diz cada adição e sua sessão, mas a soma
// do log pode ser MAIOR que a quantidade final (o comprador ou o lojista
// removeu unidades depois) ou MENOR (alguém setou a quantidade direto, por um
// caminho que não passa pelo log).
//
// A regra: percorrer o log em ordem cronológica e ir preenchendo até completar a
// quantidade final.
//   - Sobrou log? As adições MAIS RECENTES ficam de fora. É a semântica
//     intuitiva de remoção: tira-se o que foi posto por último.
//   - Faltou log? A diferença vai para a sessão da ÚLTIMA adição — é a melhor
//     evidência que existe de onde aquele acréscimo veio.
//   - Log vazio? Tudo fica sem sessão.
//
// Em qualquer caso SUM(alocado) == quantidade final, que é o invariante que faz
// "receita por sessão" fechar com o total do pedido.

// CartItemAddition é uma linha do log, já ordenada cronologicamente.
type CartItemAddition struct {
	SessionID string // vazio = adição sem sessão (ex.: item posto pelo painel)
	Quantity  int
	UnitPrice int64
}

// SessionAllocation é quanto de um produto ficou atribuído a uma sessão.
type SessionAllocation struct {
	SessionID string
	Quantity  int
	UnitPrice int64
}

// AllocateBySession reparte finalQty entre as adições, na ordem recebida.
//
// additions precisa vir em ordem cronológica. Adições consecutivas da mesma
// sessão com o mesmo preço são fundidas no resultado, para o pedido não ganhar
// linhas repetidas à toa.
func AllocateBySession(finalQty int, additions []CartItemAddition) []SessionAllocation {
	if finalQty <= 0 {
		return nil
	}

	out := make([]SessionAllocation, 0, len(additions))
	remaining := finalQty

	appendOrMerge := func(sessionID string, qty int, price int64) {
		if qty <= 0 {
			return
		}
		if n := len(out); n > 0 && out[n-1].SessionID == sessionID && out[n-1].UnitPrice == price {
			out[n-1].Quantity += qty
			return
		}
		out = append(out, SessionAllocation{SessionID: sessionID, Quantity: qty, UnitPrice: price})
	}

	var lastSession string
	var lastPrice int64
	for _, add := range additions {
		if add.Quantity <= 0 {
			continue
		}
		lastSession, lastPrice = add.SessionID, add.UnitPrice
		if remaining == 0 {
			// Log já excedido: esta adição foi removida depois. Segue o laço só
			// para manter lastSession/lastPrice apontando para a adição mais
			// recente, que é onde um eventual excedente iria parar.
			continue
		}
		take := add.Quantity
		if take > remaining {
			take = remaining
		}
		appendOrMerge(add.SessionID, take, add.UnitPrice)
		remaining -= take
	}

	// Quantidade final maior que tudo o que o log conhece: o resto vai para a
	// última adição vista. Sem log nenhum, fica sem sessão.
	if remaining > 0 {
		appendOrMerge(lastSession, remaining, lastPrice)
	}

	return out
}
