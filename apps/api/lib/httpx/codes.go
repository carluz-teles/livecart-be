package httpx

// Code is a stable, UPPER_SNAKE_CASE machine code the frontend maps to a rendered
// message. It travels on the wire as ServiceError.Reason → Envelope.reason.
// Adding a code here is the ONLY sanctioned way to introduce a new FE-facing
// reason — this file is the single source of truth mirrored by livecart-fe.
//
// Codes are stable forever once shipped (the FE keys off them); renaming one is a
// breaking change requiring a coordinated FE change. Each code is anchored to the
// real throw site(s) it will eventually cover — the migration of those sites is a
// later slice (D1c+), NOT part of this foundation. Line numbers are as of HEAD
// 2e5b1d9.
type Code string

func (c Code) String() string { return string(c) }

const (
	// --- generic (non-domain, client-safe) ---
	CodeInternal         Code = "INTERNAL"          // repo/infra failures collapse to this on the wire
	CodeValidationFailed Code = "VALIDATION_FAILED" // ozzo/fields channel (errorhandler.go)

	// --- CART / CHECKOUT (internal/checkout/service.go, internal/coupon/service.go) ---
	CodeCartExpired        Code = "CART_EXPIRED"          // checkout/service.go:132,384,579,746,952,1430,1445; coupon/service.go:250
	CodeCartAlreadyPaid    Code = "CART_ALREADY_PAID"     // checkout/service.go:387,582,749,955,1439; checkout/shipping.go:598
	CodeCartNotPayable     Code = "CART_NOT_PAYABLE"      // checkout/service.go:391,586,753,959 ("não está disponível para checkout")
	CodeCartEmpty          Code = "CART_EMPTY"            // coupon/service.go:256 ("cart is empty")
	CodeCartNoItemsPayable Code = "CART_NO_ITEMS_PAYABLE" // checkout/service.go:429,605,792,998 ("não tem itens disponíveis")

	// --- PAYMENT (internal/checkout/service.go) ---
	CodePaymentNotConfigured Code = "PAYMENT_NOT_CONFIGURED" // checkout/service.go:664,727 (was "payment_not_configured")
	CodePaymentUnavailable   Code = "PAYMENT_UNAVAILABLE"    // checkout/service.go:704 (was "payment_unavailable")
	CodePaymentLinkFailed    Code = "PAYMENT_LINK_FAILED"    // checkout/service.go:485 ("erro ao gerar link de pagamento")

	// --- SHIPPING (internal/checkout/shipping.go) ---
	CodeShippingQuoteExpired  Code = "SHIPPING_QUOTE_EXPIRED"  // shipping.go:612 ("cotação expirou")
	CodeShippingNotQuoted     Code = "SHIPPING_NOT_QUOTED"     // shipping.go:609 ("primeiro cote o frete")
	CodeShippingCepRequired   Code = "SHIPPING_CEP_REQUIRED"   // shipping.go:601 ("CEP é obrigatório")
	CodeShippingOptionMissing Code = "SHIPPING_OPTION_MISSING" // shipping.go:628 ("opção de frete não encontrada")
	CodeShippingCartEmpty     Code = "SHIPPING_CART_EMPTY"     // shipping.go:401 ("nenhum item no carrinho para cotar")

	// --- COUPON (internal/coupon/service.go) ---
	CodeCouponAlreadyExists    Code = "COUPON_ALREADY_EXISTS"    // coupon/service.go:95
	CodeCouponHasCoupon        Code = "CART_HAS_COUPON"          // coupon/service.go:253 ("cart already has a coupon")
	CodeCouponNotActive        Code = "COUPON_NOT_ACTIVE"        // coupon/service.go:267
	CodeCouponNotYetValid      Code = "COUPON_NOT_YET_VALID"     // coupon/service.go:271 ("coupon is not valid yet")
	CodeCouponExpired          Code = "COUPON_EXPIRED"           // coupon/service.go:274
	CodeCouponFullyRedeemed    Code = "COUPON_FULLY_REDEEMED"    // coupon/service.go:277
	CodeCouponRedeemed         Code = "COUPON_REDEEMED"          // coupon/service.go:168 (delete guard)
	CodeCouponCodeRequired     Code = "COUPON_CODE_REQUIRED"     // coupon/service.go:230
	CodeCouponInvalidCode      Code = "COUPON_INVALID_CODE"      // coupon/service.go:264 ("invalid coupon code")
	CodeCouponMinPurchase      Code = "COUPON_MIN_PURCHASE"      // coupon/service.go:280 ("minimum purchase ... not reached")
	CodeCouponShippingRequired Code = "COUPON_SHIPPING_REQUIRED" // coupon/service.go:672 ("select shipping before applying a free-shipping coupon")

	// --- STORE (internal/store/service.go) ---
	CodeStoreAlreadyExists Code = "STORE_ALREADY_EXISTS" // store/service.go:67 (1 user = 1 store)
	CodeStoreSlugInUse     Code = "STORE_SLUG_IN_USE"    // store/service.go:76
	CodeUserNotSynced      Code = "USER_NOT_SYNCED"      // store/service.go:57; invitation/service.go:178

	// --- INVITATION (internal/invitation/{service,repository}.go) ---
	CodeInvitationExists        Code = "INVITATION_ALREADY_EXISTS" // invitation/service.go:61
	CodeInvitationExpired       Code = "INVITATION_EXPIRED"        // invitation/service.go:136,164
	CodeInvitationNotAcceptable Code = "INVITATION_NOT_ACCEPTABLE" // invitation/service.go:138,166 ("invitation is <status>")
	CodeInvitationEmailMismatch Code = "INVITATION_EMAIL_MISMATCH" // invitation/service.go:168
	CodeInvitationNotPending    Code = "INVITATION_NOT_PENDING"    // invitation/repository.go:65; service.go:265
	CodeOwnerOfOtherStore       Code = "OWNER_OF_OTHER_STORE"      // invitation/service.go:197

	// --- ORDER (internal/order/service.go) ---
	CodeCancelUnavailable     Code = "CANCEL_UNAVAILABLE"      // order/service.go:98 (cancel not allowed in current status)
	CodeOrderAlreadyPaid      Code = "ORDER_ALREADY_PAID"      // order/service.go:105
	CodeOrderAlreadyCancelled Code = "ORDER_ALREADY_CANCELLED" // order/service.go:108
	CodeOrderExpired          Code = "ORDER_EXPIRED"           // order/service.go:111
	CodeOrderRegenCancelled   Code = "ORDER_REGEN_CANCELLED"   // order/service.go:557 (regenerate link on cancelled)
	CodeOrderAddressLocked    Code = "ORDER_ADDRESS_LOCKED"    // order/service.go:516,527 (after payment/shipment)
	CodeOrderCheckoutLocked   Code = "ORDER_CHECKOUT_LOCKED"   // order/service.go:550,568 (regenerate on paid/shipped)
	CodeNfeSyncUnavailable    Code = "NFE_SYNC_UNAVAILABLE"    // order/service.go:126
	CodeErpRetryUnavailable   Code = "ERP_RETRY_UNAVAILABLE"   // order/service.go:140

	// --- LIVE (internal/live/service.go) ---
	CodeLiveMediaRequired   Code = "LIVE_MEDIA_REQUIRED"   // live/service.go:253 ("mediaId is required")
	CodeLiveProductRequired Code = "LIVE_PRODUCT_REQUIRED" // live/service.go:256 ("select at least one product for the promotion")
	CodeLiveEventNotActive  Code = "LIVE_EVENT_NOT_ACTIVE" // live/service.go:1378,1409 (set active product / change processing on a non-active event)

	// --- INSTAGRAM messaging (internal/live/service.go, ResendCheckoutMessage) ---
	CodeIgNotifyNotConfigured Code = "IG_NOTIFY_NOT_CONFIGURED" // live/service.go:1214 ("instagram notifications are not configured")
	CodeCartNoIgRecipient     Code = "CART_NO_IG_RECIPIENT"     // live/service.go:1253 ("cart has no Instagram recipient")
	CodeCartNoItemsToSend     Code = "CART_NO_ITEMS_TO_SEND"    // live/service.go:1256 ("cart has no items to send")
	CodeIgMessageWindowClosed Code = "IG_MESSAGE_WINDOW_CLOSED" // live/service.go:1302 (IG error 2534022 — outside the messaging window)
	CodeIgMessageFailed       Code = "IG_MESSAGE_FAILED"        // live/service.go:1305 ("failed to send Instagram message")

	// --- ERP (internal/erp/{finalisation,invoice}.go) ---
	CodeErpFinalisationInProgress Code = "ERP_FINALISATION_IN_PROGRESS" // erp/finalisation.go:370 ("aguarde a finalização inicial concluir")
	CodeErpRetryInvalidState      Code = "ERP_RETRY_INVALID_STATE"      // erp/finalisation.go:375 ("estado inválido para retry: <status>")
	CodeErpRetryNoSnapshot        Code = "ERP_RETRY_NO_SNAPSHOT"        // erp/finalisation.go:387 ("snapshot de pagamento ausente")
	CodeErpNotActive              Code = "ERP_NOT_ACTIVE"               // erp/invoice.go:99 ("ERP integration not active for store")
	CodeErpNoInvoiceSupport       Code = "ERP_NO_INVOICE_SUPPORT"       // erp/invoice.go:108 ("ERP provider does not expose invoice operations")

	// --- STOCK (internal/erp/stock_service.go) ---
	CodeStockInsufficient Code = "STOCK_INSUFFICIENT" // erp/stock_service.go:213 ("estoque insuficiente para esse aumento")
)
