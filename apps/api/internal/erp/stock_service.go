package erp

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"go.uber.org/zap"

	"livecart/apps/api/internal/integration/providers"
	"livecart/apps/api/lib/httpx"
	"livecart/apps/api/lib/logger"
)

// StockCollaborators groups the integration-Service helpers the migrated ERP
// flow still calls back into. It is satisfied by integration.Service (wired when
// the delegating methods build the erp.Service), so erp stays free of the
// integration import. The interface shrinks slice by slice as the
// provider/contact/order-creation logic itself migrates (B2c-2+).
type StockCollaborators interface {
	// ResolveProvider builds the ERP provider client for an active integration,
	// honouring the finalisation tests' scripted-provider seam.
	ResolveProvider(ctx context.Context, integration *Integration) (providers.ERPProvider, error)
	// ResolveExternalProduct maps a local product to its ERP external id.
	// linked=false means there is nothing to move against the ERP — no product
	// syncer wired, the product is not linked, or the lookup failed — and the
	// caller MUST treat that as a silent no-op (the source semantics).
	ResolveExternalProduct(ctx context.Context, storeID, productID string) (externalID string, linked bool)

	// --- Order-as-reservation lifecycle collaborators (Bloco B2c, Design C) ---

	// OrderAtCheckoutEnabled reports whether the store runs design C (converting
	// the cart into an ERP order from payment initiation). Reads the
	// integration-owned per-store rollout flag.
	OrderAtCheckoutEnabled(storeID string) bool
	// ResolveERPContact finds/creates and enriches the ERP contact for a platform
	// user (the recurring-customer prewarm hits the same cache).
	ResolveERPContact(ctx context.Context, provider providers.ERPProvider, integration *Integration, storeID, platformUserID, platformHandle, name, document, email, phone string) (string, error)
	// CreateFinalERPOrderForConversion creates the unpaid ERP order (situação
	// Aberta, no stock launch) for a cart conversion and persists its external id.
	CreateFinalERPOrderForConversion(ctx context.Context, provider providers.ERPProvider, integration *Integration, storeID, cartID string) error
	// CreateFinalERPOrder creates the final sales order for the legacy
	// post-payment finalisation, loading the cart internally (CartRow stays
	// integration-owned to avoid the import cycle). status carries the gateway
	// payment; launchStock=true launches the order stock inline (legacy order),
	// false leaves the launch to the caller (inverted, launch-first).
	CreateFinalERPOrder(ctx context.Context, provider providers.ERPProvider, integration *Integration, storeID, cartID string, status *providers.PaymentStatus, launchStock bool) error
	// FinalisationInverted reports whether the store runs the launch-first
	// finalisation order (Fase 3 rollout flag, integration-owned).
	FinalisationInverted(storeID string) bool
	// ReReserveAfterFailedFinalisation re-creates the Tiny saída-manual exits and
	// local reservation rows we reversed during a finalisation that then failed,
	// so a paid cart never silently releases stock. Best-effort (logs, never
	// returns) — the caller's primary signal is the upstream create error.
	ReReserveAfterFailedFinalisation(ctx context.Context, provider providers.ERPProvider, cartID string, snapshot []StockReservationRow)
	// ReverseCartReservationsPerRow estorna the cart's active manual stock exits,
	// row by row, marking each only after the ERP confirms the entry.
	ReverseCartReservationsPerRow(ctx context.Context, provider providers.ERPProvider, storeID, cartID string) error
	// MarkFinalisationFailed records the 'failed' finalisation state with the
	// error and emits the group G erp.finalization_failed fact (best-effort).
	MarkFinalisationFailed(ctx context.Context, cartID, msg string)
	// MirrorToOrder projects the cart's current ERP state into the Order
	// aggregate (best-effort; no-op when the mirror is not wired).
	MirrorToOrder(ctx context.Context, cartID string)
	// EmitERPOrderFinalized publishes the group G erp.order_finalized fact for a
	// confirmed order-as-reservation (best-effort, dedup by cart).
	EmitERPOrderFinalized(ctx context.Context, storeID, cartID string)
	// EmitERPOrderCancelled publishes the group G erp.order_cancelled fact for a
	// cancelled/refunded order-as-reservation (best-effort, dedup by order id).
	EmitERPOrderCancelled(ctx context.Context, storeID, cartID, externalOrderID, reason string)

	// --- Cart NFe sync / health-check collaborators (Bloco B2d) ---

	// ResolveERPProviderByID resolves the ERP provider for a specific integration
	// id (the health-check anchors on the integration, not the store's active
	// one). Backed by integration.Service.GetERPProvider.
	ResolveERPProviderByID(ctx context.Context, integrationID, storeID string) (providers.ERPProvider, error)
	// HandleProviderError records a provider failure into integration telemetry
	// (integration_logs / status), keeping that ACL concern in the integration
	// package. Best-effort — it never returns.
	HandleProviderError(ctx context.Context, integrationID, operation string, err error)
}

// =============================================================================
// CART → ERP STOCK RESERVATION (moved from internal/integration, Bloco B2b)
// =============================================================================

// ReserveStockInERP creates a manual stock exit (tipo S) in the ERP for a product
// added to a cart. The movement is tracked in stock_reservations for later reversal.
func (s *Service) ReserveStockInERP(ctx context.Context, storeID, cartID, eventID, productID string, quantity int, unitPrice int64, platformHandle string) error {
	integration, err := s.repo.GetActiveByProvider(ctx, storeID, "erp", "tiny")
	if err != nil {
		logger.From(ctx, s.logger).Debug("no active ERP integration, skipping stock reservation",
			zap.String("store_id", storeID),
		)
		return nil
	}

	// Cart convertido em pedido-como-reserva (design C): quem segura a peça é
	// o PEDIDO, não saídas manuais — a grade nova (o caller já gravou o item
	// no cart) entra pelo ciclo estornar→PUT→lançar. Cobre o live-add pós-pix
	// e a promoção de waitlist de cart convertido num único ponto.
	if st, stErr := s.repo.GetCartERPOrderState(ctx, cartID); stErr == nil &&
		st.State != OrderStateNone && st.State != OrderStateCancelled {
		if mutErr := s.MutateERPOrderItems(ctx, cartID, storeID); mutErr != nil {
			return fmt.Errorf("applying grid to converted cart order: %w", mutErr)
		}
		return nil
	}

	// MODO RESERVA: o PEDIDO é a reserva, e nenhum movimento manual acontece.
	//
	// O cart ainda não foi convertido (o ramo acima cuidaria disso), então este é
	// o PRIMEIRO item: criar o pedido de venda no Tiny já segura a peça, sem
	// tocar no saldo físico. A partir daqui todo item novo cai no ramo acima e
	// vira um `PUT /itens`, que reajusta a reserva.
	//
	// É o que substitui a saída manual tipo `S` — e com ela somem o webhook de
	// estoque que realimentava a fila, o par estorno→criação no pagamento e a
	// classe inteira de reservas órfãs.
	if s.reserveModeEnabled(ctx, storeID) {
		s.logReserveMode(ctx, cartID, true)
		if convErr := s.EnsureERPOrderForCart(ctx, cartID, storeID); convErr != nil {
			return fmt.Errorf("creating order-as-reservation for cart %s: %w", cartID, convErr)
		}
		return nil
	}

	erpProvider, err := s.collab.ResolveProvider(ctx, integration)
	if err != nil {
		return fmt.Errorf("creating ERP provider: %w", err)
	}

	// Get external product ID
	externalID, linked := s.collab.ResolveExternalProduct(ctx, storeID, productID)
	if !linked {
		logger.From(ctx, s.logger).Debug("product not linked to ERP, skipping stock reservation",
			zap.String("product_id", productID),
		)
		return nil
	}

	// Já existe reserva deste produto neste carrinho: SOMA, não pula.
	//
	// `quantity` é a quantidade DESTA adição — o mesmo número que o chamador
	// acabou de descontar do estoque local —, nunca o total do carrinho. Pular
	// aqui rompia a única coisa que precisa ser verdade: o que o LiveCart tirou
	// do estoque tem de estar reservado no ERP.
	//
	// O que acontecia: o comprador comenta "quero 1000" três vezes numa live.
	// A primeira criava a reserva; a segunda e a terceira entravam no carrinho,
	// baixavam o estoque local e NÃO reservavam no ERP. Resultado em campo: 5
	// unidades vendidas no LiveCart e só 3 seguradas no Tiny — as outras 2
	// seguiam à venda em qualquer outro canal, e no estorno voltariam 3 de 5.
	//
	// Repetir o mesmo comentário não cai aqui: o comentário é deduplicado por
	// platform_comment_id antes de chegar ao carrinho.
	// Caminho com razão: a intenção é gravada ANTES da chamada, e a chamada
	// roda fora do prazo da requisição. Foi o prazo compartilhado que perdeu
	// duas respostas do Tiny em 17/08 — item no carrinho, nada registrado, e um
	// lançamento órfão que ninguém ia estornar. Com o razão, todo desfecho
	// (inclusive "não sei") vira linha consultável, o retry só acontece com
	// prova de não-entrega, e a finalização não fecha com dúvida aberta.
	cartRef := s.cartRef(ctx, cartID)

	if s.movements != nil {
		mov, movErr := s.movements.CreateERPStockMovement(ctx, CreateStockMovementParams{
			StoreID:           storeID,
			CartID:            cartID,
			EventID:           eventID,
			ProductID:         productID,
			ExternalProductID: externalID,
			Direction:         "out",
			Quantity:          quantity,
			UnitPriceCents:    unitPrice,
		})
		if movErr != nil {
			// Sem registro de intenção não há chamada: é o registro que garante
			// que nenhum desfecho se perde.
			return fmt.Errorf("recording stock movement intent: %w", movErr)
		}
		obs := movementObservacao(cartRef, platformHandle, 0)
		go s.executeStockMovement(logger.WithStore(context.Background(), storeID, ""), erpProvider, mov, obs)
		return nil
	}

	existing, _ := s.repo.ListActiveReservationsByCartAndProduct(ctx, cartID, productID)
	if len(existing) > 0 {
		obs := fmt.Sprintf("@%s - Cart %s (+%d)", platformHandle, cartRef, quantity)
		movementID, err := erpProvider.ReserveStock(ctx, externalID, quantity, float64(unitPrice)/100, obs)
		if err != nil {
			return fmt.Errorf("reserving additional stock in ERP: %w", err)
		}
		if _, err := s.repo.AdjustActiveReservationQuantity(ctx, cartID, productID, quantity, movementID); err != nil {
			// A saída no ERP já aconteceu; sem a linha local o estorno não sabe
			// devolvê-la. Erro alto para a reconciliação pegar.
			return fmt.Errorf("bumping reservation quantity after ERP movement %s: %w", movementID, err)
		}
		logger.From(ctx, s.logger).Info("ERP reservation increased for repeat add",
			zap.String("cart_id", cartID),
			zap.String("product_id", productID),
			zap.Int("added", quantity),
			zap.String("erp_movement_id", movementID),
		)
		return nil
	}

	obs := fmt.Sprintf("@%s - Cart %s", platformHandle, cartRef)
	movementID, err := erpProvider.ReserveStock(ctx, externalID, quantity, float64(unitPrice)/100, obs)
	if err != nil {
		return fmt.Errorf("reserving stock in ERP: %w", err)
	}

	_, err = s.repo.CreateStockReservation(ctx, CreateStockReservationParams{
		EventID:           eventID,
		CartID:            cartID,
		ProductID:         productID,
		ExternalProductID: externalID,
		Quantity:          quantity,
		ERPMovementID:     movementID,
	})
	if err != nil {
		// ERP movement was created but we can't track it locally — attempt compensating reversal
		logger.From(ctx, s.logger).Error("failed to save stock reservation, attempting ERP reversal",
			zap.String("cart_id", cartID),
			zap.String("product_id", productID),
			zap.String("erp_movement_id", movementID),
			zap.Error(err),
		)
		reverseObs := fmt.Sprintf("Estorno compensatório - falha DB - Cart %s", cartID)
		if _, reverseErr := erpProvider.ReverseStockReservation(ctx, externalID, quantity, 0, reverseObs); reverseErr != nil {
			logger.From(ctx, s.logger).Error("CRITICAL: failed to compensate ERP stock after DB failure — manual reconciliation required",
				zap.String("external_product_id", externalID),
				zap.Int("quantity", quantity),
				zap.String("erp_movement_id", movementID),
				zap.Error(reverseErr),
			)
		}
		return fmt.Errorf("saving stock reservation: %w", err)
	}

	logger.From(ctx, s.logger).Info("stock reserved in ERP",
		zap.String("cart_id", cartID),
		zap.String("product_id", productID),
		zap.String("external_product_id", externalID),
		zap.Int("quantity", quantity),
		zap.String("erp_movement_id", movementID),
	)

	return nil
}

// AdjustStockReservationDelta applies a quantity delta (positive or negative)
// to a (cart, product) reservation. It mutates both the local products.stock
// counter and the ERP reservation in a single call so the two stay in sync.
//
// Local stock is the source-of-truth gate for waitlist promotion
// (ProcessWaitlistForProduct's atomic DecrementProductStock) and live-add
// availability (processLiveAdd reads product.Stock). It is mutated FIRST so
// that even stores without an ERP integration get correct waitlist behavior.
// A failure in the optional ERP sync rolls the local mutation back.
//
// On delta > 0, an insufficient-stock condition returns httpx 422 instead of
// silently over-allocating (the previous behavior caused buyers reducing then
// re-increasing their cart to over-allocate against the original stock).
//
// Returns the ERP movement_id (empty when no ERP integration is configured or
// the product is not linked — both treated as no-ops for the ERP side; local
// stock is still updated).
// op labels the emitted stock event; pass StockOpUnspecified to use the default
// sign-based label (qty_increase / qty_decrease), or a specific op (e.g.
// waitlist_cancel / waitlist_expire) when the delta represents a domain action
// other than a buyer quantity edit.
func (s *Service) AdjustStockReservationDelta(ctx context.Context, storeID, cartID, eventID, productID string, delta int, unitPrice int64, platformHandle string, op StockOp) (string, error) {
	if delta == 0 {
		return "", nil
	}

	// 1. Local stock mutation — atomic gate for delta>0, mirror of the ERP
	//    reversal for delta<0. Runs unconditionally so waitlist promotion sees
	//    freed units immediately, even when the store has no ERP integration.
	if delta > 0 {
		if err := s.repo.DecrementProductStock(ctx, productID, delta); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return "", httpx.DomainError(422, httpx.CodeStockInsufficient, "estoque insuficiente para esse aumento")
			}
			return "", fmt.Errorf("decrementing local stock: %w", err)
		}
	} else {
		if err := s.repo.IncrementProductStock(ctx, productID, -delta); err != nil {
			return "", fmt.Errorf("releasing local stock: %w", err)
		}
	}

	// localCommitted tracks whether the local stock mutation above stands. It is
	// cleared by rollbackLocal so the deferred emit below fires ONLY on the
	// definitive success of this operation (every rollback path returns an error).
	localCommitted := true

	// Rollback helper used when ERP sync fails after local stock already moved.
	rollbackLocal := func() {
		localCommitted = false
		if delta > 0 {
			if err := s.repo.IncrementProductStock(ctx, productID, delta); err != nil {
				logger.From(ctx, s.logger).Error("failed to rollback local stock decrement after ERP failure",
					zap.String("product_id", productID),
					zap.Int("delta", delta),
					zap.Error(err),
				)
			}
		} else {
			if err := s.repo.DecrementProductStock(ctx, productID, -delta); err != nil {
				logger.From(ctx, s.logger).Error("failed to rollback local stock increment after ERP failure",
					zap.String("product_id", productID),
					zap.Int("delta", delta),
					zap.Error(err),
				)
			}
		}
	}

	// stock.reserved / stock.released — the single emit point for this operation.
	// Deferred + guarded so every nil-error return below funnels through here
	// without instrumenting each one, and rollbacks (which clear localCommitted)
	// stay silent.
	defer func() {
		if !localCommitted {
			return
		}
		if delta > 0 {
			reserveOp := op
			if reserveOp == StockOpUnspecified {
				reserveOp = StockOpQtyIncrease
			}
			s.stock.NoteReserved(ctx, ReserveParams{Op: reserveOp, ProductID: productID, Quantity: delta, CartID: cartID, EventID: eventID})
		} else {
			releaseOp := op
			if releaseOp == StockOpUnspecified {
				releaseOp = StockOpQtyDecrease
			}
			s.stock.NoteReleased(ctx, ReleaseParams{Op: releaseOp, ProductID: productID, Quantity: -delta, CartID: cartID, EventID: eventID})
		}
	}()

	// Cart convertido (design C): a mutação vai para o PEDIDO via ciclo
	// estornar→PUT→lançar — a grade final já está no banco (o checkout grava
	// o cart_item ANTES de chamar este método). Sem movimentação manual e sem
	// movementID; falha desfaz o estoque local para o comprador ver o erro.
	if st, stErr := s.repo.GetCartERPOrderState(ctx, cartID); stErr == nil &&
		st.State != OrderStateNone && st.State != OrderStateCancelled {
		if mutErr := s.MutateERPOrderItems(ctx, cartID, storeID); mutErr != nil {
			rollbackLocal()
			return "", fmt.Errorf("applying grid to converted cart order: %w", mutErr)
		}
		return "", nil
	}

	// 2. ERP sync — optional. Anything below is best-effort against the ERP;
	//    any failure rolls back the local change above.
	integration, err := s.repo.GetActiveByProvider(ctx, storeID, "erp", "tiny")
	if err != nil {
		logger.From(ctx, s.logger).Debug("no active ERP integration, skipping reservation delta",
			zap.String("store_id", storeID),
		)
		return "", nil
	}

	erpProvider, err := s.collab.ResolveProvider(ctx, integration)
	if err != nil {
		rollbackLocal()
		return "", fmt.Errorf("creating ERP provider: %w", err)
	}

	externalID, linked := s.collab.ResolveExternalProduct(ctx, storeID, productID)
	if !linked {
		logger.From(ctx, s.logger).Debug("product not linked to ERP, skipping reservation delta",
			zap.String("product_id", productID),
		)
		return "", nil
	}

	cartRef := s.cartRef(ctx, cartID)

	existing, _ := s.repo.ListActiveReservationsByCartAndProduct(ctx, cartID, productID)

	if delta > 0 {
		// BANCO PRIMEIRO, ERP DEPOIS — simétrico ao ramo de redução.
		//
		// Antes: lia as reservas ativas, chamava o Tiny, e só então escolhia
		// entre CREATE e ADJUST com base naquela leitura. Entre a leitura e a
		// gravação passa ~1s (o limitador), e a escolha envelhece. As duas
		// pontas quebraram no mesmo teste em 12/08/2026:
		//
		//   "no rows in result set" — leu reserva ativa, ela foi reversada no
		//   meio, e o ADJUST não achou linha nenhuma;
		//   "duplicate key ... uq_stock_reservations_active" — leu vazio, outra
		//   requisição criou a linha, e o CREATE colidiu.
		//
		// Nos dois casos o movimento JÁ estava no Tiny e o comprador levava 422
		// depois de o estoque ter se mexido. Clicando rápido no "+", ele
		// repetia o ciclo a cada tentativa.
		//
		// O upsert atômico não precisa escolher ramo: o índice parcial decide.
		row, saveErr := s.repo.UpsertActiveReservationQuantity(ctx, UpsertReservationParams{
			EventID:           eventID,
			CartID:            cartID,
			ProductID:         productID,
			ExternalProductID: externalID,
			IncQty:            delta,
		})
		if saveErr != nil {
			rollbackLocal()
			return "", fmt.Errorf("recording reservation increase: %w", saveErr)
		}

		obs := fmt.Sprintf("@%s - Cart %s (+%d)", platformHandle, cartRef, delta)
		movementID, err := erpProvider.ReserveStock(ctx, externalID, delta, float64(unitPrice)/100, obs)
		if err != nil {
			// O ERP recusou depois de já termos gravado. Desfazer é obrigatório:
			// a reserva diria que seguramos unidades que o Tiny não separou.
			if _, decErr := s.repo.DecrementActiveReservationQuantity(ctx, cartID, productID, delta); decErr != nil {
				logger.From(ctx, s.logger).Error("ERP refused the increase and the reservation could not be undone — unit inconsistent between DB and ERP",
					zap.String("cart_id", cartID),
					zap.String("product_id", productID),
					zap.String("reservation_id", row.ID),
					zap.Int("quantity", delta),
					zap.Error(decErr),
				)
			}
			rollbackLocal()
			return "", fmt.Errorf("reserving stock delta in ERP: %w", err)
		}

		logger.From(ctx, s.logger).Info("ERP reservation increased",
			zap.String("cart_id", cartID),
			zap.String("product_id", productID),
			zap.Int("delta", delta),
			zap.Int("new_qty", row.Quantity),
			zap.String("erp_movement_id", movementID),
		)
		return movementID, nil
	}

	// delta < 0
	if len(existing) == 0 {
		logger.From(ctx, s.logger).Warn("no active reservation to decrease for cart+product, skipping ERP call",
			zap.String("cart_id", cartID),
			zap.String("product_id", productID),
			zap.Int("delta", delta),
		)
		return "", nil
	}

	// BANCO PRIMEIRO, ERP DEPOIS — a ordem aqui é a correção.
	//
	// Antes: somava a quantidade de uma leitura anterior, decidia o ramo por
	// esse número, chamava o Tiny e SÓ ENTÃO gravava. Duas coisas davam errado
	// ao mesmo tempo. A leitura envelhecia durante a chamada HTTP (~1s com o
	// limitador), e quando a gravação seguinte falhava — tipicamente no
	// CHECK (quantity > 0) ao tentar zerar em vez de reverter — o movimento já
	// estava no Tiny e ninguém o desfazia.
	//
	// Foi exatamente isso em 12/08/2026: um PATCH (2→1) e um DELETE do mesmo
	// item se cruzaram por 83ms. O DELETE leu `cart_items` já em 1 e a reserva
	// ainda em 2, concluiu que sobraria 1, mandou a entrada (movimento
	// 365095970) e bateu no CHECK. O Gabinete Gamer fechou o dia com uma
	// unidade a mais no Tiny do que existia de verdade.
	//
	// Agora o UPDATE condicional decide: ou baixou (e diz quanto sobrou), ou não
	// havia o que baixar. Só depois o ERP é chamado, e se ele recusar a
	// quantidade volta. O pior caso deixou de ser "unidade fantasma invisível" e
	// passou a ser "unidade a menos no Tiny", que a reconciliação enxerga.
	dec := -delta
	res, err := s.repo.DecrementActiveReservationQuantity(ctx, cartID, productID, dec)
	if err != nil {
		rollbackLocal()
		return "", fmt.Errorf("decrementing reservation quantity: %w", err)
	}
	if !res.Applied {
		// A reserva tem menos do que se pediu para baixar: leitura obsoleta, ou
		// outra requisição do mesmo comprador chegou primeiro e já devolveu a
		// unidade. Reverter o que sobrou é a única leitura segura — e se não
		// sobrou nada, não há ERP a chamar.
		logger.From(ctx, s.logger).Warn("reservation smaller than the requested decrease; reversing what is left",
			zap.String("cart_id", cartID),
			zap.String("product_id", productID),
			zap.Int("delta", delta),
		)
		leftover := 0
		for _, r := range existing {
			leftover += r.Quantity
		}
		if leftover <= 0 {
			return "", nil
		}
		obs := fmt.Sprintf("@%s - Cart %s (-%d)", platformHandle, cartRef, leftover)
		movementID, ferr := erpProvider.ReverseStockReservation(ctx, externalID, leftover, 0, obs)
		if ferr != nil {
			rollbackLocal()
			return "", fmt.Errorf("reversing leftover reservation in ERP: %w", ferr)
		}
		if rerr := s.repo.ReverseReservationsByCartAndProduct(ctx, cartID, productID); rerr != nil {
			return movementID, fmt.Errorf("marking leftover reservation reversed: %w", rerr)
		}
		return movementID, nil
	}

	obs := fmt.Sprintf("@%s - Cart %s (%d)", platformHandle, cartRef, delta)
	movementID, err := erpProvider.ReverseStockReservation(ctx, externalID, dec, 0, obs)
	if err != nil {
		// O ERP recusou DEPOIS de já termos baixado no banco. Devolver as
		// unidades é obrigatório: sem isso o banco diria "livre" e o Tiny diria
		// "reservada", e nada reconciliaria as duas versões.
		for _, id := range res.ReservationIDs {
			if rerr := s.repo.RestoreReservationQuantityByID(ctx, id, dec); rerr != nil {
				logger.From(ctx, s.logger).Error("ERP refused the decrease and the reservation could not be restored — unit inconsistent between DB and ERP",
					zap.String("cart_id", cartID),
					zap.String("product_id", productID),
					zap.String("reservation_id", id),
					zap.Int("quantity", dec),
					zap.Error(rerr),
				)
			}
		}
		rollbackLocal()
		return "", fmt.Errorf("reversing stock delta in ERP: %w", err)
	}

	logger.From(ctx, s.logger).Info("ERP reservation decreased",
		zap.String("cart_id", cartID),
		zap.String("product_id", productID),
		zap.Int("delta", delta),
		zap.Int("new_qty", res.Remaining),
		zap.String("erp_movement_id", movementID),
	)
	return movementID, nil
}
