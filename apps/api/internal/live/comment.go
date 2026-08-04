package live

import (
	"context"
	"fmt"
	"strings"
	"time"

	"go.uber.org/zap"

	"livecart/apps/api/internal/notification"
	"livecart/apps/api/lib/config"
	"livecart/apps/api/lib/logger"
)

// ProcessInstagramCommentInput is the wire contract the comment.received consumer
// deserializes into. Canonical home is this package (Bloco B4b); integration
// aliases it so the webhook edge and in-flight tasks keep the same shape.
type ProcessInstagramCommentInput struct {
	AccountID string
	MediaID   string
	CommentID string
	UserID    string
	Username  string
	Text      string
	Timestamp int64
	// Channel is the reply channel: "" / "comment" (default) replies on the
	// comment thread with a DM fallback; "dm" replies straight via DM — used by
	// story replies, which arrive as DMs and have no public comment to answer.
	Channel    string
	RawPayload []byte // Original webhook payload for audit storage
	// SignatureValid é o RESULTADO REAL do X-Hub-Signature-256 desta requisição,
	// e viaja colado ao RawPayload de propósito: os dois alimentam a MESMA linha
	// de webhook_events, e uma linha que grava "assinatura válida" sobre um
	// payload cuja checagem falhou é pior que linha nenhuma.
	//
	// É esta coluna que decide o deploy 2 do modo observação: o 401 só entra
	// depois de dias de tráfego 100% válido. Com o `true` fixo que vivia no
	// service, a consulta respondia 100% válido POR CONSTRUÇÃO — inclusive para
	// as requisições que o handler acabara de reprovar e aceitar mesmo assim.
	// Quem lesse o painel ligaria a exigência e derrubaria a captura de
	// comentários da base inteira.
	//
	// Falso por padrão sem consequência: o polling não tem assinatura e também
	// não tem RawPayload, então nem chega a gravar auditoria.
	SignatureValid bool
}

// StoreWebhookInput is the audit-trail record for an inbound webhook event.
// Canonical home here; integration aliases it (WebhookAuditor is declared over it).
type StoreWebhookInput struct {
	StoreID        string // From URL parameter
	Provider       string
	IntegrationID  string // Resolved by service layer before storing
	EventType      string
	EventID        string
	Payload        []byte
	SignatureValid bool
}

// ReserveParams is the NEUTRAL reservation DTO the comment core hands to the
// StockReserver port. It is deliberately NOT erp.ReserveParams: keeping the shape
// local is what lets live stay import-cycle free (erp/inventory already import
// live). The integration adapter maps this to erp.ReserveParams at the boundary.
type ReserveParams struct {
	Op        string
	ProductID string
	CartID    string
	EventID   string
	Quantity  int
}

// stockOpCartAdd labels a cart-add reservation. It is a local string constant
// (mirrors erp.StockOpCartAdd) so live does not import erp; the adapter converts
// it back to the typed erp.StockOp.
const stockOpCartAdd = "cart_add"

// StockReserver notes the definitive local stock reservation (stock.reserved,
// keyed by the real cart) and creates the paired ERP reservation. Satisfied by an
// integration-side adapter that maps ReserveParams → erp.ReserveParams, breaking
// the erp import cycle. Wired via SetStockReserver; nil-guarded at every use.
type StockReserver interface {
	NoteReserved(ctx context.Context, p ReserveParams) error
	ReserveStockInERP(ctx context.Context, storeID, cartID, eventID, productID string, quantity int, unitPrice int64, platformHandle string) error
}

// BillingGate blocks new-cart creation for stores with an inactive subscription
// (PRD 007). Satisfied by *integration.Service. Wired via SetBillingGate.
type BillingGate interface {
	IsStoreBlocked(ctx context.Context, storeID string) bool
}

// WebhookAuditor persists inbound webhook events for the audit trail. Satisfied
// by *integration.Service. Wired via SetWebhookAuditor.
type WebhookAuditor interface {
	StoreWebhookEvent(ctx context.Context, input StoreWebhookInput) error
}

// SocialReplier is the Instagram reply ACL: DM a buyer or answer their comment.
// Satisfied by *integration.Service. Wired via SetSocialReplier.
type SocialReplier interface {
	SendInstagramDM(ctx context.Context, storeID, recipientID, text string) error
	ReplyToInstagramComment(ctx context.Context, storeID, commentID, text string) error
}

// commentCore is the slice of *Service behaviour the comment ingestion core
// reuses. The core calls these through the interface (s.core, defaulting to the
// Service itself) so the flow is unit-testable with fakes and no database —
// mirroring how inventory.Service keeps its live.Service dependency swappable.
type commentCore interface {
	GetSessionByPlatformLiveID(ctx context.Context, platformLiveID string) (*SessionOutput, error)
	GetEventByPlatformLiveID(ctx context.Context, platformLiveID string) (*EventOutput, error)
	AddToCart(ctx context.Context, input AddToCartInput) (AddToCartOutput, error)
	// ListSessionWhitelist lê a lista de produtos vendáveis DA TRANSMISSÃO em
	// que o comentário caiu — sem checagem de posse, porque a sessão já foi
	// resolvida pela mídia que chegou no webhook.
	//
	// Era ListEventProducts (a união do evento). Ler do evento fazia post e
	// story da mesma campanha compartilharem a mesma barreira, que é justamente
	// o que não pode: a live vende qualquer coisa, o post vende só o produto X.
	ListSessionWhitelist(ctx context.Context, sessionID string) ([]SessionProductOutput, error)
}

// ProcessInstagramComment processes a live comment from Instagram webhook.
// All comments are saved to DB. Purchase intents trigger stock check → cart or waitlist.
func (s *Service) ProcessInstagramComment(ctx context.Context, input ProcessInstagramCommentInput) error {
	logger.From(ctx, s.logger).Info("processing instagram comment",
		zap.String("account_id", input.AccountID),
		zap.String("media_id", input.MediaID),
		zap.String("comment_id", input.CommentID),
		zap.String("user_id", input.UserID),
		zap.String("username", input.Username),
		zap.String("text", input.Text),
	)

	// [IGTRACE] TODO remover — investigação da entrega de live_comments.
	//
	// ORIGEM é o campo que faltava para ler o log. Webhook e polling produzem
	// linhas idênticas daqui para baixo, e a diferença entre os dois é
	// justamente o que decide se o comprador recebe a DM: private reply só
	// funciona com o comment_id que a Meta empurra.
	trace := logger.From(ctx, s.logger).With(
		zap.String("comment_id", input.CommentID),
		zap.String("media_id", input.MediaID),
		zap.String("origin", commentOrigin(input)),
	)
	trace.Info(TracePrefix + "comment received")

	// Idempotency guard: a comment can reach us from BOTH the real-time webhook
	// and the polling capture. Skip if we've already stored this comment id, so
	// we never create a duplicate cart for the same comment.
	if input.CommentID != "" {
		if exists, _ := s.ingestRepo.LiveCommentExistsByPlatformID(ctx, input.CommentID); exists {
			logger.From(ctx, s.logger).Info("comment already processed, skipping",
				zap.String("comment_id", input.CommentID),
			)
			trace.Info(TracePrefix + "decision: skipped, comment already stored")
			return nil
		}
	}

	// Find live session by platform_live_id (media_id)
	session, err := s.core.GetSessionByPlatformLiveID(ctx, input.MediaID)
	if err != nil {
		return fmt.Errorf("finding live session: %w", err)
	}
	if session == nil {
		logger.From(ctx, s.logger).Warn("no active live session found for media_id",
			zap.String("media_id", input.MediaID),
		)
		return nil
	}

	// Get the event (which has store_id) from the session
	event, err := s.core.GetEventByPlatformLiveID(ctx, input.MediaID)
	if err != nil {
		return fmt.Errorf("finding live event: %w", err)
	}
	if event == nil {
		trace.Warn(TracePrefix + "decision: dropped, media resolves to no event")
		logger.From(ctx, s.logger).Warn("no active live event found for media_id",
			zap.String("media_id", input.MediaID),
		)
		return nil
	}

	// Store resolved (media_id → event): carry it on the ctx so every log below
	// gets store_id without manual fields. Slug lookup skipped on this hot path.
	ctx = logger.WithStore(ctx, event.StoreID, "")

	// Paywall (PRD 007): blocked stores stop creating carts from comments.
	// Existing checkouts and payment webhooks keep working elsewhere.
	if s.billingGate != nil && s.billingGate.IsStoreBlocked(ctx, event.StoreID) {
		logger.From(ctx, s.logger).Info("comment ignored: store subscription blocked",
			zap.String("comment_id", input.CommentID),
		)
		return nil
	}

	// Store webhook event for audit trail (only if we have payload and store context)
	if len(input.RawPayload) > 0 && s.webhookAuditor != nil {
		if err := s.webhookAuditor.StoreWebhookEvent(ctx, StoreWebhookInput{
			StoreID:        event.StoreID,
			Provider:       "instagram",
			EventType:      "live_comments",
			EventID:        input.CommentID,
			Payload:        input.RawPayload,
			SignatureValid: true, // Instagram webhook signature validation could be added
		}); err != nil {
			logger.From(ctx, s.logger).Error("failed to store instagram webhook event",
				zap.String("comment_id", input.CommentID),
				zap.Error(err),
			)
			// Don't return error - continue processing the comment
		}
	}

	// Increment comment counter on session
	if err := s.ingestRepo.IncrementLiveSessionComments(ctx, session.ID); err != nil {
		logger.From(ctx, s.logger).Error("failed to increment comment counter",
			zap.String("session_id", session.ID),
			zap.Error(err),
		)
	}

	// Modo Live é estado EFÊMERO de execução da TRANSMISSÃO, não da campanha
	// (D17, migration 000113). Ler do evento faria duas sessões simultâneas
	// compartilharem a mesma pausa, e o estado residual de segunda contaminaria
	// a live de quarta.
	if session.ProcessingPaused {
		logger.From(ctx, s.logger).Info("processing paused, storing comment only",
			zap.String("event_id", event.ID),
			zap.String("comment_id", input.CommentID),
			zap.String("username", input.Username),
		)

		// Save comment with "paused" result but don't process cart
		_, err := s.ingestRepo.CreateLiveComment(ctx, CreateLiveCommentParams{
			SessionID:         session.ID,
			EventID:           event.ID,
			Platform:          "instagram",
			PlatformCommentID: input.CommentID,
			PlatformUserID:    input.UserID,
			PlatformHandle:    input.Username,
			Text:              input.Text,
			HasPurchaseIntent: false, // Don't parse when paused
			Result:            "paused",
		})
		if err != nil {
			logger.From(ctx, s.logger).Error("failed to save paused comment", zap.Error(err))
		}
		return nil
	}

	// Block list: if the merchant has blocked this handle, drop the comment
	// from the purchase flow. Still persist it with result='blocked' so the
	// live feed can show "ignorado" badge and the merchant can see that the
	// person is still trying to buy.
	blocked, blockErr := s.ingestRepo.IsHandleBlocked(ctx, event.StoreID, strings.ToLower(strings.TrimPrefix(strings.TrimSpace(input.Username), "@")))
	if blockErr != nil {
		logger.From(ctx, s.logger).Error("failed to check blocked handle, proceeding",
			zap.String("username", input.Username),
			zap.Error(blockErr),
		)
	} else if blocked {
		logger.From(ctx, s.logger).Info("comment from blocked handle ignored",
			zap.String("event_id", event.ID),
			zap.String("username", input.Username),
			zap.String("comment_id", input.CommentID),
		)
		_, err := s.ingestRepo.CreateLiveComment(ctx, CreateLiveCommentParams{
			SessionID:         session.ID,
			EventID:           event.ID,
			Platform:          "instagram",
			PlatformCommentID: input.CommentID,
			PlatformUserID:    input.UserID,
			PlatformHandle:    input.Username,
			Text:              input.Text,
			HasPurchaseIntent: false,
			Result:            "blocked",
		})
		if err != nil {
			logger.From(ctx, s.logger).Error("failed to save blocked comment", zap.Error(err))
		}
		return nil
	}

	// Parse purchase intent
	intent := ParsePurchaseIntent(input.Text)
	hasPurchaseIntent := intent != nil
	if hasPurchaseIntent {
		trace.Info(TracePrefix+"decision: purchase intent detected", zap.Int("quantity", intent.Quantity))
	} else {
		trace.Info(TracePrefix + "decision: no purchase intent, comment is just stored")
	}

	// Try to match product by keyword
	var product *ProductRow
	if hasPurchaseIntent {
		product = s.FindProductByKeyword(ctx, event.StoreID, input.Text)

		// If no keyword match but has purchase intent, try active product as fallback
		// Produto em destaque também é da sessão (D17).
		if product == nil && session.CurrentActiveProductID != nil && *session.CurrentActiveProductID != "" {
			logger.From(ctx, s.logger).Info("no keyword match, trying active product fallback",
				zap.String("session_id", session.ID),
				zap.String("active_product_id", *session.CurrentActiveProductID),
			)
			product, _ = s.ingestRepo.GetProductByID(ctx, event.StoreID, *session.CurrentActiveProductID)
		}
	}

	// Gate de janela, para TODO tipo de evento (RN-18/19/20).
	//
	// Ele vivia dentro do ramo isPostCommerce, então para live (single/multi)
	// não existia checagem nenhuma de janela: a única proteção era o
	// status='active' exigido pelas queries de resolução — e esse filtro saiu,
	// para o sistema parar de descartar comentário em silêncio. Sem o gate aqui,
	// comentar numa campanha encerrada criaria carrinho e reservaria estoque.
	//
	// Só vale com intenção de compra: comentário qualquer em campanha encerrada
	// não merece resposta automática.
	if hasPurchaseIntent {
		switch WindowAt(event.Status, event.ScheduledAt, event.EndsAt, time.Now()) {
		case WindowNotStarted:
			trace.Info(TracePrefix + "decision: refused, event window has not opened")
			s.replyOutOfWindow(ctx, event, input, notification.TypeOutOfWindowScheduled)
			s.savePostComment(ctx, session.ID, event.ID, input, "event_not_started")
			return nil
		case WindowEnded:
			trace.Info(TracePrefix + "decision: refused, event window has closed")
			s.replyOutOfWindow(ctx, event, input, notification.TypeOutOfWindowEventEnded)
			s.savePostComment(ctx, session.ID, event.ID, input, "event_ended")
			return nil
		}
		// A transmissão pode ter acabado com a campanha ainda aberta — a venda
		// não entra, mas o comprador é avisado (RN-18).
		if !SessionAcceptsPurchase(session.Status) {
			trace.Info(TracePrefix+"decision: refused, session no longer accepts purchase",
				zap.String("session_status", session.Status))
			s.replyOutOfWindow(ctx, event, input, notification.TypeOutOfWindowSessionEnded)
			s.savePostComment(ctx, session.ID, event.ID, input, "session_ended")
			return nil
		}
	}

	// Regras de post-commerce: só os produtos selecionados participam, promoção
	// de produto único auto-adiciona num "EU QUERO" pelado, e pedido indisponível
	// ou ambíguo recebe resposta privada. A espécie agora vem da SESSÃO (D3), não
	// de live_events.type, que a 000122 dropou.
	//
	// A lista consultada é a DESTA transmissão, não a do evento: é o que permite
	// que o post e o story da mesma campanha tenham barreiras diferentes.
	if IsPostCommerceSessionType(session.Type) && hasPurchaseIntent {
		resolved, handled, resultLabel := s.resolvePostEventProduct(ctx, event, session, input, intent, product)
		if handled {
			s.savePostComment(ctx, session.ID, event.ID, input, resultLabel)
			return nil
		}
		product = resolved
	}

	// Determine result for the comment record
	var commentResult string
	var matchedProductID string
	var matchedQuantity int
	if !hasPurchaseIntent {
		commentResult = "no_intent"
	} else if product == nil {
		commentResult = "no_product"
	}
	if product != nil && intent != nil {
		matchedProductID = product.ID
		matchedQuantity = intent.Quantity
	}

	// Save ALL comments to DB
	commentID, err := s.ingestRepo.CreateLiveComment(ctx, CreateLiveCommentParams{
		SessionID:         session.ID,
		EventID:           event.ID,
		Platform:          "instagram",
		PlatformCommentID: input.CommentID,
		PlatformUserID:    input.UserID,
		PlatformHandle:    input.Username,
		Text:              input.Text,
		HasPurchaseIntent: hasPurchaseIntent,
		MatchedProductID:  matchedProductID,
		MatchedQuantity:   matchedQuantity,
		Result:            commentResult,
	})
	if err != nil {
		logger.From(ctx, s.logger).Error("failed to save live comment", zap.Error(err))
		// Continue processing even if save fails
	}

	// If no purchase intent or no product match, we're done
	if hasPurchaseIntent && product == nil {
		trace.Info(TracePrefix + "decision: intent without a matching product")
	}
	if !hasPurchaseIntent || product == nil {
		return nil
	}

	logger.From(ctx, s.logger).Info("purchase intent detected with product match",
		zap.String("username", input.Username),
		zap.String("product_id", product.ID),
		zap.String("keyword", product.Keyword),
		zap.Int("quantity", intent.Quantity),
		zap.Int("stock", product.Stock),
	)

	// A expiração de carrinhos deixou de ser lazy (por-comentário, por-produto):
	// era cega a carts 'checkout' pós-live e corria com o webhook de pagamento
	// sem lock. Agora a schedule asynq (cart.expire → RunScheduledExpiry →
	// ExpireCart) expira por-cart, com advisory lock, filtro de pago e devolução
	// de TODOS os itens.
	// A promoção de waitlist do produto liberado passou a ser disparada pelo
	// próprio worker; aqui não é mais necessário nada.

	// Validate maxQuantityPerItem limit
	storeInfo, _ := s.ingestRepo.GetStoreInfo(ctx, event.StoreID)
	if storeInfo != nil && storeInfo.MaxQuantityPerItem > 0 {
		currentQty, _ := s.ingestRepo.GetProductQuantityInUserCart(ctx, event.ID, input.UserID, product.ID)
		maxAllowed := storeInfo.MaxQuantityPerItem

		if currentQty >= maxAllowed {
			// User already has max quantity, ignore this request
			logger.From(ctx, s.logger).Info("user already at max quantity for product, ignoring",
				zap.String("username", input.Username),
				zap.String("product_id", product.ID),
				zap.Int("current_qty", currentQty),
				zap.Int("max_allowed", maxAllowed),
			)
			if commentID != "" {
				_ = s.ingestRepo.UpdateLiveCommentResult(ctx, commentID, false, product.ID, intent.Quantity, "max_quantity_reached")
			}
			// Send reply notifying user they've reached the limit.
			// Detached goroutine: never carry the (recyclable) request ctx —
			// hand it a Background ctx enriched with the store instead.
			go s.sendMaxQuantityReply(logger.WithStore(context.Background(), event.StoreID, ""), event.StoreID, input.Channel, input.CommentID, input.UserID, input.Username, product.Name, maxAllowed, true)
			return nil
		}

		// Cap quantity to remaining allowed
		remaining := maxAllowed - currentQty
		if intent.Quantity > remaining {
			logger.From(ctx, s.logger).Info("capping quantity to max allowed",
				zap.String("username", input.Username),
				zap.String("product_id", product.ID),
				zap.Int("requested", intent.Quantity),
				zap.Int("capped_to", remaining),
			)
			// Send reply notifying user their quantity was capped.
			// Detached goroutine: same ctx rule as above.
			go s.sendMaxQuantityReply(logger.WithStore(context.Background(), event.StoreID, ""), event.StoreID, input.Channel, input.CommentID, input.UserID, input.Username, product.Name, maxAllowed, false)
			intent.Quantity = remaining
		}
	}

	// Calculate partial fulfillment: how many available vs waitlisted
	availableQty := intent.Quantity
	if product.Stock < intent.Quantity {
		availableQty = product.Stock
	}
	if availableQty < 0 {
		availableQty = 0
	}
	waitlistQty := intent.Quantity - availableQty
	trace.Info(TracePrefix+"decision: stock split",
		zap.Int("requested", intent.Quantity),
		zap.Int("from_stock", availableQty),
		zap.Int("to_waitlist", waitlistQty),
		zap.Int("product_stock", product.Stock))

	// Reserve available stock (provisional: rolled back below if AddToCart
	// fails). stock.reserved is emitted only after the add succeeds, with the
	// real cart_id — see NoteReserved after AddToCart.
	if availableQty > 0 {
		if stockErr := s.ingestRepo.DecrementProductStock(ctx, product.ID, availableQty); stockErr != nil {
			// Failed to reserve even available stock - put all in waitlist
			logger.From(ctx, s.logger).Warn("failed to decrement stock, putting all in waitlist",
				zap.Error(stockErr),
				zap.Int("attempted", availableQty),
			)
			availableQty = 0
			waitlistQty = intent.Quantity
		}
	}

	// Handle waitlist gating: if user already has a row, skip the waitlist
	// portion (we don't double-queue) and either return early or fall back
	// to adding only the available portion to the cart.
	createWaitlistRow := false
	var waitlistPosition int
	if waitlistQty > 0 {
		alreadyWaiting, _ := s.ingestRepo.GetWaitlistItemByEventUserProduct(ctx, event.ID, input.UserID, product.ID)
		if alreadyWaiting {
			logger.From(ctx, s.logger).Info("user already on waitlist, ignoring waitlist portion",
				zap.String("username", input.Username),
				zap.String("product_id", product.ID),
				zap.Int("waitlist_qty", waitlistQty),
			)
			if availableQty == 0 {
				if commentID != "" {
					_ = s.ingestRepo.UpdateLiveCommentResult(ctx, commentID, true, product.ID, intent.Quantity, "already_waitlisted")
				}
				return nil
			}
			waitlistQty = 0
		} else {
			// Defer the actual INSERT to after AddToCart so we can stamp
			// cart_id on the row (the public checkout lists waitlist items
			// by cart_id). Position is read here to keep ordering stable
			// even if two intents race on the same event+product.
			waitlistPosition, _ = s.ingestRepo.GetNextWaitlistPosition(ctx, event.ID, product.ID)
			createWaitlistRow = true
		}
	}

	// Determine total quantity to add to cart
	totalQtyToAdd := availableQty + waitlistQty
	if totalQtyToAdd == 0 {
		// Nothing to add
		return nil
	}

	// Add product to cart with partial fulfillment
	result, err := s.core.AddToCart(ctx, AddToCartInput{
		StoreID:            event.StoreID,
		EventID:            event.ID,
		SessionID:          session.ID,
		PlatformUserID:     input.UserID,
		PlatformHandle:     input.Username,
		ProductID:          product.ID,
		ProductPrice:       product.Price,
		Quantity:           totalQtyToAdd,
		WaitlistedQuantity: waitlistQty,
	})
	if err != nil {
		// If we reserved stock but failed to add to cart, release it
		if availableQty > 0 {
			_ = s.ingestRepo.IncrementProductStock(ctx, product.ID, availableQty)
		}
		return fmt.Errorf("adding to cart: %w", err)
	}

	trace.Info(TracePrefix+"decision: cart updated",
		zap.String("cart_id", result.CartID),
		zap.Bool("new_cart", result.IsNewCart),
		zap.Int("total_items", result.TotalItems))

	// Reservation is now definitive — emit stock.reserved keyed by the real cart.
	if availableQty > 0 && s.stockReserver != nil {
		if noteErr := s.stockReserver.NoteReserved(ctx, ReserveParams{Op: stockOpCartAdd, ProductID: product.ID, Quantity: availableQty, CartID: result.CartID, EventID: event.ID}); noteErr != nil {
			logger.From(ctx, s.logger).Warn("failed to note stock reservation",
				zap.String("cart_id", result.CartID),
				zap.Error(noteErr),
			)
		}
	}

	// Persist the waitlist row now that we have the cart_id from AddToCart.
	if createWaitlistRow {
		if _, wlErr := s.ingestRepo.CreateWaitlistItem(ctx, CreateWaitlistItemParams{
			EventID:        event.ID,
			ProductID:      product.ID,
			PlatformUserID: input.UserID,
			PlatformHandle: input.Username,
			Quantity:       waitlistQty,
			Position:       waitlistPosition,
			CartID:         result.CartID,
		}); wlErr != nil {
			logger.From(ctx, s.logger).Error("failed to create waitlist item", zap.Error(wlErr))
		} else {
			logger.From(ctx, s.logger).Info("user added to waitlist (partial fulfillment)",
				zap.String("username", input.Username),
				zap.String("product_id", product.ID),
				zap.String("cart_id", result.CartID),
				zap.Int("available_qty", availableQty),
				zap.Int("waitlist_qty", waitlistQty),
				zap.Int("position", waitlistPosition),
			)
		}
	}

	// Update comment result
	if commentID != "" {
		if waitlistQty > 0 && availableQty > 0 {
			_ = s.ingestRepo.UpdateLiveCommentResult(ctx, commentID, true, product.ID, intent.Quantity, "partial_fulfillment")
		} else if waitlistQty > 0 {
			_ = s.ingestRepo.UpdateLiveCommentResult(ctx, commentID, true, product.ID, intent.Quantity, "waitlisted")
		} else {
			_ = s.ingestRepo.UpdateLiveCommentResult(ctx, commentID, true, product.ID, intent.Quantity, "added_to_cart")
		}
	}

	// Increment order counter on event only for new carts
	if result.IsNewCart {
		if err := s.ingestRepo.IncrementLiveEventOrders(ctx, event.ID); err != nil {
			logger.From(ctx, s.logger).Error("failed to increment order counter",
				zap.String("event_id", event.ID),
				zap.Error(err),
			)
		}
	}

	// Reserve stock in ERP (only for available items)
	if availableQty > 0 && s.stockReserver != nil {
		if syncErr := s.stockReserver.ReserveStockInERP(ctx, event.StoreID, result.CartID, event.ID, product.ID, availableQty, product.Price, input.Username); syncErr != nil {
			logger.From(ctx, s.logger).Warn("failed to reserve stock in ERP",
				zap.String("cart_id", result.CartID),
				zap.Error(syncErr),
			)
		}
	}

	// Send immediate notification (fire-and-forget, doesn't block the flow). For
	// story replies (Channel="dm") there is no comment to reply on, so we clear
	// the comment id — the notification service then delivers straight via DM to
	// the buyer's IGSID.
	notifyCommentID := input.CommentID
	if input.Channel == "dm" {
		notifyCommentID = ""
	}
	s.sendImmediateNotification(ctx, sendNotificationInput{
		StoreID:           event.StoreID,
		EventID:           event.ID,
		EventTitle:        event.Title,
		CartID:            result.CartID,
		CartToken:         result.CartToken,
		PlatformUserID:    input.UserID,
		PlatformHandle:    input.Username,
		PlatformCommentID: notifyCommentID,
		ProductName:       product.Name,
		ProductKeyword:    product.Keyword,
		Quantity:          intent.Quantity,
		TotalItems:        result.TotalItems,
		TotalCents:        result.TotalCents,
		IsNewCart:         result.IsNewCart,
	})

	return nil
}

// sendNotificationInput contains all data needed for immediate notifications.
type sendNotificationInput struct {
	StoreID           string
	EventID           string
	EventTitle        string
	CartID            string
	CartToken         string
	PlatformUserID    string
	PlatformHandle    string
	PlatformCommentID string // Instagram comment ID for reply
	ProductName       string
	ProductKeyword    string
	Quantity          int
	TotalItems        int
	TotalCents        int64
	IsNewCart         bool
}

// sendImmediateNotification sends an immediate checkout notification via the notification service.
// This is fire-and-forget - errors are logged but don't affect the main flow.
func (s *Service) sendImmediateNotification(ctx context.Context, input sendNotificationInput) {
	// Skip if notification service not configured
	if s.notificationSvc == nil {
		logger.From(ctx, s.logger).Warn("no DM sent: notification service not configured",
			zap.String("cart_id", input.CartID),
		)
		return
	}

	// Determine notification type based on whether this is a new cart or adding to existing
	notifType := notification.TypeCheckoutImmediate
	if !input.IsNewCart {
		notifType = notification.TypeItemAdded
	}

	// Check if we should notify based on store settings
	shouldNotify, err := s.notificationSvc.ShouldNotify(ctx, input.StoreID, notifType, input.IsNewCart)
	if err != nil {
		logger.From(ctx, s.logger).Warn("failed to check notification settings",
			zap.Error(err),
		)
		return
	}
	if !shouldNotify {
		// O carrinho existe, o estoque já foi reservado e o comprador não vai
		// receber nada. Isso é indistinguível de uma falha quando não aparece
		// no log — foi o que fez um teste em staging parecer que o webhook não
		// tinha chegado, quando na verdade o portão é que recusou.
		//
		// Info, não Warn: o lojista TEM o direito de desligar a DM em tempo
		// real, e nesse caso isto é operação normal, não defeito.
		logger.From(ctx, s.logger).Info("no DM sent: notification disabled for this store",
			zap.String("cart_id", input.CartID),
			zap.String("notification_type", string(notifType)),
			zap.Bool("is_new_cart", input.IsNewCart),
		)
		return
	}

	// Get store info for notification
	storeInfo, err := s.ingestRepo.GetStoreInfo(ctx, input.StoreID)
	if err != nil {
		logger.From(ctx, s.logger).Warn("failed to get store info for notification",
			zap.Error(err),
		)
		return
	}

	// Build checkout URL
	frontendURL := config.FrontendURL.StringOr("http://localhost:3000")
	checkoutURL := fmt.Sprintf("%s/cart/%s", frontendURL, input.CartToken)

	// Build template variables
	vars := notification.TemplateVariables{
		Handle:     "@" + input.PlatformHandle,
		Produto:    input.ProductName,
		Keyword:    input.ProductKeyword,
		Quantidade: input.Quantity,
		TotalItens: input.TotalItems,
		Total:      notification.FormatCurrency(input.TotalCents),
		TotalCents: input.TotalCents,
		Link:       checkoutURL,
		Loja:       storeInfo.Name,
		ExpiraEm:   notification.FormatExpiryMinutes(storeInfo.CartExpirationMinutes),
		LiveTitulo: input.EventTitle,
	}

	// Send notification
	result, err := s.notificationSvc.Send(ctx, notification.SendInput{
		StoreID:           input.StoreID,
		EventID:           input.EventID,
		CartID:            input.CartID,
		CartToken:         input.CartToken,
		PlatformUserID:    input.PlatformUserID,
		PlatformHandle:    input.PlatformHandle,
		PlatformCommentID: input.PlatformCommentID,
		NotificationType:  notifType,
		Variables:         vars,
	})

	if err != nil {
		logger.From(ctx, s.logger).Warn("notification send error",
			zap.String("cart_id", input.CartID),
			zap.Error(err),
		)
		return
	}

	logger.From(ctx, s.logger).Info("immediate notification processed",
		zap.String("cart_id", input.CartID),
		zap.String("status", string(result.Status)),
		zap.Bool("is_new_cart", input.IsNewCart),
	)
}

// sendMaxQuantityReply sends a reply to the user when they've reached or exceeded the max quantity limit.
// This is fire-and-forget - errors are logged but don't affect the main flow.
// isAtLimit: true = already at limit (rejected), false = quantity was capped
func (s *Service) sendMaxQuantityReply(ctx context.Context, storeID, channel, commentID, userID, username, productName string, maxAllowed int, isAtLimit bool) {
	if s.socialReplier == nil {
		return
	}

	var message string
	if isAtLimit {
		message = fmt.Sprintf("Oi @%s! Você já atingiu o limite de %d unidades de %s. 🛒", username, maxAllowed, productName)
	} else {
		message = fmt.Sprintf("Oi @%s! Adicionei o máximo permitido (%d unidades) de %s ao seu carrinho. 🛒", username, maxAllowed, productName)
	}

	// Story replies have no comment to answer — DM the buyer directly.
	if channel == "dm" {
		if dmErr := s.socialReplier.SendInstagramDM(ctx, storeID, userID, message); dmErr != nil {
			logger.From(ctx, s.logger).Warn("failed to send max quantity DM",
				zap.String("user_id", userID), zap.Error(dmErr))
		}
		return
	}

	if commentID == "" {
		return
	}

	// Try comment reply first, then DM fallback
	err := s.socialReplier.ReplyToInstagramComment(ctx, storeID, commentID, message)
	if err != nil {
		logger.From(ctx, s.logger).Warn("failed to send max quantity reply via comment, trying DM",
			zap.String("comment_id", commentID),
			zap.Error(err),
		)
		// Fallback to DM
		if dmErr := s.socialReplier.SendInstagramDM(ctx, storeID, userID, message); dmErr != nil {
			logger.From(ctx, s.logger).Warn("failed to send max quantity DM",
				zap.String("user_id", userID),
				zap.Error(dmErr),
			)
		}
	}

	logger.From(ctx, s.logger).Info("max quantity reply sent",
		zap.String("username", username),
		zap.String("product", productName),
		zap.Int("max_allowed", maxAllowed),
		zap.Bool("is_at_limit", isAtLimit),
	)
}

// =============================================================================
// POST-COMMERCE COMMENT RULES
// =============================================================================

// A cópia local de isPostCommerce SAIU. Ela dizia "post ou story" e esquecia
// 'reel' — exatamente o modo de falha que IsPostCommerceSessionType (session_type.go)
// documenta. Enquanto o tipo vinha do EVENTO isso não aparecia, porque o
// vocabulário antigo achatava reel em post ao gravar live_events.type; desde a
// 000122 a sessão diz mesmo 'reel', e a cópia passou a deixar todo Reel sem
// barreira nenhuma: os dois caminhos que criam um reel EXIGEM pelo menos um
// produto, gravavam a lista na sessão, e a ingestão ignorava a lista e vendia o
// catálogo inteiro. Uma regra, um lugar.

// resolvePostEventProduct applies post-event rules. It returns the product to
// add (resolved from a single-product promotion when the comment is a bare
// "EU QUERO"), and handled=true when it already answered the commenter (product
// not in the promotion, or ambiguous request), in which case the caller saves
// the comment with resultLabel and stops.
//
// A lista é lida da SESSÃO em que o comentário caiu. Antes vinha da união do
// evento, o que colava a barreira do post na do story da mesma campanha.
func (s *Service) resolvePostEventProduct(
	ctx context.Context,
	event *EventOutput,
	session *SessionOutput,
	input ProcessInstagramCommentInput,
	intent *PurchaseIntent,
	matched *ProductRow,
) (resolved *ProductRow, handled bool, resultLabel string) {
	whitelist, err := s.core.ListSessionWhitelist(ctx, session.ID)
	if err != nil {
		logger.From(ctx, s.logger).Warn("failed to load session promotion products", zap.Error(err))
		return matched, false, ""
	}

	// Regra ÚNICA em todo o sistema: lista VAZIA libera TODOS os produtos ativos
	// da loja nesta transmissão. Até aqui a ingestão fazia o OPOSTO do checkout —
	// sem lista, todo produto casado caía em 'not_in_promo' e o comprador ouvia
	// "não está disponível nesta promoção". Com a herança fora, toda sessão nova
	// nasce vazia: sem este bypass, ela recusaria 100% dos comentários.
	openSession := len(whitelist) == 0

	inPromo := func(productID string) bool {
		if openSession {
			return true
		}
		for _, w := range whitelist {
			if w.ProductID == productID {
				return true
			}
		}
		return false
	}

	// Case A: a code matched a real product.
	if matched != nil {
		if inPromo(matched.ID) {
			if matched.Stock <= 0 {
				s.replyPostOutOfStock(ctx, event, input, matched.Name, whitelist)
				return nil, true, "out_of_stock"
			}
			return matched, false, "" // proceed to the normal cart flow
		}
		s.replyPostUnavailable(ctx, event, input, whitelist)
		return nil, true, "not_in_promo"
	}

	// Case B: no product matched. A typed-but-unknown code is "unavailable";
	// a bare trigger with one promo product auto-adds it, with many it asks.
	if codes := ExtractPossibleKeywords(input.Text); len(codes) > 0 {
		// Numa transmissão sem lista, um código que não casou não é "fora da
		// promoção": ele simplesmente não existe no catálogo. Responder
		// "não está disponível nesta promoção" seria mentira, e listar os
		// disponíveis é impossível (são todos). Silêncio, como na live.
		if openSession {
			return nil, true, "no_product"
		}
		s.replyPostUnavailable(ctx, event, input, whitelist)
		return nil, true, "not_in_promo"
	}

	// "EU QUERO" pelado numa transmissão sem lista: não há como adivinhar o
	// produto de uma loja inteira, e pedir o código listando os disponíveis
	// significaria despejar o catálogo na DM. Fica sem produto.
	if openSession {
		return nil, true, "no_product"
	}

	available := availablePromoProducts(whitelist)
	switch len(available) {
	case 1:
		p, err := s.ingestRepo.GetProductByID(ctx, event.StoreID, available[0].ProductID)
		if err == nil && p != nil {
			return p, false, ""
		}
		return nil, true, "no_product"
	case 0:
		// No available products: if the promotion has products but all are out
		// of stock, tell the buyer; otherwise stay silent.
		if len(whitelist) > 0 {
			s.replyPostOutOfStock(ctx, event, input, "", whitelist)
			return nil, true, "out_of_stock"
		}
		return nil, true, "no_product"
	default:
		s.replyPostChooseProduct(ctx, event, input, available)
		return nil, true, "needs_keyword"
	}
}

// savePostComment persists a post comment that was fully handled by the rules.
func (s *Service) savePostComment(ctx context.Context, sessionID, eventID string, input ProcessInstagramCommentInput, result string) {
	if _, err := s.ingestRepo.CreateLiveComment(ctx, CreateLiveCommentParams{
		SessionID:         sessionID,
		EventID:           eventID,
		Platform:          "instagram",
		PlatformCommentID: input.CommentID,
		PlatformUserID:    input.UserID,
		PlatformHandle:    input.Username,
		Text:              input.Text,
		HasPurchaseIntent: true,
		Result:            result,
	}); err != nil {
		logger.From(ctx, s.logger).Error("failed to save post comment", zap.Error(err))
	}
}

// replyPostNotStarted privately tells the buyer the promotion hasn't started and
// when it will (formatted in Brazil time, UTC-3).
func (s *Service) replyPostNotStarted(ctx context.Context, event *EventOutput, input ProcessInstagramCommentInput, startsAt time.Time) {
	msg := fmt.Sprintf(
		"Oi @%s! Esta promoção ainda não começou. 🗓️\nEla começa em %s. Volte lá pra garantir o seu! 💜",
		input.Username, FormatBRT(startsAt),
	)
	s.sendPostReply(ctx, event, input, msg)
}

// replyPostEnded privately tells the buyer the promotion has ended.
func (s *Service) replyPostEnded(ctx context.Context, event *EventOutput, input ProcessInstagramCommentInput) {
	msg := fmt.Sprintf("Oi @%s! Esta promoção já foi encerrada. 😕 Fique de olho que logo teremos novidades! 💜", input.Username)
	s.sendPostReply(ctx, event, input, msg)
}

// replyPostOutOfStock privately tells the buyer the product is sold out and
// lists what's still available (when there is anything).
func (s *Service) replyPostOutOfStock(ctx context.Context, event *EventOutput, input ProcessInstagramCommentInput, productName string, whitelist []EventProductOutput) {
	available := availablePromoProducts(whitelist)
	var msg string
	switch {
	case productName != "" && len(available) > 0:
		msg = fmt.Sprintf("Oi @%s! O produto %s esgotou. 😕\nAinda temos:\n%s\n\nComente o código do que você quer. 💜", input.Username, productName, promoProductLines(available))
	case productName != "":
		msg = fmt.Sprintf("Oi @%s! O produto %s esgotou. 😕", input.Username, productName)
	default:
		msg = fmt.Sprintf("Oi @%s! Os produtos desta promoção esgotaram. 😕 Fique de olho nas próximas! 💜", input.Username)
	}
	s.sendPostReply(ctx, event, input, msg)
}

// sendPostReply privately answers the buyer. For a comment-channel event it
// replies on the comment thread (which Instagram delivers as a private reply);
// for a story (Channel="dm") it messages the buyer's IGSID directly, since a
// story reply arrives as a DM and has no public comment to answer.
func (s *Service) sendPostReply(ctx context.Context, event *EventOutput, input ProcessInstagramCommentInput, msg string) {
	if s.socialReplier == nil {
		return
	}
	if input.Channel == "dm" {
		if err := s.socialReplier.SendInstagramDM(ctx, event.StoreID, input.UserID, msg); err != nil {
			logger.From(ctx, s.logger).Warn("failed to send story DM reply",
				zap.String("event_id", event.ID),
				zap.String("user_id", input.UserID),
				zap.Error(err),
			)
		}
		return
	}
	if err := s.socialReplier.ReplyToInstagramComment(ctx, event.StoreID, input.CommentID, msg); err != nil {
		logger.From(ctx, s.logger).Warn("failed to send post reply",
			zap.String("event_id", event.ID),
			zap.String("comment_id", input.CommentID),
			zap.Error(err),
		)
	}
}

// availablePromoProducts filters the promotion to active, in-stock products.
func availablePromoProducts(whitelist []EventProductOutput) []EventProductOutput {
	out := make([]EventProductOutput, 0, len(whitelist))
	for _, w := range whitelist {
		if w.ProductActive && w.Stock > 0 {
			out = append(out, w)
		}
	}
	return out
}

// promoProductLines renders "• CODE — Name" lines for a list of products.
func promoProductLines(products []EventProductOutput) string {
	var b strings.Builder
	for _, p := range products {
		b.WriteString(fmt.Sprintf("• %s — %s\n", p.Keyword, p.Name))
	}
	return strings.TrimRight(b.String(), "\n")
}

// replyPostUnavailable privately tells the commenter the product isn't in this
// promotion and lists what is available.
func (s *Service) replyPostUnavailable(ctx context.Context, event *EventOutput, input ProcessInstagramCommentInput, whitelist []EventProductOutput) {
	available := availablePromoProducts(whitelist)
	var msg string
	if len(available) == 0 {
		msg = fmt.Sprintf("Oi @%s! Esse produto não está disponível nesta promoção no momento. 😕", input.Username)
	} else {
		msg = fmt.Sprintf(
			"Oi @%s! Esse produto não está disponível nesta promoção. 😕\nDisponíveis nesta publicação:\n%s\n\nComente o código do produto que você quer. 💜",
			input.Username, promoProductLines(available),
		)
	}
	s.sendPostReply(ctx, event, input, msg)
}

// replyPostChooseProduct privately asks the commenter to specify which product
// (used when a bare "EU QUERO" is posted on a multi-product promotion).
func (s *Service) replyPostChooseProduct(ctx context.Context, event *EventOutput, input ProcessInstagramCommentInput, available []EventProductOutput) {
	msg := fmt.Sprintf(
		"Oi @%s! Pra adicionar ao carrinho, comente o código do produto que você quer:\n%s 💜",
		input.Username, promoProductLines(available),
	)
	s.sendPostReply(ctx, event, input, msg)
}

// replyOutOfWindow responde ao comprador que comentou fora da janela — campanha
// agendada, transmissão encerrada ou campanha encerrada (RN-18/19/20).
//
// O texto é configurável pelo lojista (RN-28). Se o serviço de notificação não
// estiver ligado, cai no texto fixo em vez de ficar em SILÊNCIO: descartar
// comentário sem resposta é o achado mais caro da análise de produção, e um
// texto genérico é melhor que nenhum.
func (s *Service) replyOutOfWindow(
	ctx context.Context,
	event *EventOutput,
	input ProcessInstagramCommentInput,
	notifType notification.NotificationType,
) {
	if s.notificationSvc != nil {
		if ok, err := s.notificationSvc.ShouldNotify(ctx, event.StoreID, notifType, false); err == nil && ok {
			vars := notification.TemplateVariables{
				Handle:     "@" + input.Username,
				LiveTitulo: event.Title,
			}
			if notifType == notification.TypeOutOfWindowScheduled && event.ScheduledAt != nil {
				vars.ComecaEm = FormatBRT(*event.ScheduledAt)
			}

			// Story chega como DM e não tem comentário público para responder.
			directOnly := input.Channel == "dm"
			commentID := input.CommentID
			if directOnly {
				commentID = ""
			}

			commentAt := time.Time{}
			if input.Timestamp > 0 {
				commentAt = time.Unix(input.Timestamp, 0)
			}

			if _, err := s.notificationSvc.Send(ctx, notification.SendInput{
				StoreID:           event.StoreID,
				EventID:           event.ID,
				PlatformUserID:    input.UserID,
				PlatformHandle:    input.Username,
				PlatformCommentID: commentID,
				NotificationType:  notifType,
				Variables:         vars,
				CommentCreatedAt:  commentAt,
				DirectOnly:        directOnly,
			}); err != nil {
				logger.From(ctx, s.logger).Warn("out-of-window reply send error",
					zap.String("event_id", event.ID),
					zap.String("type", string(notifType)),
					zap.Error(err),
				)
			}
			return
		}
	}

	switch notifType {
	case notification.TypeOutOfWindowScheduled:
		if event.ScheduledAt != nil {
			s.replyPostNotStarted(ctx, event, input, *event.ScheduledAt)
			return
		}
	case notification.TypeOutOfWindowSessionEnded:
		s.sendPostReply(ctx, event, input, fmt.Sprintf(
			"Oi @%s! Esta transmissão já encerrou, mas a campanha %s continua. Fica de olho na próxima! 💜",
			input.Username, event.Title))
		return
	}
	s.replyPostEnded(ctx, event, input)
}
