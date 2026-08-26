package integration

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"go.uber.org/zap"

	"livecart/apps/api/db/sqlc"
	"livecart/apps/api/internal/erp"
	"livecart/apps/api/internal/erp/erpwrite"
	"livecart/apps/api/internal/events"
	"livecart/apps/api/internal/integration/providers"
	"livecart/apps/api/internal/integration/providers/payment"
	"livecart/apps/api/internal/inventory"
	"livecart/apps/api/internal/live"
	"livecart/apps/api/internal/notification"
	paymentdomain "livecart/apps/api/internal/payment"
	"livecart/apps/api/lib/config"
	"livecart/apps/api/lib/crypto"
	"livecart/apps/api/lib/dbtx"
	"livecart/apps/api/lib/httpx"
	"livecart/apps/api/lib/idempotency"
	"livecart/apps/api/lib/logger"
	"livecart/apps/api/lib/ratelimit"
	"livecart/apps/api/lib/storage"
)

// ProductSyncer syncs products from external ERP systems into the local database.
type ProductSyncer interface {
	HasProduct(ctx context.Context, storeID, externalID, externalSource string) (bool, error)
	// FilterRegisteredExternalIDs returns which of the given ERP external IDs
	// are already imported for the store+source (batch, no pagination gaps).
	FilterRegisteredExternalIDs(ctx context.Context, storeID, externalSource string, externalIDs []string) ([]string, error)
	GetProduct(ctx context.Context, storeID, productID string) (externalID, externalSource string, err error)
	// SyncProduct updates an existing LiveCart product with the latest ERP data.
	// When product.Shipping is non-nil, dimensions are refreshed too; otherwise
	// the local shipping profile is preserved. skipStock=true keeps local stock
	// entirely — used by the webhook path, where the balance is applied apart,
	// under the optimistic lock. false = normal full stock sync.
	SyncProduct(ctx context.Context, storeID, externalSource string, product providers.ERPProduct, skipStock bool) error
	// ImportProduct creates a new simple product in LiveCart from an ERP source.
	// Returns the new LiveCart product UUID.
	ImportProduct(ctx context.Context, storeID, externalSource string, product providers.ERPProduct) (productID string, err error)
}

// ProductGroupSyncer handles ERP products that ship with variations
// (e.g. Tiny tipo=V). Wired separately from ProductSyncer to avoid pulling the
// productgroup package into the product package.
type ProductGroupSyncer interface {
	SyncFromERP(ctx context.Context, storeID, externalSource string, parent providers.ERPProduct) error
	// ImportFromERP creates a new product_group in LiveCart with the given
	// (already filtered) variants. Returns the new group UUID and the external
	// IDs of the variants that were persisted.
	ImportFromERP(ctx context.Context, storeID, externalSource string, parent providers.ERPProduct) (groupID string, importedExternalIDs []string, err error)
}

// PostCheckoutHook is the customer-facing post-payment flow: cancellation/
// shipment/delivery transactional emails + timeline. Best-effort by design —
// implementations must swallow their own errors so the payment webhook ACKs
// regardless. Wired from the postcheckout package via SetPostCheckoutHook.
//
// OnCartPaid saiu daqui (Fatia A4): o tracking_token e a timeline
// `payment_confirmed` nascem na materialização da Order (order/listeners.OnCartPaid).
type PostCheckoutHook interface {
	// OnCartCancelled envia o e-mail transacional do cancelamento (cobrança não
	// concluída). Idempotente na implementação (timeline unique). O e-mail de
	// ESTORNO saiu daqui: virou reactor do domínio Notification
	// (notification/listeners.OnCartRefunded → SendRefundEmail), que reage ao
	// fato cart.refunded em vez de ser chamado inline neste fan-out.
	OnCartCancelled(ctx context.Context, cartID string)
	// OnShipmentPosted fires after a shipment is created or has a
	// tracking_code attached. Idempotent at the implementation: subsequent
	// calls for the same cart are no-ops.
	OnShipmentPosted(ctx context.Context, cartID, trackingCode string)
	// OnDelivered records the terminal state. Source explains who saw it
	// first: "merchant" (dashboard click), "customer" (public page click),
	// "system" (tracking poller saw carrier flip to delivered).
	OnDelivered(ctx context.Context, cartID, source string)
}

// ERPOrderMirror é um alias para o canônico erp.ERPOrderMirror. A interface foi
// movida para internal/erp (Bloco B2a); o alias mantém o campo, o setter e a
// fiação de boot (main.go) intactos enquanto a lógica ERP é extraída.
type ERPOrderMirror = erp.ERPOrderMirror

// Service handles business logic for integrations.
type Service struct {
	repo                *Repository
	factory             *providers.Factory
	encryptor           *crypto.Encryptor
	idempotency         *idempotency.Service
	liveService         *live.Service
	productSyncer       ProductSyncer
	productGroupSyncer  ProductGroupSyncer
	postCheckoutHook    PostCheckoutHook
	notificationService *notification.Service
	storage             *storage.S3Client
	billingGate         BillingGate
	stock               *StockReservations
	expiryScheduler     CartExpiryScheduler
	waitlistCloseSched  WaitlistCloseScheduler
	publishScheduler    PublishScheduler
	erpResyncScheduler  ERPResyncScheduler
	erpResyncNotifier   ERPResyncNotifier
	logger              *zap.Logger

	// subscriptionEnsured guarda QUANDO a inscrição de webhook de cada loja foi
	// garantida pela última vez neste processo, para que o sweep não repita a
	// chamada a cada 20s. Memória, e não banco, de propósito: reinscrever é
	// idempotente na Meta, então perder o registro num deploy custa uma chamada
	// por loja com live no ar — e é justamente num deploy (lista de campos nova)
	// que reinscrever é desejável.
	//
	// Guarda TEMPO, e não um booleano. Com booleano, o primeiro sucesso travava
	// a verificação para o resto da vida do processo: uma inscrição que morresse
	// no meio — e a Meta derruba assinatura de app cujas entregas falham
	// seguidamente — nunca mais seria restabelecida, num processo que pode ficar
	// semanas no ar. O caminho de FALHA já era retentado; o de sucesso é que
	// latchava para sempre.
	subscriptionEnsured sync.Map

	// erpProviderFactory lets the finalisation state-machine tests inject a
	// scripted ERP provider. nil in production (falls back to getERPProvider,
	// which builds the real Tiny client from the integration credentials).
	erpProviderFactory func(ctx context.Context, integration *IntegrationRow) (providers.ERPProvider, error)

	// Fase 3 do fix do race: lojas com finalização INVERTIDA (criar pedido e
	// lançar estoque ANTES de estornar as reservas — o saldo do Tiny nunca
	// sobe acima do real). Populado de ERP_FINALISE_INVERTED_STORE_IDS
	// (lista separada por vírgula; "*" liga para todas). Pré-requisito da
	// loja: "Permissão de estoque negativo = Sim" no Tiny — sem isso o
	// fallback degrada para a ordem legada, protegida pelos guards da Fase 1.
	invertFinalisationAll      bool
	invertFinalisationStoreIDs map[string]bool

	// Design C: lojas no modo pedido-como-reserva (conversão na iniciação do
	// pagamento). Populado de ERP_ORDER_AT_CHECKOUT_STORE_IDS ("*" = todas).
	orderAtCheckoutAll      bool
	orderAtCheckoutStoreIDs map[string]bool

	// erpOrderMirror projects ERP state changes into the Order aggregate.
	// Wired at boot from main.go; nil = mirror disabled (e.g. order module not yet live).
	erpOrderMirror ERPOrderMirror

	// paymentService owns payment-provider resolution (strangler-fig B1a).
	// GetPaymentProvider delegates to it. Wired at boot via SetPaymentService.
	paymentService *paymentdomain.Service

	// erpStockService owns the cart→ERP stock reservation flow (strangler-fig
	// B2b). ReserveStockInERP / AdjustStockReservationDelta delegate to it. Built
	// lazily by erpStock() (production builds it in NewService; direct-literal
	// tests get it on first use) so it always wraps THIS Service's collaborators.
	erpStockService *erp.Service
	erpStockOnce    sync.Once

	// inventoryService owns the waitlist/fila flow (strangler-fig B3a).
	// ListActiveWaitlistByCart / CancelWaitlistItem delegate to it. Built lazily
	// by inventory() (production builds it in NewService; direct-literal tests get
	// it on first use) so it always wraps THIS Service's repo/collaborators/stock.
	inventoryService *inventory.Service
	inventoryOnce    sync.Once
}

// erpStock returns the delegate erp.Service, building it once over this
// Service's repo adapter and collaborator methods. Kept lazy so the finalisation
// tests that construct a Service literal (no NewService) still delegate correctly.
// sync.Once makes the lazy build safe when concurrent goroutines hit it before
// the eager NewService warm-up ran (direct-literal tests under -race).
func (s *Service) erpStock() *erp.Service {
	s.erpStockOnce.Do(func() {
		s.erpStockService = erp.NewService(erpRepoAdapter{s.repo}, s, s.logger)
		// O razão de movimentos (000132) é o mesmo Repository por baixo; a
		// interface é separada para o ledger ser opcional nos testes do erp.
		s.erpStockService.SetStockMovementLedger(s.repo)
		s.erpStockService.SetStockMovementResolution(s.repo)
		// Serialização por pedido + teto real da API. Desligado por padrão: o
		// caminho legado segue sendo o default até a migração terminar (ADR 001).
		if config.ERPWritePipeline.Bool() {
			s.erpStockService.EnableWritePipeline()
			s.logger.Info("erp write pipeline enabled: serial queue per order + measured rate limits")
		}
	})
	return s.erpStockService
}

// ERP returns the erp.Service singleton owned by this integration.Service, so the
// composition root can wire ERP reactors/handlers straight to the canonical erp
// package instead of routing through integration delegations. Reuses erpStock();
// it never builds a second erp.Service.
func (s *Service) ERP() *erp.Service { return s.erpStock() }

// inventory returns the delegate inventory.Service, building it once over this
// Service's repo adapter, collaborator methods and stock manager. Kept lazy so
// tests that construct a Service literal (no NewService) still delegate correctly
// — mirrors erpStock().
func (s *Service) inventory() *inventory.Service {
	s.inventoryOnce.Do(func() {
		s.inventoryService = inventory.NewService(inventoryRepoAdapter{s.repo}, s, s.stock, s.liveService, s.logger)
	})
	return s.inventoryService
}

// finalisationInverted reports whether this store runs the launch-first
// finalisation order (Fase 3).
func (s *Service) finalisationInverted(storeID string) bool {
	return s.invertFinalisationAll || s.invertFinalisationStoreIDs[storeID]
}

// erpProviderFor resolves the ERP provider for an integration, honouring the
// test seam when set.
func (s *Service) erpProviderFor(ctx context.Context, integration *IntegrationRow) (providers.ERPProvider, error) {
	if s.erpProviderFactory != nil {
		return s.erpProviderFactory(ctx, integration)
	}
	return s.getERPProvider(ctx, integration)
}

// RunScheduledStockMovementResolve delega ao resolver do razão de movimentos
// (comando agendado erp.stock_movement.resolve e gate da finalização).
func (s *Service) RunScheduledStockMovementResolve(ctx context.Context, movementID string) error {
	return s.erpStockService.RunScheduledMovementResolve(ctx, movementID)
}

// SetStockMovementScheduler liga o agendador de retries do razão de movimentos.
func (s *Service) SetStockMovementScheduler(sch erp.StockMovementScheduler) {
	s.erpStockService.SetStockMovementScheduler(sch)
}

// RunStockReconciliation roda a comparação local × ERP para a loja, em modo
// RELATÓRIO: só detecta, nunca corrige, e não dispara alerta nenhum. A fórmula
// (LocalStock − Held) ainda precisa de calibração com dados reais — a validação
// de 18/08 mostrou divergência não explicada num produto saudável — então o
// primeiro uso disto é justamente calibrar, com a loja quieta.
func (s *Service) RunStockReconciliation(ctx context.Context, storeID string) (*erp.ReconciliationReport, error) {
	integration, err := s.repo.GetActiveByProvider(ctx, storeID, "erp", "tiny")
	if err != nil {
		return nil, httpx.ErrNotFound("nenhuma integração ERP ativa")
	}
	provider, err := s.erpProviderFor(ctx, integration)
	if err != nil {
		return nil, fmt.Errorf("creating ERP provider: %w", err)
	}
	stockReader, ok := provider.(interface {
		GetProductStock(ctx context.Context, externalID string) (int, error)
	})
	if !ok {
		return nil, httpx.ErrNotFound("o provedor ERP não expõe leitura de saldo")
	}
	return erp.ReconcileStockAgainstERP(ctx, s.logger, s.repo, stockReader, storeID, "tiny")
}

// ListPendingStockMovements delega o painel de pendências do razão.
func (s *Service) ListPendingStockMovements(ctx context.Context, storeID string) ([]erp.PendingStockMovement, error) {
	return s.erpStockService.ListPendingStockMovements(ctx, storeID)
}

// ResolveStockMovementManually delega a decisão humana pós-extrato.
func (s *Service) ResolveStockMovementManually(ctx context.Context, storeID, movementID string, landed bool) (*erp.StockMovementRow, error) {
	return s.erpStockService.ResolveStockMovementManually(ctx, storeID, movementID, landed)
}

// ResolveProvider satisfies erp.StockCollaborators: it maps the neutral
// erp.Integration back to the integration-owned IntegrationRow (lossless mirror)
// and resolves the provider through the existing seam-aware path.
func (s *Service) ResolveProvider(ctx context.Context, integration *erp.Integration) (providers.ERPProvider, error) {
	return s.erpProviderFor(ctx, integrationRowFromERP(integration))
}

// ResolveERPProviderByID satisfies erp.StockCollaborators: the ERP health-check
// anchors on a specific integration id (not the store's active one), so it reuses
// the existing GetERPProvider path (which loads by id, checks the ERP type, and
// resolves through the seam).
func (s *Service) ResolveERPProviderByID(ctx context.Context, integrationID, storeID string) (providers.ERPProvider, error) {
	return s.GetERPProvider(ctx, integrationID, storeID)
}

// ResolveExternalProduct satisfies erp.StockCollaborators. linked=false means
// there is nothing to move against the ERP (no product syncer wired, product not
// linked, or lookup failed) — the migrated stock flow treats all three as a
// silent no-op, matching the pre-migration behaviour.
func (s *Service) ResolveExternalProduct(ctx context.Context, storeID, productID string) (string, bool) {
	if s.productSyncer == nil {
		return "", false
	}
	externalID, _, err := s.productSyncer.GetProduct(ctx, storeID, productID)
	if err != nil || externalID == "" {
		return "", false
	}
	return externalID, true
}

// integrationRowFromERP copies the neutral erp.Integration back into the
// integration-owned IntegrationRow. The two are field-for-field mirrors; this
// exists only because erp must not import the integration package.
func integrationRowFromERP(i *erp.Integration) *IntegrationRow {
	if i == nil {
		return nil
	}
	return &IntegrationRow{
		ID:             i.ID,
		StoreID:        i.StoreID,
		Type:           i.Type,
		Provider:       i.Provider,
		Status:         i.Status,
		Credentials:    i.Credentials,
		TokenExpiresAt: i.TokenExpiresAt,
		Metadata:       i.Metadata,
		LastSyncedAt:   i.LastSyncedAt,
		CreatedAt:      i.CreatedAt,
		Priority:       i.Priority,
	}
}

// erpRepoAdapter adapts *Repository to erp.ERPRepository. Every method is
// promoted from the embedded *Repository except GetActiveByProvider, whose
// return type (*IntegrationRow) is mapped into the neutral *erp.Integration so
// the erp package stays free of the integration import (cycle guard).
type erpRepoAdapter struct{ *Repository }

func (a erpRepoAdapter) GetActiveByProvider(ctx context.Context, storeID, integrationType, provider string) (*erp.Integration, error) {
	row, err := a.Repository.GetActiveByProvider(ctx, storeID, integrationType, provider)
	if err != nil {
		return nil, err
	}
	return erpIntegrationFromRow(row), nil
}

// GetByProvider maps the integration-owned *IntegrationRow into the neutral
// *erp.Integration (same cycle guard as GetActiveByProvider) so the legacy
// finalisation can disambiguate a never-configured Tiny from an errored one.
func (a erpRepoAdapter) GetByProvider(ctx context.Context, storeID, integrationType, provider string) (*erp.Integration, error) {
	row, err := a.Repository.GetByProvider(ctx, storeID, integrationType, provider)
	if err != nil {
		return nil, err
	}
	return erpIntegrationFromRow(row), nil
}

// GetShipmentByOrderID bridges the integration-owned *ShipmentRow into the slim
// *erp.ShipmentInvoiceRef the NFe sync consumes (same cycle guard as
// GetActiveByProvider — the full shipment row stays in the shipment/logistics
// domain). Shadows the method promoted from the embedded *Repository.
func (a erpRepoAdapter) GetShipmentByOrderID(ctx context.Context, cartID string) (*erp.ShipmentInvoiceRef, error) {
	sh, err := a.Repository.GetShipmentByOrderID(ctx, cartID)
	if err != nil || sh == nil {
		return nil, err
	}
	return &erp.ShipmentInvoiceRef{ID: sh.ID, InvoiceKey: sh.InvoiceKey}, nil
}

// erpIntegrationFromRow copies the integration-owned row into the neutral
// erp.Integration port DTO (nil-safe). Inverse of integrationRowFromERP.
func erpIntegrationFromRow(row *IntegrationRow) *erp.Integration {
	if row == nil {
		return nil
	}
	return &erp.Integration{
		ID:             row.ID,
		StoreID:        row.StoreID,
		Type:           row.Type,
		Provider:       row.Provider,
		Status:         row.Status,
		Credentials:    row.Credentials,
		TokenExpiresAt: row.TokenExpiresAt,
		Metadata:       row.Metadata,
		LastSyncedAt:   row.LastSyncedAt,
		CreatedAt:      row.CreatedAt,
		Priority:       row.Priority,
	}
}

// inventoryRepoAdapter adapts *Repository to inventory.InventoryRepository. Every
// method is promoted from the embedded *Repository except GetCartByID, whose
// return type (*CartRow) is mapped into the slim *inventory.CartRef so the
// inventory package stays free of the full cart struct (and the integration
// import — cycle guard). The waitlist DTOs the other methods return are already
// aliases of the inventory ones, so they satisfy the port directly.
type inventoryRepoAdapter struct{ *Repository }

// GetCartByID bridges the integration-owned *CartRow into the slim
// *inventory.CartRef the waitlist-cancel flow consumes (same cycle guard as the
// erp adapter — the full cart row stays integration-owned). Shadows the method
// promoted from the embedded *Repository.
func (a inventoryRepoAdapter) GetCartByID(ctx context.Context, cartID string) (*inventory.CartRef, error) {
	cart, err := a.Repository.GetCartByID(ctx, cartID)
	if err != nil || cart == nil {
		return nil, err
	}
	return &inventory.CartRef{StoreID: cart.StoreID, PlatformHandle: cart.PlatformHandle}, nil
}

// GetProductByID bridges the integration-owned *ProductRow into the enxuto
// *inventory.ProductRef the waitlist promotion consumes (same cycle guard as
// GetCartByID — ID/Stock/ExternalID stay integration-owned). Shadows the method
// promoted from the embedded *Repository.
func (a inventoryRepoAdapter) GetProductByID(ctx context.Context, storeID, productID string) (*inventory.ProductRef, error) {
	p, err := a.Repository.GetProductByID(ctx, storeID, productID)
	if err != nil || p == nil {
		return nil, err
	}
	return &inventory.ProductRef{Price: p.Price, Name: p.Name, Keyword: p.Keyword}, nil
}

// liveIngestRepoAdapter adapts *Repository to live.IngestRepository (Bloco B4a).
// GetProductByKeyword is promoted from the embedded *Repository verbatim — its
// *ProductRow return is a type alias of *live.ProductRow, so it satisfies the
// port directly. EmitCommentReceived wraps the outbox write in one transaction
// via dbtx.InTx, keeping the pool/queries behind the port (live never sees them).
type liveIngestRepoAdapter struct{ *Repository }

// EmitCommentReceived writes the comment.received envelope to the outbox in a
// single transaction, so the state-free dispatch commits atomically and the
// envelope DedupKey dedups at-least-once redelivery.
func (a liveIngestRepoAdapter) EmitCommentReceived(ctx context.Context, env events.Envelope) error {
	return dbtx.InTx(ctx, a.pool, a.queries, func(q *sqlc.Queries) error {
		return events.Emit(ctx, q, env)
	})
}

// liveStockReserverAdapter adapts *Service to live.StockReserver (Bloco B4b),
// breaking the erp import cycle: the comment core (now in live) declares a NEUTRAL
// live.ReserveParams and this maps it to the erp.ReserveParams the in-package
// stock manager expects. NoteReserved is best-effort (void) upstream, so it
// always returns nil; ReserveStockInERP delegates to the preserved public method.
type liveStockReserverAdapter struct{ s *Service }

// NewLiveStockReserverAdapter builds the adapter over the integration service so
// main.go can wire it into the live.Service via SetStockReserver.
func NewLiveStockReserverAdapter(s *Service) live.StockReserver {
	return liveStockReserverAdapter{s: s}
}

func (a liveStockReserverAdapter) NoteReserved(ctx context.Context, p live.ReserveParams) error {
	a.s.stock.NoteReserved(ctx, ReserveParams{
		Op:        StockOp(p.Op),
		ProductID: p.ProductID,
		CartID:    p.CartID,
		EventID:   p.EventID,
		Quantity:  p.Quantity,
	})
	return nil
}

func (a liveStockReserverAdapter) ReserveStockInERP(ctx context.Context, storeID, cartID, eventID, productID string, quantity int, unitPrice int64, platformHandle string) error {
	return a.s.ReserveStockInERP(ctx, storeID, cartID, eventID, productID, quantity, unitPrice, platformHandle)
}

// IsStoreBlocked satisfies live.BillingGate: the comment core (in live) reads the
// paywall gate through it. Delegates to the wired billing gate (nil = not blocked).
func (s *Service) IsStoreBlocked(ctx context.Context, storeID string) bool {
	if s.billingGate == nil {
		return false
	}
	return s.billingGate.IsStoreBlocked(ctx, storeID)
}

// Compile-time guards: integration.Service satisfies the collaborator ports and
// the repo adapters satisfy the persistence ports.
var (
	_ erp.StockCollaborators          = (*Service)(nil)
	_ erp.ERPRepository               = erpRepoAdapter{}
	_ inventory.WaitlistCollaborators = (*Service)(nil)
	_ inventory.InventoryRepository   = inventoryRepoAdapter{}
	_ live.IngestRepository           = liveIngestRepoAdapter{}
	_ live.StockReserver              = liveStockReserverAdapter{}
	_ live.BillingGate                = (*Service)(nil)
	_ live.WebhookAuditor             = (*Service)(nil)
	_ live.SocialReplier              = (*Service)(nil)
)

// SetStorage wires the object storage client (used to delete transient post
// images after they are published to Instagram).
func (s *Service) SetStorage(c *storage.S3Client) {
	s.storage = c
}

// BillingGate blocks new-cart creation for stores with an inactive
// subscription (PRD 007). Implemented by billing.Service; narrow interface
// to keep the packages decoupled.
type BillingGate interface {
	IsStoreBlocked(ctx context.Context, storeID string) bool
	// OnCartPaid registra a venda no ledger (GMV) e reporta o meter ao Stripe.
	// Propaga erro para retry+DLQ (idempotente via ON CONFLICT DO NOTHING).
	OnCartPaid(ctx context.Context, storeID, cartID string, gmvCents int64) error
	// OnCartRefunded registra o estorno no ledger e devolve a taxa cobrada
	// (crédito na próxima fatura). Propaga erro para retry+DLQ.
	OnCartRefunded(ctx context.Context, storeID, cartID string) error
}

// SetBillingGate wires the paywall gate (optional — absent means no gating).
func (s *Service) SetBillingGate(gate BillingGate) {
	s.billingGate = gate
}

// NewService creates a new integration service.
func NewService(
	repo *Repository,
	factory *providers.Factory,
	encryptor *crypto.Encryptor,
	idempotency *idempotency.Service,
	liveService *live.Service,
	logger *zap.Logger,
) *Service {
	parseStoreFlag := func(envName string) (bool, map[string]bool) {
		all := false
		ids := map[string]bool{}
		for _, part := range strings.Split(os.Getenv(envName), ",") {
			part = strings.TrimSpace(part)
			switch {
			case part == "*":
				all = true
			case part != "":
				ids[part] = true
			}
		}
		return all, ids
	}
	invertAll, invertIDs := parseStoreFlag("ERP_FINALISE_INVERTED_STORE_IDS")
	orderModeAll, orderModeIDs := parseStoreFlag("ERP_ORDER_AT_CHECKOUT_STORE_IDS")
	svc := &Service{
		repo:                       repo,
		factory:                    factory,
		encryptor:                  encryptor,
		idempotency:                idempotency,
		liveService:                liveService,
		stock:                      NewStockReservations(repo, logger),
		logger:                     logger,
		invertFinalisationAll:      invertAll,
		invertFinalisationStoreIDs: invertIDs,
		orderAtCheckoutAll:         orderModeAll,
		orderAtCheckoutStoreIDs:    orderModeIDs,
	}
	// Build the ERP stock and Inventory delegates eagerly so the production
	// singletons never race on the lazy init in erpStock() / inventory() under
	// concurrent request handling.
	svc.erpStock()
	svc.inventory()
	// Wire the ingest persistence port into the shared live.Service (Bloco B4a):
	// FindProductByKeyword / DispatchCommentReceived delegate to live, which
	// reaches back into this Repository through the adapter. Done eagerly here so
	// it is set before any webhook is served.
	if liveService != nil {
		liveService.SetIngestRepository(liveIngestRepoAdapter{repo})
	}
	return svc
}

// SetProductSyncer sets the product syncer for webhook processing.
// SetERPResyncScheduler injeta o enfileirador da releitura em massa.
func (s *Service) SetERPResyncScheduler(sched ERPResyncScheduler) {
	s.erpResyncScheduler = sched
}

// SetERPResyncNotifier injeta o avisador do fim da releitura em massa.
func (s *Service) SetERPResyncNotifier(n ERPResyncNotifier) {
	s.erpResyncNotifier = n
}

func (s *Service) SetProductSyncer(syncer ProductSyncer) {
	s.productSyncer = syncer
}

// SetProductGroupSyncer wires the syncer used when an imported ERP product has
// variations (Tiny tipo=V, etc.).
func (s *Service) SetProductGroupSyncer(syncer ProductGroupSyncer) {
	s.productGroupSyncer = syncer
}

// SetPostCheckoutHook wires the customer-facing post-payment flow. The hook
// fires after the cart is marked paid and is responsible for tracking-token
// generation and email receipts.
func (s *Service) SetPostCheckoutHook(hook PostCheckoutHook) {
	s.postCheckoutHook = hook
}

// SetNotificationService sets the notification service for sending DMs.
func (s *Service) SetNotificationService(svc *notification.Service) {
	s.notificationService = svc
}

// SetPaymentService wires the extracted payment.Service so GetPaymentProvider
// delegates to it (strangler-fig B1a). Called once at boot from main.go, after
// the integration.Service exists (it is the resolver the payment.Service uses).
func (s *Service) SetPaymentService(svc *paymentdomain.Service) {
	s.paymentService = svc
}

// SetERPOrderMirror wires the order/listeners.Listener so ERP state changes are
// projected into the Order aggregate (best-effort). Called once at boot from main.go.
func (s *Service) SetERPOrderMirror(m ERPOrderMirror) {
	s.erpOrderMirror = m
}

// mirrorToOrder projects the current ERP state of a cart into the Order aggregate.
// Best-effort: mirror failures are swallowed inside MirrorCartERPToOrder and only
// logged — they never propagate to callers.
func (s *Service) mirrorToOrder(ctx context.Context, cartID string) {
	if s.erpOrderMirror == nil {
		return
	}
	s.erpOrderMirror.MirrorCartERPToOrder(ctx, cartID)
}

// =============================================================================
// INTEGRATION CRUD
// =============================================================================

// Create creates a new integration.
func (s *Service) Create(ctx context.Context, input CreateIntegrationInput) (*CreateIntegrationOutput, error) {
	// Enforce single-ERP-per-store: a merchant must disconnect the current ERP
	// before connecting a new one. Mirrors the partial unique index in the DB
	// but surfaces a friendly PT-BR message instead of a constraint violation.
	if input.Type == string(providers.ProviderTypeERP) {
		existing, err := s.repo.GetAnyByType(ctx, input.StoreID, string(providers.ProviderTypeERP))
		if err != nil {
			return nil, err
		}
		if existing != nil && existing.Provider != input.Provider {
			return nil, httpx.ErrConflict(fmt.Sprintf(
				"você já tem o ERP %s conectado. Desconecte-o antes de conectar outro ERP.",
				existing.Provider,
			))
		}
		// RECONEXÃO do mesmo ERP. A guarda acima só barrava ERP DIFERENTE, então
		// reconectar o mesmo caía no INSERT e estourava a
		// uniq_integrations_store_one_erp com 500 (SQLSTATE 23505) — e reconectar
		// é justamente o caminho de recuperação.
		//
		// Medido em produção em 12/08/2026: o token do Tiny venceu em 09/08 18:40,
		// o refresh falhou, a integração virou 'error' e o lojista ficou preso —
		// o botão de sincronizar desaparece (o front exige status 'active') e
		// quatro tentativas de reconectar devolveram 500.
		if existing != nil {
			return s.reconnectSameProvider(ctx, existing, input)
		}
	}

	// Encrypt credentials
	encryptedCreds, err := s.encryptor.EncryptJSON(input.Credentials)
	if err != nil {
		return nil, fmt.Errorf("encrypting credentials: %w", err)
	}

	// Determine token expiration if present
	var tokenExpiresAt *time.Time
	if input.Credentials != nil && !input.Credentials.ExpiresAt.IsZero() {
		tokenExpiresAt = &input.Credentials.ExpiresAt
	}

	row, err := s.repo.Create(ctx, CreateIntegrationParams{
		StoreID:        input.StoreID,
		Type:           input.Type,
		Provider:       input.Provider,
		Status:         "pending_auth",
		Credentials:    encryptedCreds,
		TokenExpiresAt: tokenExpiresAt,
		Metadata:       input.Metadata,
	})
	if err != nil {
		return nil, err
	}

	return s.toCreateOutput(row), nil
}

// reconnectSameProvider reaproveita a linha existente em vez de inserir outra:
// credenciais novas, prazo novo, status de volta para 'pending_auth' — o mesmo
// estado em que uma integração nasce, para o fluxo de autorização seguir igual.
//
// Reusa a linha (e não apaga/recria) porque o id da integração é referenciado
// por integration_logs e webhook_events; recriar romperia a trilha de auditoria
// e invalidaria a URL de webhook que o lojista já cadastrou no ERP, que carrega
// o id.
//
// Metadados só são substituídos quando vêm preenchidos: apagá-los levaria
// embora o webhookLastPingAt, que é o que sustenta o indicador de webhook do
// painel — o lojista reconectaria e o webhook apareceria como "nunca pingou".
func (s *Service) reconnectSameProvider(ctx context.Context, existing *IntegrationRow, input CreateIntegrationInput) (*CreateIntegrationOutput, error) {
	encryptedCreds, err := s.encryptor.EncryptJSON(input.Credentials)
	if err != nil {
		return nil, fmt.Errorf("encrypting credentials: %w", err)
	}

	var tokenExpiresAt *time.Time
	if input.Credentials != nil && !input.Credentials.ExpiresAt.IsZero() {
		tokenExpiresAt = &input.Credentials.ExpiresAt
	}

	if err := s.repo.UpdateCredentials(ctx, existing.ID, encryptedCreds, tokenExpiresAt); err != nil {
		return nil, fmt.Errorf("updating credentials on reconnect: %w", err)
	}
	if err := s.repo.UpdateStatus(ctx, existing.ID, "pending_auth"); err != nil {
		return nil, fmt.Errorf("resetting status on reconnect: %w", err)
	}
	if len(input.Metadata) > 0 {
		if err := s.repo.UpdateMetadata(ctx, existing.ID, input.Metadata); err != nil {
			return nil, fmt.Errorf("updating metadata on reconnect: %w", err)
		}
	}

	logger.From(ctx, s.logger).Info("erp integration reconnected in place",
		zap.String("integration_id", existing.ID),
		zap.String("store_id", existing.StoreID),
		zap.String("provider", existing.Provider),
		zap.String("previous_status", existing.Status),
	)

	// Lê de volta para devolver o estado real gravado, e não o que supomos ter
	// gravado — o front usa este retorno para montar as URLs e o indicador.
	fresh, err := s.repo.GetByID(ctx, existing.ID, existing.StoreID)
	if err != nil {
		return nil, fmt.Errorf("reloading integration after reconnect: %w", err)
	}
	return s.toCreateOutput(fresh), nil
}

// GetByID retrieves an integration by ID.
func (s *Service) GetByID(ctx context.Context, id, storeID string) (*CreateIntegrationOutput, error) {
	row, err := s.repo.GetByID(ctx, id, storeID)
	if err != nil {
		return nil, err
	}
	return s.toCreateOutput(row), nil
}

// List lists all integrations for a store.
func (s *Service) List(ctx context.Context, input ListIntegrationsInput) (*ListIntegrationsOutput, error) {
	input.Pagination.Normalize()

	rows, total, err := s.repo.ListByStore(ctx, input.StoreID, input.Pagination)
	if err != nil {
		return nil, err
	}

	result := make([]CreateIntegrationOutput, len(rows))
	for i, row := range rows {
		result[i] = *s.toCreateOutput(&row)
	}

	return &ListIntegrationsOutput{
		Integrations: result,
		Pagination:   input.Pagination,
		Total:        total,
	}, nil
}

// Delete deletes an integration.
func (s *Service) Delete(ctx context.Context, id, storeID string) error {
	return s.repo.Delete(ctx, id, storeID)
}

// UpdateStatus updates an integration's status.
func (s *Service) UpdateStatus(ctx context.Context, id, status string) error {
	return s.repo.UpdateStatus(ctx, id, status)
}

// UpdatePriority sets the priority of an integration scoped to a store. Lower
// number = higher priority in the checkout selection ordering.
func (s *Service) UpdatePriority(ctx context.Context, id, storeID string, priority int) error {
	return s.repo.UpdatePriority(ctx, id, storeID, priority)
}

// TestConnection tests if the integration credentials are valid and the provider is reachable.
func (s *Service) TestConnection(ctx context.Context, input TestConnectionInput) (*TestConnectionOutput, error) {
	provider, err := s.GetProvider(ctx, input.IntegrationID, input.StoreID)
	if err != nil {
		return nil, err
	}

	result, err := provider.TestConnection(ctx)
	if err != nil {
		s.handleProviderError(ctx, input.IntegrationID, "test_connection", err)
		return &TestConnectionOutput{
			Success:  false,
			Message:  fmt.Sprintf("Erro ao testar conexão: %v", err),
			TestedAt: time.Now(),
		}, nil
	}

	return &TestConnectionOutput{
		Success:     result.Success,
		Message:     result.Message,
		Latency:     result.Latency,
		AccountInfo: result.AccountInfo,
		TestedAt:    result.TestedAt,
	}, nil
}

// =============================================================================
// OAUTH OPERATIONS
// =============================================================================

// GetOAuthURL generates the OAuth authorization URL for a provider.
func (s *Service) GetOAuthURL(ctx context.Context, input GetOAuthURLInput) (*GetOAuthURLOutput, error) {
	switch input.Provider {
	case "mercado_pago":
		return s.getMercadoPagoOAuthURL(input.StoreID)
	case "tiny":
		return s.getTinyOAuthURL(input.StoreID)
	case "instagram":
		return s.getInstagramOAuthURL(input.StoreID)
	case "melhor_envio":
		return s.getMelhorEnvioOAuthURL(input.StoreID)
	default:
		return nil, httpx.ErrUnprocessable("unknown provider: " + input.Provider)
	}
}

// getMercadoPagoOAuthURL generates the Mercado Pago OAuth URL with PKCE.
func (s *Service) getMercadoPagoOAuthURL(storeID string) (*GetOAuthURLOutput, error) {
	appID := config.MercadoPagoAppID.String()
	if appID == "" {
		return nil, httpx.ErrUnprocessable("Mercado Pago app not configured")
	}

	redirectURI := config.WebhookBaseURL.String() + "/api/v1/integrations/oauth/mercado_pago/callback"

	// Generate unique state
	state := uuid.New().String()

	// Generate PKCE code_verifier (43-128 characters, URL-safe)
	codeVerifier := generateCodeVerifier()

	// Generate code_challenge (SHA256 hash of code_verifier, base64url encoded)
	codeChallenge := generateCodeChallenge(codeVerifier)

	// Store state and code_verifier for later retrieval in callback
	ctx := context.Background()
	if err := s.repo.CreateOAuthState(ctx, state, storeID, "mercado_pago", codeVerifier); err != nil {
		return nil, fmt.Errorf("storing OAuth state: %w", err)
	}

	authURL := fmt.Sprintf(
		"https://auth.mercadopago.com/authorization?client_id=%s&response_type=code&platform_id=mp&redirect_uri=%s&state=%s&code_challenge=%s&code_challenge_method=S256",
		appID,
		url.QueryEscape(redirectURI),
		state,
		codeChallenge,
	)

	return &GetOAuthURLOutput{
		AuthURL: authURL,
		State:   state,
	}, nil
}

// generateCodeVerifier generates a random code verifier for PKCE (43-128 chars).
func generateCodeVerifier() string {
	b := make([]byte, 32)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// generateCodeChallenge generates the code challenge from the verifier (S256 method).
func generateCodeChallenge(verifier string) string {
	h := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(h[:])
}

// getInstagramOAuthURL generates the Instagram Business Login OAuth URL.
func (s *Service) getInstagramOAuthURL(storeID string) (*GetOAuthURLOutput, error) {
	appID := config.InstagramAppID.String()
	if appID == "" {
		return nil, httpx.ErrUnprocessable("Instagram app not configured")
	}

	redirectURI := config.WebhookBaseURL.String() + "/api/v1/integrations/oauth/instagram/callback"

	// Generate unique state
	state := uuid.New().String()

	// Store state for later retrieval in callback
	ctx := context.Background()
	if err := s.repo.CreateOAuthState(ctx, state, storeID, "instagram", ""); err != nil {
		return nil, fmt.Errorf("storing OAuth state: %w", err)
	}

	// Build authorization URL
	// Scopes:
	//   instagram_business_basic            (required)
	//   instagram_business_manage_comments  (comments + live_comments webhooks, moderation)
	//   instagram_business_manage_messages  (checkout-link DMs, story-reply DMs)
	//   instagram_business_content_publish  (publish posts/Reels/Stories from the app)
	authURL := fmt.Sprintf(
		"https://www.instagram.com/oauth/authorize?client_id=%s&redirect_uri=%s&response_type=code&scope=%s&state=%s",
		appID,
		url.QueryEscape(redirectURI),
		url.QueryEscape("instagram_business_basic,instagram_business_manage_comments,instagram_business_manage_messages,instagram_business_content_publish"),
		state,
	)

	return &GetOAuthURLOutput{
		AuthURL: authURL,
		State:   state,
	}, nil
}

// getTinyOAuthURL generates the Tiny ERP OAuth URL using stored credentials.
func (s *Service) getTinyOAuthURL(storeID string) (*GetOAuthURLOutput, error) {
	// Find existing integration (active or pending_auth) to get client_id
	existing, err := s.repo.GetByProvider(context.Background(), storeID, "erp", "tiny")
	if err != nil || existing == nil {
		return nil, httpx.ErrUnprocessable("Crie primeiro o aplicativo Tiny e salve as credenciais")
	}

	// Decrypt credentials to get client_id
	creds, err := s.decryptCredentials(existing.Credentials)
	if err != nil {
		return nil, fmt.Errorf("decrypting credentials: %w", err)
	}

	clientID := creds.Extra["client_id"]
	if clientID == nil || clientID == "" {
		return nil, httpx.ErrUnprocessable("Client ID não encontrado nas credenciais")
	}

	redirectURI := config.WebhookBaseURL.String() + "/api/v1/integrations/oauth/tiny/callback"

	// Generate state with store ID for callback
	state := storeID

	authURL := fmt.Sprintf(
		"https://accounts.tiny.com.br/realms/tiny/protocol/openid-connect/auth?client_id=%s&redirect_uri=%s&scope=openid&response_type=code&state=%s",
		clientID,
		redirectURI,
		state,
	)

	return &GetOAuthURLOutput{
		AuthURL: authURL,
		State:   state,
	}, nil
}

// GetProviderURLs returns the redirect (OAuth callback) and webhook URLs the
// merchant must paste into the provider's app config. The webhook URL embeds
// the store ID so it stays stable across reconnects.
func (s *Service) GetProviderURLs(_ context.Context, input GetProviderURLsInput) (*GetProviderURLsOutput, error) {
	urls := buildProviderURLs(input.Provider, input.StoreID)
	if urls.RedirectURL == "" && urls.WebhookURL == "" {
		return nil, httpx.ErrUnprocessable("provider has no setup URLs: " + input.Provider)
	}
	return urls, nil
}

// buildProviderURLs resolves the setup URLs for a provider. Returns an output
// with empty fields when the provider has nothing to expose. Kept as a pure
// helper so it can be reused when assembling integration responses.
func buildProviderURLs(provider, storeID string) *GetProviderURLsOutput {
	base := strings.TrimRight(config.WebhookBaseURL.String(), "/")
	out := &GetProviderURLsOutput{Provider: provider}
	switch provider {
	case "tiny":
		out.RedirectURL = base + "/api/v1/integrations/oauth/tiny/callback"
		if storeID != "" {
			out.WebhookURL = base + "/api/webhooks/tiny/" + storeID
		}
	case "mercado_pago":
		out.RedirectURL = base + "/api/v1/integrations/oauth/mercado_pago/callback"
		if storeID != "" {
			out.WebhookURL = base + "/api/webhooks/mercado_pago/" + storeID
		}
	case "pagarme":
		if storeID != "" {
			out.WebhookURL = base + "/api/webhooks/pagarme/" + storeID
		}
	case "instagram":
		out.RedirectURL = base + "/api/v1/integrations/oauth/instagram/callback"
	case "melhor_envio":
		out.RedirectURL = base + "/api/v1/integrations/oauth/melhor_envio/callback"
		if storeID != "" {
			out.WebhookURL = base + "/api/webhooks/melhor_envio/" + storeID
		}
	}
	return out
}

// RecordWebhookPing stamps webhookLastPingAt on the integration metadata so the
// admin UI can show whether the merchant has the webhook URL correctly wired in
// the provider's app. Best-effort: failures are logged and swallowed so they
// can't block the webhook 200 response (Tiny disables URLs after 20 non-200s).
func (s *Service) RecordWebhookPing(ctx context.Context, storeID, provider string) {
	integrationType := "payment"
	switch provider {
	case "tiny":
		integrationType = "erp"
	case "instagram":
		integrationType = "social"
	case "twilio_whatsapp":
		integrationType = "communication"
	}

	integration, err := s.repo.GetByProvider(ctx, storeID, integrationType, provider)
	if err != nil || integration == nil {
		// Webhook arrived before the merchant created the integration — nothing to stamp.
		return
	}

	metadata := integration.Metadata
	if metadata == nil {
		metadata = make(map[string]any)
	}
	metadata["webhookLastPingAt"] = time.Now().UTC().Format(time.RFC3339)

	if err := s.repo.UpdateMetadata(ctx, integration.ID, metadata); err != nil {
		logger.From(ctx, s.logger).Warn("failed to stamp webhook ping",
			zap.String("store_id", storeID),
			zap.String("provider", provider),
			zap.Error(err),
		)
	}
}

// HandleOAuthCallback handles the OAuth callback and creates/updates the integration.
func (s *Service) HandleOAuthCallback(ctx context.Context, input OAuthCallbackInput) (*OAuthCallbackOutput, error) {
	switch input.Provider {
	case "mercado_pago":
		return s.handleMercadoPagoCallback(ctx, input)
	case "tiny":
		return s.handleTinyCallback(ctx, input)
	case "instagram":
		return s.handleInstagramCallback(ctx, input)
	case "melhor_envio":
		return s.handleMelhorEnvioCallback(ctx, input)
	default:
		return nil, httpx.ErrUnprocessable("unknown provider: " + input.Provider)
	}
}

// handleMercadoPagoCallback exchanges the code for tokens and creates the integration.
func (s *Service) handleMercadoPagoCallback(ctx context.Context, input OAuthCallbackInput) (*OAuthCallbackOutput, error) {
	appID := config.MercadoPagoAppID.String()
	appSecret := config.MercadoPagoAppSecret.String()
	redirectURI := config.WebhookBaseURL.String() + "/api/v1/integrations/oauth/mercado_pago/callback"

	if appID == "" || appSecret == "" {
		return nil, httpx.ErrUnprocessable("Mercado Pago app not configured")
	}

	// Retrieve OAuth state (includes code_verifier for PKCE)
	oauthState, err := s.repo.GetOAuthState(ctx, input.State)
	if err != nil {
		logger.From(ctx, s.logger).Error("OAuth state not found or expired",
			zap.String("state", input.State),
			zap.Error(err),
		)
		return nil, httpx.ErrUnprocessable("OAuth state expired or invalid")
	}

	// Clean up the state after retrieval
	defer s.repo.DeleteOAuthState(ctx, input.State)

	// Override input.State with actual store_id from database
	input.State = oauthState.StoreID.String()

	// Exchange code for tokens (with PKCE code_verifier)
	tokenURL := "https://api.mercadopago.com/oauth/token"
	payload := map[string]string{
		"grant_type":    "authorization_code",
		"client_id":     appID,
		"client_secret": appSecret,
		"code":          input.Code,
		"redirect_uri":  redirectURI,
		"code_verifier": oauthState.CodeVerifier,
	}

	// Add test_token parameter to get TEST credentials instead of production
	if config.MercadoPagoTestMode.String() == "true" {
		payload["test_token"] = "true"
		logger.From(ctx, s.logger).Info("Mercado Pago OAuth: requesting TEST credentials (test_token=true)")
	}

	payloadBytes, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, "POST", tokenURL, bytes.NewReader(payloadBytes))
	if err != nil {
		return nil, fmt.Errorf("creating token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("exchanging code for token: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		logger.From(ctx, s.logger).Error("OAuth token exchange failed",
			zap.Int("status", resp.StatusCode),
			zap.String("body", string(body)),
		)
		return nil, fmt.Errorf("token exchange failed: status %d", resp.StatusCode)
	}

	var tokenResp struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int    `json:"expires_in"`
		TokenType    string `json:"token_type"`
		UserID       int64  `json:"user_id"`
		PublicKey    string `json:"public_key"`
	}
	if err := json.Unmarshal(body, &tokenResp); err != nil {
		return nil, fmt.Errorf("parsing token response: %w", err)
	}

	// State contains the store ID
	storeID := input.State

	// Create credentials
	creds := &providers.Credentials{
		AccessToken:  tokenResp.AccessToken,
		RefreshToken: tokenResp.RefreshToken,
		TokenType:    tokenResp.TokenType,
		ExpiresAt:    time.Now().Add(time.Duration(tokenResp.ExpiresIn) * time.Second),
		Extra: map[string]any{
			"user_id":    tokenResp.UserID,
			"public_key": tokenResp.PublicKey,
		},
	}

	// Encrypt credentials
	encryptedCreds, err := s.encryptor.EncryptJSON(creds)
	if err != nil {
		return nil, fmt.Errorf("encrypting credentials: %w", err)
	}

	tokenExpiresAt := creds.ExpiresAt

	// Check if integration already exists for this store
	existing, _ := s.repo.GetActiveByProvider(ctx, storeID, "payment", "mercado_pago")

	var integrationID string
	if existing != nil {
		// Update existing integration
		err = s.repo.UpdateCredentials(ctx, existing.ID, encryptedCreds, &tokenExpiresAt)
		if err != nil {
			return nil, fmt.Errorf("updating credentials: %w", err)
		}
		err = s.repo.UpdateStatus(ctx, existing.ID, "active")
		if err != nil {
			return nil, fmt.Errorf("updating status: %w", err)
		}
		integrationID = existing.ID
	} else {
		// Create new integration
		row, err := s.repo.Create(ctx, CreateIntegrationParams{
			StoreID:        storeID,
			Type:           "payment",
			Provider:       "mercado_pago",
			Status:         "active",
			Credentials:    encryptedCreds,
			TokenExpiresAt: &tokenExpiresAt,
			Metadata: map[string]any{
				"user_id":      tokenResp.UserID,
				"public_key":   tokenResp.PublicKey,
				"connected_at": time.Now(),
			},
		})
		if err != nil {
			return nil, fmt.Errorf("creating integration: %w", err)
		}
		integrationID = row.ID
	}

	logger.From(ctx, s.logger).Info("Mercado Pago OAuth completed",
		zap.String("store_id", storeID),
		zap.String("integration_id", integrationID),
		zap.Int64("mp_user_id", tokenResp.UserID),
	)

	return &OAuthCallbackOutput{
		IntegrationID: integrationID,
		StoreID:       storeID,
		Provider:      "mercado_pago",
		Status:        "active",
	}, nil
}

// handleTinyCallback exchanges the code for tokens using stored credentials.
func (s *Service) handleTinyCallback(ctx context.Context, input OAuthCallbackInput) (*OAuthCallbackOutput, error) {
	// State contains the store ID
	storeID := input.State

	// Get existing integration with stored client_id/client_secret
	existing, err := s.repo.GetByProvider(ctx, storeID, "erp", "tiny")
	if err != nil || existing == nil {
		return nil, httpx.ErrUnprocessable("Integração Tiny não encontrada. Crie primeiro com client_id e client_secret.")
	}

	// Decrypt stored credentials to get client_id and client_secret
	storedCreds, err := s.decryptCredentials(existing.Credentials)
	if err != nil {
		return nil, fmt.Errorf("decrypting stored credentials: %w", err)
	}

	clientID, _ := storedCreds.Extra["client_id"].(string)
	clientSecret, _ := storedCreds.Extra["client_secret"].(string)

	// Debug logging to see what values we have
	clientIDPrefix := ""
	if len(clientID) > 20 {
		clientIDPrefix = clientID[:20] + "..."
	} else if clientID != "" {
		clientIDPrefix = clientID
	}
	logger.From(ctx, s.logger).Info("Tiny OAuth token exchange - credentials loaded",
		zap.String("store_id", storeID),
		zap.String("integration_id", existing.ID),
		zap.String("client_id_prefix", clientIDPrefix),
		zap.Int("client_id_len", len(clientID)),
		zap.Bool("has_client_secret", clientSecret != ""),
		zap.Int("client_secret_len", len(clientSecret)),
	)

	if clientID == "" || clientSecret == "" {
		return nil, httpx.ErrUnprocessable("Client ID ou Client Secret não encontrado")
	}

	redirectURI := config.WebhookBaseURL.String() + "/api/v1/integrations/oauth/tiny/callback"

	// Use url.Values for proper URL encoding
	formData := url.Values{}
	formData.Set("grant_type", "authorization_code")
	formData.Set("client_id", clientID)
	formData.Set("client_secret", clientSecret)
	formData.Set("code", input.Code)
	formData.Set("redirect_uri", redirectURI)

	logger.From(ctx, s.logger).Info("Tiny OAuth token exchange - request params",
		zap.String("redirect_uri", redirectURI),
		zap.Bool("has_code", input.Code != ""),
	)

	tokenURL := "https://accounts.tiny.com.br/realms/tiny/protocol/openid-connect/token"
	req, err := http.NewRequestWithContext(ctx, "POST", tokenURL, strings.NewReader(formData.Encode()))
	if err != nil {
		return nil, fmt.Errorf("creating token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("exchanging code for token: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		logger.From(ctx, s.logger).Error("Tiny OAuth token exchange failed",
			zap.Int("status", resp.StatusCode),
			zap.String("body", string(body)),
		)
		return nil, fmt.Errorf("token exchange failed: status %d", resp.StatusCode)
	}

	var tokenResp struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int    `json:"expires_in"`
		TokenType    string `json:"token_type"`
	}
	if err := json.Unmarshal(body, &tokenResp); err != nil {
		return nil, fmt.Errorf("parsing token response: %w", err)
	}

	// Log token expiration info for debugging
	logger.From(ctx, s.logger).Info("Tiny OAuth token received",
		zap.Int("expires_in", tokenResp.ExpiresIn),
		zap.Bool("has_access_token", tokenResp.AccessToken != ""),
		zap.Bool("has_refresh_token", tokenResp.RefreshToken != ""),
	)

	// Default to 4 hours if expires_in is 0 or not provided
	// Tiny access tokens typically last about 4 hours
	expiresInSeconds := tokenResp.ExpiresIn
	if expiresInSeconds <= 0 {
		logger.From(ctx, s.logger).Warn("Tiny OAuth: expires_in is 0 or negative, defaulting to 4 hours",
			zap.Int("original_expires_in", tokenResp.ExpiresIn),
		)
		expiresInSeconds = 14400 // 4 hours in seconds
	}

	// Create credentials preserving client_id and client_secret
	expiresAt := time.Now().Add(time.Duration(expiresInSeconds) * time.Second)
	creds := &providers.Credentials{
		AccessToken:  tokenResp.AccessToken,
		RefreshToken: tokenResp.RefreshToken,
		TokenType:    tokenResp.TokenType,
		ExpiresAt:    expiresAt,
		Extra: map[string]any{
			"client_id":     clientID,
			"client_secret": clientSecret,
		},
	}

	logger.From(ctx, s.logger).Info("Tiny OAuth credentials created",
		zap.Time("expires_at", expiresAt),
		zap.Int("expires_in_seconds_used", expiresInSeconds),
	)

	// Encrypt credentials
	encryptedCreds, err := s.encryptor.EncryptJSON(creds)
	if err != nil {
		return nil, fmt.Errorf("encrypting credentials: %w", err)
	}

	tokenExpiresAt := creds.ExpiresAt

	// Update existing integration with OAuth tokens
	err = s.repo.UpdateCredentials(ctx, existing.ID, encryptedCreds, &tokenExpiresAt)
	if err != nil {
		return nil, fmt.Errorf("updating credentials: %w", err)
	}
	err = s.repo.UpdateStatus(ctx, existing.ID, "active")
	if err != nil {
		return nil, fmt.Errorf("updating status: %w", err)
	}

	logger.From(ctx, s.logger).Info("Tiny OAuth completed",
		zap.String("store_id", storeID),
		zap.String("integration_id", existing.ID),
	)

	return &OAuthCallbackOutput{
		IntegrationID: existing.ID,
		StoreID:       storeID,
		Provider:      "tiny",
		Status:        "active",
	}, nil
}

// handleInstagramCallback exchanges the code for tokens and creates the integration.
func (s *Service) handleInstagramCallback(ctx context.Context, input OAuthCallbackInput) (*OAuthCallbackOutput, error) {
	appID := config.InstagramAppID.String()
	appSecret := config.InstagramAppSecret.String()
	redirectURI := config.WebhookBaseURL.String() + "/api/v1/integrations/oauth/instagram/callback"

	if appID == "" || appSecret == "" {
		return nil, httpx.ErrUnprocessable("Instagram app not configured")
	}

	// Retrieve OAuth state
	oauthState, err := s.repo.GetOAuthState(ctx, input.State)
	if err != nil {
		logger.From(ctx, s.logger).Error("OAuth state not found or expired",
			zap.String("state", input.State),
			zap.Error(err),
		)
		return nil, httpx.ErrUnprocessable("OAuth state expired or invalid")
	}

	// Clean up the state after retrieval
	defer s.repo.DeleteOAuthState(ctx, input.State)

	storeID := oauthState.StoreID.String()

	// Step 1: Exchange code for short-lived token
	shortLivedToken, instagramUserID, err := s.exchangeInstagramCode(ctx, appID, appSecret, redirectURI, input.Code)
	if err != nil {
		return nil, fmt.Errorf("exchanging code for token: %w", err)
	}

	// Step 2: Exchange short-lived token for long-lived token
	longLivedToken, expiresIn, err := s.exchangeInstagramLongLivedToken(ctx, appSecret, shortLivedToken)
	if err != nil {
		logger.From(ctx, s.logger).Warn("failed to get long-lived token, using short-lived",
			zap.Error(err),
		)
		// Fall back to short-lived token (1 hour)
		longLivedToken = shortLivedToken
		expiresIn = 3600
	}

	// Step 3: Get user profile info (username + o id da conta profissional)
	//
	// São DOIS ids diferentes e a diferença é o bug: `instagramUserID` acima vem
	// da troca do código e é app-scoped (28139…); `accountID` aqui é o id da
	// conta profissional (17841…), que é o que a Meta manda em entry.id de todo
	// webhook. Gravar o primeiro como "instagram_user_id" fazia a resolução de
	// loja por conta não achar nada — toda DM de comprador caía em "no
	// integration found" e era descartada em silêncio.
	username, accountID, err := s.getInstagramUserProfile(ctx, longLivedToken)
	if err != nil {
		logger.From(ctx, s.logger).Warn("failed to get Instagram profile",
			zap.Error(err),
		)
		username = instagramUserID // fallback to user ID
	}
	// Sem o perfil, o app-scoped é o único id que temos. Continua não casando
	// com o webhook, mas não piora nada — e o fallback abaixo cobre o caso.
	if accountID == "" {
		accountID = instagramUserID
	}

	// Create credentials
	creds := &providers.Credentials{
		AccessToken: longLivedToken,
		TokenType:   "bearer",
		ExpiresAt:   time.Now().Add(time.Duration(expiresIn) * time.Second),
		Extra: map[string]any{
			"instagram_user_id": instagramUserID,
			"username":          username,
		},
	}

	// O metadata é a fonte da resolução de loja por conta, então ele guarda o id
	// que o WEBHOOK usa. O app-scoped fica ao lado, com nome próprio, porque é o
	// que a Graph aceita em algumas chamadas — perder um para ganhar o outro só
	// trocaria de bug.
	igMetadata := map[string]any{
		"instagram_user_id":       accountID,
		"instagram_app_scoped_id": instagramUserID,
		"username":                username,
	}

	// Encrypt credentials
	encryptedCreds, err := s.encryptor.EncryptJSON(creds)
	if err != nil {
		return nil, fmt.Errorf("encrypting credentials: %w", err)
	}

	tokenExpiresAt := creds.ExpiresAt

	// Check if integration already exists for this store
	existing, _ := s.repo.GetActiveByProvider(ctx, storeID, "social", "instagram")

	var integrationID string
	if existing != nil {
		// Update existing integration
		err = s.repo.UpdateCredentials(ctx, existing.ID, encryptedCreds, &tokenExpiresAt)
		if err != nil {
			return nil, fmt.Errorf("updating credentials: %w", err)
		}
		err = s.repo.UpdateStatus(ctx, existing.ID, "active")
		if err != nil {
			return nil, fmt.Errorf("updating status: %w", err)
		}
		// O metadata TAMBÉM é reescrito ao reconectar.
		//
		// Antes só credenciais e status eram atualizados, então uma integração
		// gravada com o id errado ficava errada para sempre: reconectar, que é o
		// que qualquer um tenta primeiro, não tocava no campo que a resolução de
		// loja lê. Preserva connected_at — a data da PRIMEIRA conexão não muda
		// porque o lojista reconectou.
		merged := map[string]any{}
		for k, v := range existing.Metadata {
			merged[k] = v
		}
		for k, v := range igMetadata {
			merged[k] = v
		}
		if _, ok := merged["connected_at"]; !ok {
			merged["connected_at"] = time.Now()
		}
		if err := s.repo.UpdateMetadata(ctx, existing.ID, merged); err != nil {
			return nil, fmt.Errorf("updating instagram metadata: %w", err)
		}
		integrationID = existing.ID
	} else {
		// Create new integration
		newMetadata := map[string]any{"connected_at": time.Now()}
		for k, v := range igMetadata {
			newMetadata[k] = v
		}
		row, err := s.repo.Create(ctx, CreateIntegrationParams{
			StoreID:        storeID,
			Type:           "social",
			Provider:       "instagram",
			Status:         "active",
			Credentials:    encryptedCreds,
			TokenExpiresAt: &tokenExpiresAt,
			Metadata:       newMetadata,
		})
		if err != nil {
			return nil, fmt.Errorf("creating integration: %w", err)
		}
		integrationID = row.ID
	}

	logger.From(ctx, s.logger).Info("Instagram OAuth completed",
		zap.String("store_id", storeID),
		zap.String("integration_id", integrationID),
		zap.String("instagram_user_id", instagramUserID),
		zap.String("username", username),
	)

	// Inscreve a conta nos eventos de webhook. Sem este passo a Meta NÃO
	// entrega nada para a conta — foi o que derrubou a venda por Story do
	// primeiro cliente (18/07/2026): comentário seguia funcionando pelo
	// polling, mas resposta de Story (DM) não tem fallback e morria em
	// silêncio. Best-effort aqui para não quebrar a conexão; o resultado fica
	// no log e o lojista pode reexecutar por EnsureInstagramWebhookSubscription.
	if err := s.SubscribeInstagramWebhooks(ctx, storeID); err != nil {
		logger.From(ctx, s.logger).Error("instagram connected BUT webhook subscription failed — story/DM sales will not work until this is fixed",
			zap.String("store_id", storeID),
			zap.String("integration_id", integrationID),
			zap.Error(err),
		)
	}

	return &OAuthCallbackOutput{
		IntegrationID: integrationID,
		StoreID:       storeID,
		Provider:      "instagram",
		Status:        "active",
	}, nil
}

// SubscribeInstagramWebhooks subscribes the store's connected Instagram account
// to the webhook fields LiveCart consumes (comments + messages). Idempotent:
// Meta accepts repeated calls, so it is safe to run on every connect and from
// the admin repair endpoint.
func (s *Service) SubscribeInstagramWebhooks(ctx context.Context, storeID string) error {
	provider, err := s.resolveInstagramSocialProvider(ctx, storeID)
	if err != nil {
		return err
	}
	subscriber, ok := provider.(interface {
		SubscribeWebhooks(ctx context.Context) error
	})
	if !ok {
		return fmt.Errorf("instagram provider does not support webhook subscription")
	}
	if err := subscriber.SubscribeWebhooks(ctx); err != nil {
		return fmt.Errorf("subscribing instagram webhooks: %w", err)
	}
	logger.From(ctx, s.logger).Info("instagram webhook subscription ensured",
		zap.String("store_id", storeID),
	)
	return nil
}

// GetInstagramWebhookSubscription reports which webhook fields the account is
// currently subscribed to, so the dashboard (and support) can tell at a glance
// whether story/DM sales will work.
func (s *Service) GetInstagramWebhookSubscription(ctx context.Context, storeID string) ([]string, error) {
	provider, err := s.resolveInstagramSocialProvider(ctx, storeID)
	if err != nil {
		return nil, err
	}
	lister, ok := provider.(interface {
		ListSubscribedFields(ctx context.Context) ([]string, error)
	})
	if !ok {
		return nil, fmt.Errorf("instagram provider does not support reading the subscription")
	}
	return lister.ListSubscribedFields(ctx)
}

// exchangeInstagramCode exchanges the authorization code for a short-lived access token.
func (s *Service) exchangeInstagramCode(ctx context.Context, appID, appSecret, redirectURI, code string) (string, string, error) {
	tokenURL := "https://api.instagram.com/oauth/access_token"

	// Instagram requires form-urlencoded for this endpoint
	formData := url.Values{}
	formData.Set("client_id", appID)
	formData.Set("client_secret", appSecret)
	formData.Set("grant_type", "authorization_code")
	formData.Set("redirect_uri", redirectURI)
	formData.Set("code", code)

	req, err := http.NewRequestWithContext(ctx, "POST", tokenURL, strings.NewReader(formData.Encode()))
	if err != nil {
		return "", "", fmt.Errorf("creating token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("sending token request: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		logger.From(ctx, s.logger).Error("Instagram token exchange failed",
			zap.Int("status", resp.StatusCode),
			zap.String("body", string(body)),
		)
		return "", "", fmt.Errorf("token exchange failed: status %d", resp.StatusCode)
	}

	var tokenResp struct {
		AccessToken string `json:"access_token"`
		UserID      int64  `json:"user_id"`
	}
	if err := json.Unmarshal(body, &tokenResp); err != nil {
		return "", "", fmt.Errorf("parsing token response: %w", err)
	}

	return tokenResp.AccessToken, fmt.Sprintf("%d", tokenResp.UserID), nil
}

// exchangeInstagramLongLivedToken exchanges a short-lived token for a long-lived token (60 days).
func (s *Service) exchangeInstagramLongLivedToken(ctx context.Context, appSecret, shortLivedToken string) (string, int, error) {
	tokenURL := fmt.Sprintf(
		"https://graph.instagram.com/access_token?grant_type=ig_exchange_token&client_secret=%s&access_token=%s",
		appSecret,
		shortLivedToken,
	)

	req, err := http.NewRequestWithContext(ctx, "GET", tokenURL, nil)
	if err != nil {
		return "", 0, fmt.Errorf("creating long-lived token request: %w", err)
	}

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", 0, fmt.Errorf("sending long-lived token request: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		logger.From(ctx, s.logger).Error("Instagram long-lived token exchange failed",
			zap.Int("status", resp.StatusCode),
			zap.String("body", string(body)),
		)
		return "", 0, fmt.Errorf("long-lived token exchange failed: status %d", resp.StatusCode)
	}

	var tokenResp struct {
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &tokenResp); err != nil {
		return "", 0, fmt.Errorf("parsing long-lived token response: %w", err)
	}

	return tokenResp.AccessToken, tokenResp.ExpiresIn, nil
}

// getInstagramUserProfile fetches the user's Instagram username.
func (s *Service) getInstagramUserProfile(ctx context.Context, accessToken string) (username, accountID string, err error) {
	profileURL := fmt.Sprintf(
		"https://graph.instagram.com/me?fields=user_id,username&access_token=%s",
		accessToken,
	)

	req, err := http.NewRequestWithContext(ctx, "GET", profileURL, nil)
	if err != nil {
		return "", "", fmt.Errorf("creating profile request: %w", err)
	}

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("sending profile request: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		logger.From(ctx, s.logger).Error("Instagram profile fetch failed",
			zap.Int("status", resp.StatusCode),
			zap.String("body", string(body)),
		)
		return "", "", fmt.Errorf("profile fetch failed: status %d", resp.StatusCode)
	}

	var profileResp struct {
		UserID   string `json:"user_id"`
		Username string `json:"username"`
	}
	if err := json.Unmarshal(body, &profileResp); err != nil {
		return "", "", fmt.Errorf("parsing profile response: %w", err)
	}

	// user_id é o id da CONTA PROFISSIONAL (17841…) — o mesmo que a Meta manda
	// em entry.id nos webhooks. É diferente do id devolvido pela troca do
	// código, que é app-scoped (28139…) e não aparece em webhook nenhum.
	// Buscávamos o campo e descartávamos, guardando o app-scoped como
	// `instagram_user_id`: a resolução de loja por conta nunca achava nada.
	return profileResp.Username, profileResp.UserID, nil
}

// RefreshInstagramToken SAIU: era uma segunda implementacao do
// GET /refresh_access_token, sem nenhum chamador, ao lado do
// Instagram.RefreshToken que era um stub `return nil, nil`. Ou seja: existia o
// codigo que renova e existia o gancho por onde a renovacao passa, e os dois
// nunca se encontraram. A implementacao vive agora no provider
// (providers/social/instagram.go), que e onde createProviderFromRow e o
// TokenRefreshWorker a procuram.

// =============================================================================
// PROVIDER OPERATIONS
// =============================================================================

// GetProvider returns an initialized provider for the given integration.
func (s *Service) GetProvider(ctx context.Context, integrationID, storeID string) (providers.Provider, error) {
	integration, err := s.repo.GetByID(ctx, integrationID, storeID)
	if err != nil {
		return nil, err
	}

	return s.createProviderFromRow(ctx, integration)
}

// GetPaymentProvider returns a PaymentProvider for the given integration.
//
// The resolution logic now lives in the extracted payment.Service (strangler-fig
// B1a); this method is a thin delegation kept for the ~8 internal call sites and
// external callers whose signature must not change.
func (s *Service) GetPaymentProvider(ctx context.Context, integrationID, storeID string) (providers.PaymentProvider, error) {
	return s.paymentService.GetProvider(ctx, integrationID, storeID)
}

// ResolveIntegration implements payment.IntegrationResolver: it fetches the
// integration and returns its declared type plus a builder that constructs the
// provider through the shared createProviderFromRow. Returning a builder (rather
// than the provider) lets payment.Service.GetProvider run the type check before
// any credential decrypt/refresh, and keeps createProviderFromRow — shared with
// ERP/Social — inside this package.
func (s *Service) ResolveIntegration(ctx context.Context, integrationID, storeID string) (string, func() (providers.Provider, error), error) {
	integration, err := s.repo.GetByID(ctx, integrationID, storeID)
	if err != nil {
		return "", nil, err
	}

	return integration.Type, func() (providers.Provider, error) {
		return s.createProviderFromRow(ctx, integration)
	}, nil
}

// GetERPProvider returns an ERPProvider for the given integration.
func (s *Service) GetERPProvider(ctx context.Context, integrationID, storeID string) (providers.ERPProvider, error) {
	integration, err := s.repo.GetByID(ctx, integrationID, storeID)
	if err != nil {
		return nil, err
	}

	if integration.Type != string(providers.ProviderTypeERP) {
		return nil, httpx.ErrUnprocessable("integration is not an ERP provider")
	}

	provider, err := s.createProviderFromRow(ctx, integration)
	if err != nil {
		return nil, err
	}

	erpProvider, ok := provider.(providers.ERPProvider)
	if !ok {
		return nil, httpx.ErrUnprocessable("failed to cast to ERP provider")
	}

	return erpProvider, nil
}

// GetShippingProvider returns the ShippingProvider for the store's active
// shipping integration. Returns httpx.ErrNotFound when no shipping integration
// is configured for the store.
//
// Deprecated: prefer GetShippingProviders (plural) when quoting — stores may
// have more than one active shipping integration and the checkout should show
// options from all of them. This method is kept for callers that genuinely
// need a single provider by ID.
func (s *Service) GetShippingProvider(ctx context.Context, storeID string) (providers.ShippingProvider, error) {
	all, err := s.GetShippingProviders(ctx, storeID)
	if err != nil {
		return nil, err
	}
	if len(all) == 0 {
		return nil, httpx.ErrNotFound("no active shipping integration for this store")
	}
	return all[0], nil
}

// GetShippingProviders returns every active shipping integration for the
// store, fully initialized. Returns an empty slice (not an error) when the
// store has no shipping integration configured — callers decide how to treat
// that (checkout will surface an UnprocessableEntity to the customer).
func (s *Service) GetShippingProviders(ctx context.Context, storeID string) ([]providers.ShippingProvider, error) {
	rows, err := s.repo.ListByType(ctx, storeID, string(providers.ProviderTypeShipping))
	if err != nil {
		return nil, err
	}
	out := make([]providers.ShippingProvider, 0, len(rows))
	for i := range rows {
		if rows[i].Status != "active" {
			continue
		}
		provider, err := s.createProviderFromRow(ctx, &rows[i])
		if err != nil {
			logger.From(ctx, s.logger).Warn("failed to instantiate shipping provider — skipping",
				zap.String("integration_id", rows[i].ID),
				zap.String("provider", rows[i].Provider),
				zap.Error(err),
			)
			continue
		}
		sp, ok := provider.(providers.ShippingProvider)
		if !ok {
			logger.From(ctx, s.logger).Warn("integration is marked as shipping but does not implement ShippingProvider",
				zap.String("integration_id", rows[i].ID),
				zap.String("provider", rows[i].Provider),
			)
			continue
		}
		out = append(out, sp)
	}
	return out, nil
}

// GetShippingProviderByName returns the ShippingProvider of a specific
// integration (store + provider name). Returns httpx.ErrNotFound when the
// integration is absent, httpx.ErrUnprocessable when it exists but is not
// active.
func (s *Service) GetShippingProviderByName(ctx context.Context, storeID string, providerName providers.ProviderName) (providers.ShippingProvider, error) {
	integration, err := s.repo.GetByProvider(ctx, storeID, string(providers.ProviderTypeShipping), string(providerName))
	if err != nil {
		return nil, err
	}
	if integration.Status != "active" {
		return nil, httpx.ErrUnprocessable(fmt.Sprintf("%s integration is not active (status=%s)", providerName, integration.Status))
	}
	provider, err := s.createProviderFromRow(ctx, integration)
	if err != nil {
		return nil, err
	}
	sp, ok := provider.(providers.ShippingProvider)
	if !ok {
		return nil, httpx.ErrUnprocessable("failed to cast to shipping provider")
	}
	return sp, nil
}

// GetShippingOrderProvider returns the ShippingOrderProvider for a specific
// integration (identified by store + provider name). Returns
// providers.ErrOperationNotSupported when the provider does not support the
// order-lifecycle operations (quote-only aggregators).
func (s *Service) GetShippingOrderProvider(ctx context.Context, storeID string, providerName providers.ProviderName) (providers.ShippingOrderProvider, error) {
	sp, err := s.GetShippingProviderByName(ctx, storeID, providerName)
	if err != nil {
		return nil, err
	}
	osp, ok := sp.(providers.ShippingOrderProvider)
	if !ok {
		return nil, providers.ErrOperationNotSupported
	}
	return osp, nil
}

// ConnectSmartEnviosInput is the admin payload to set up or rotate the
// SmartEnvios integration for a store.
type ConnectSmartEnviosInput struct {
	StoreID string
	Token   string
	Env     string // "sandbox" | "production" — defaults to "production"
}

// ConnectSmartEnviosOutput mirrors CreateIntegrationOutput for API responses.
type ConnectSmartEnviosOutput = CreateIntegrationOutput

// ConnectSmartEnvios validates a SmartEnvios token via a live call to
// /quote/services and persists (or updates) the integration as active. No
// OAuth involved — token is static and provided by the merchant.
func (s *Service) ConnectSmartEnvios(ctx context.Context, input ConnectSmartEnviosInput) (*ConnectSmartEnviosOutput, error) {
	token := strings.TrimSpace(input.Token)
	if token == "" {
		return nil, httpx.ErrBadRequest("token is required")
	}
	env := input.Env
	if env == "" {
		env = "production"
	}
	if env != "production" && env != "sandbox" {
		return nil, httpx.ErrBadRequest("env must be 'sandbox' or 'production'")
	}

	// Validate the token with a real call so we never persist garbage.
	creds := &providers.Credentials{AccessToken: token}
	probe, err := s.factory.CreateShippingProvider(providers.ProviderConfig{
		IntegrationID: "probe",
		StoreID:       input.StoreID,
		Type:          providers.ProviderTypeShipping,
		Name:          providers.ProviderSmartEnvios,
		Credentials:   creds,
		Metadata:      map[string]any{"environment": env},
	})
	if err != nil {
		return nil, fmt.Errorf("instantiating smartenvios provider: %w", err)
	}
	// TestConnection follows the shared provider convention: it returns
	// (result, nil) on both success AND failure — failures are surfaced via
	// result.Success == false. Checking only `err != nil` silently accepts
	// bad tokens and stores the integration as active. Check both.
	probeResult, err := probe.TestConnection(ctx)
	if err != nil {
		return nil, httpx.ErrUnprocessable("falha ao validar token SmartEnvios: " + err.Error())
	}
	if probeResult == nil || !probeResult.Success {
		msg := "token rejeitado pela SmartEnvios"
		if probeResult != nil && probeResult.Message != "" {
			msg = probeResult.Message
		}
		return nil, httpx.ErrUnprocessable("falha ao validar token SmartEnvios: " + msg)
	}

	encrypted, err := s.encryptor.EncryptJSON(creds)
	if err != nil {
		return nil, fmt.Errorf("encrypting smartenvios token: %w", err)
	}

	// If an integration already exists, update it; otherwise create it.
	// The repository surfaces "not found" as httpx-wrapped errors whose kind
	// is opaque here — we treat any error that mentions "not found" as
	// "integration is missing, go create it".
	existing, err := s.repo.GetByProvider(ctx, input.StoreID, string(providers.ProviderTypeShipping), string(providers.ProviderSmartEnvios))
	if err != nil {
		if !strings.Contains(strings.ToLower(err.Error()), "not found") {
			return nil, err
		}
		existing = nil
	}

	// Build metadata the admin UI can use to render "Informações da Conta".
	// SmartEnvios has no /me endpoint, so we surface what we can observe:
	// environment + list of enabled carrier services. The `accountName` field
	// mirrors the shape used by other providers so the UI doesn't need a
	// provider-specific branch to pick something to display.
	metadata := map[string]any{"environment": env}
	if probeResult != nil && probeResult.AccountInfo != nil {
		if names, ok := probeResult.AccountInfo["service_names"].([]string); ok && len(names) > 0 {
			metadata["accountName"] = fmt.Sprintf("%d serviços habilitados", len(names))
			metadata["enabledServices"] = names
		}
	}
	if existing != nil {
		if err := s.repo.UpdateCredentials(ctx, existing.ID, encrypted, nil); err != nil {
			return nil, err
		}
		if err := s.repo.UpdateMetadata(ctx, existing.ID, metadata); err != nil {
			return nil, err
		}
		if err := s.repo.UpdateStatus(ctx, existing.ID, "active"); err != nil {
			return nil, err
		}
		row, err := s.repo.GetByID(ctx, existing.ID, input.StoreID)
		if err != nil {
			return nil, err
		}
		return s.toCreateOutput(row), nil
	}
	row, err := s.repo.Create(ctx, CreateIntegrationParams{
		StoreID:     input.StoreID,
		Type:        string(providers.ProviderTypeShipping),
		Provider:    string(providers.ProviderSmartEnvios),
		Status:      "active",
		Credentials: encrypted,
		Metadata:    metadata,
	})
	if err != nil {
		return nil, err
	}
	return s.toCreateOutput(row), nil
}

// =============================================================================
// CONNECT PAGAR.ME
// =============================================================================

// ConnectPagarmeInput is the admin payload to set up or rotate the Pagar.me
// payment integration for a store. Pagar.me uses static API keys (no OAuth),
// so the merchant pastes secret + public into a form and we validate live.
type ConnectPagarmeInput struct {
	StoreID         string
	SecretKey       string
	PublicKey       string
	WebhookUsername string
	WebhookPassword string
}

// ConnectPagarmeOutput mirrors CreateIntegrationOutput for API responses.
type ConnectPagarmeOutput = CreateIntegrationOutput

// ConnectPagarme validates a Pagar.me secret key via TestConnection and
// persists (or updates) the integration as active. The "environment" is
// derived from the key prefix (sk_test_ → sandbox, plain sk_ → production)
// — Pagar.me has no separate sandbox host; the prefix is the only switch.
// The live TestConnection probe below is the authoritative check: the prefix
// rule only catches obvious mistakes like swapping the secret and public keys.
func (s *Service) ConnectPagarme(ctx context.Context, input ConnectPagarmeInput) (*ConnectPagarmeOutput, error) {
	secret := strings.TrimSpace(input.SecretKey)
	public := strings.TrimSpace(input.PublicKey)
	if secret == "" {
		return nil, httpx.ErrBadRequest("chave secreta é obrigatória")
	}
	if public == "" {
		return nil, httpx.ErrBadRequest("chave pública é obrigatória")
	}

	secretEnv := pagarmeKeyEnvironment(secret, "sk_")
	publicEnv := pagarmeKeyEnvironment(public, "pk_")
	if secretEnv == "" {
		return nil, httpx.ErrBadRequest("chave secreta inválida — deve começar com sk_")
	}
	if publicEnv == "" {
		return nil, httpx.ErrBadRequest("chave pública inválida — deve começar com pk_")
	}
	if secretEnv != publicEnv {
		return nil, httpx.ErrBadRequest("as chaves precisam ser do mesmo ambiente (ambas de teste ou ambas de produção)")
	}

	creds := &providers.Credentials{
		APIKey:    secret,
		APISecret: public,
		Extra: map[string]any{
			"public_key":  public,
			"environment": secretEnv,
		},
	}
	if input.WebhookUsername != "" {
		creds.Extra["webhook_username"] = input.WebhookUsername
	}
	if input.WebhookPassword != "" {
		creds.Extra["webhook_password"] = input.WebhookPassword
	}

	probe, err := s.factory.CreatePaymentProvider(providers.ProviderConfig{
		IntegrationID: "probe",
		StoreID:       input.StoreID,
		Type:          providers.ProviderTypePayment,
		Name:          providers.ProviderPagarme,
		Credentials:   creds,
	})
	if err != nil {
		return nil, fmt.Errorf("instantiating pagarme provider: %w", err)
	}
	probeResult, err := probe.TestConnection(ctx)
	if err != nil {
		return nil, httpx.ErrUnprocessable("falha ao validar chave Pagar.me: " + err.Error())
	}
	if probeResult == nil || !probeResult.Success {
		msg := "chave rejeitada pela Pagar.me"
		if probeResult != nil && probeResult.Message != "" {
			msg = probeResult.Message
		}
		return nil, httpx.ErrUnprocessable("falha ao validar chave Pagar.me: " + msg)
	}

	encrypted, err := s.encryptor.EncryptJSON(creds)
	if err != nil {
		return nil, fmt.Errorf("encrypting pagarme credentials: %w", err)
	}

	metadata := map[string]any{
		"public_key":  public,
		"environment": secretEnv,
	}
	if probeResult.AccountInfo != nil {
		if name, ok := probeResult.AccountInfo["name"].(string); ok && name != "" {
			metadata["accountName"] = name
		}
	}

	existing, err := s.repo.GetByProvider(ctx, input.StoreID, string(providers.ProviderTypePayment), string(providers.ProviderPagarme))
	if err != nil {
		if !strings.Contains(strings.ToLower(err.Error()), "not found") {
			return nil, err
		}
		existing = nil
	}

	if existing != nil {
		if err := s.repo.UpdateCredentials(ctx, existing.ID, encrypted, nil); err != nil {
			return nil, err
		}
		if err := s.repo.UpdateMetadata(ctx, existing.ID, metadata); err != nil {
			return nil, err
		}
		if err := s.repo.UpdateStatus(ctx, existing.ID, "active"); err != nil {
			return nil, err
		}
		row, err := s.repo.GetByID(ctx, existing.ID, input.StoreID)
		if err != nil {
			return nil, err
		}
		return s.toCreateOutput(row), nil
	}

	row, err := s.repo.Create(ctx, CreateIntegrationParams{
		StoreID:     input.StoreID,
		Type:        string(providers.ProviderTypePayment),
		Provider:    string(providers.ProviderPagarme),
		Status:      "active",
		Credentials: encrypted,
		Metadata:    metadata,
	})
	if err != nil {
		return nil, err
	}
	return s.toCreateOutput(row), nil
}

// ValidatePagarmeWebhookAuth reads the integration's stored webhook
// username/password (set at connect time, optional) and validates an
// inbound `Authorization: Basic ...` header against them. Returns
// (true, nil) when the merchant has not configured Basic Auth — we
// can't verify what wasn't provided, and Pagar.me does not sign payloads
// so there is no fallback verification path. Returns (false, nil) only
// when creds ARE configured and the inbound auth is wrong/missing.
func (s *Service) ValidatePagarmeWebhookAuth(ctx context.Context, storeID, authHeader string) (bool, error) {
	row, err := s.repo.GetByProvider(ctx, storeID, string(providers.ProviderTypePayment), string(providers.ProviderPagarme))
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "not found") {
			return true, nil
		}
		return false, err
	}
	creds, err := s.decryptCredentials(row.Credentials)
	if err != nil {
		return false, err
	}
	expectedUser, _ := creds.Extra["webhook_username"].(string)
	expectedPass, _ := creds.Extra["webhook_password"].(string)
	if expectedUser == "" && expectedPass == "" {
		return true, nil
	}

	const prefix = "Basic "
	if !strings.HasPrefix(authHeader, prefix) {
		return false, nil
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(authHeader, prefix))
	if err != nil {
		return false, nil
	}
	parts := strings.SplitN(string(decoded), ":", 2)
	if len(parts) != 2 {
		return false, nil
	}
	gotUser, gotPass := parts[0], parts[1]
	userOK := subtle.ConstantTimeCompare([]byte(gotUser), []byte(expectedUser)) == 1
	passOK := subtle.ConstantTimeCompare([]byte(gotPass), []byte(expectedPass)) == 1
	return userOK && passOK, nil
}

// pagarmeKeyEnvironment returns "sandbox" / "production" / "" based on the key
// prefix. Pagar.me only tags SANDBOX keys: sk_test_/pk_test_. Production keys
// carry no environment segment — they are just sk_/pk_ followed by the token.
// There is no sk_live_ (that's Stripe's convention); assuming there was made
// the connect form reject every real production key. Returns "" only when the
// key lacks the scope prefix entirely (e.g. secret and public swapped).
func pagarmeKeyEnvironment(key, scope string) string {
	if !strings.HasPrefix(key, scope) || len(key) <= len(scope) {
		return ""
	}
	if strings.HasPrefix(key, scope+"test_") {
		return "sandbox"
	}
	return "production"
}

// GetSocialProvider returns a SocialProvider for the given integration.
func (s *Service) GetSocialProvider(ctx context.Context, integrationID, storeID string) (providers.SocialProvider, error) {
	integration, err := s.repo.GetByID(ctx, integrationID, storeID)
	if err != nil {
		return nil, err
	}

	if integration.Type != string(providers.ProviderTypeSocial) {
		return nil, httpx.ErrUnprocessable("integration is not a social provider")
	}

	provider, err := s.createProviderFromRow(ctx, integration)
	if err != nil {
		return nil, err
	}

	socialProvider, ok := provider.(providers.SocialProvider)
	if !ok {
		return nil, httpx.ErrUnprocessable("failed to cast to social provider")
	}

	return socialProvider, nil
}

// SendInstagramDM resolves the active Instagram integration of a store and sends a DM
// to the given platform user. Best-effort: callers should treat errors as non-fatal.
func (s *Service) SendInstagramDM(ctx context.Context, storeID, recipientID, text string) error {
	// GetByProvider returns httpx.ErrNotFound when there is no integration —
	// no need for a separate nil check.
	integration, err := s.repo.GetByProvider(ctx, storeID, "social", "instagram")
	if err != nil {
		return fmt.Errorf("instagram integration unavailable: %w", err)
	}
	if integration.Status != "active" {
		return fmt.Errorf("instagram integration is not active (status=%s)", integration.Status)
	}

	provider, err := s.createProviderFromRow(ctx, integration)
	if err != nil {
		return fmt.Errorf("instantiating instagram provider: %w", err)
	}

	socialProvider, ok := provider.(providers.SocialProvider)
	if !ok {
		return fmt.Errorf("provider is not a social provider")
	}

	if err := socialProvider.SendDirectMessage(ctx, recipientID, text); err != nil {
		logger.From(ctx, s.logger).Warn("failed to send instagram dm",
			zap.String("store_id", storeID),
			zap.String("recipient_id", recipientID),
			zap.Error(err),
		)
		return err
	}

	logger.From(ctx, s.logger).Info("instagram dm sent",
		zap.String("store_id", storeID),
		zap.String("recipient_id", recipientID),
	)
	return nil
}

// ReplyToInstagramComment resolves the active Instagram integration of a store and sends
// a private DM to the user who made the comment using Instagram's Private Reply feature.
// This sends a DM in response to a comment (not a public reply).
func (s *Service) ReplyToInstagramComment(ctx context.Context, storeID, commentID, text string) error {
	integration, err := s.repo.GetByProvider(ctx, storeID, "social", "instagram")
	if err != nil {
		return fmt.Errorf("instagram integration unavailable: %w", err)
	}
	if integration.Status != "active" {
		return fmt.Errorf("instagram integration is not active (status=%s)", integration.Status)
	}

	provider, err := s.createProviderFromRow(ctx, integration)
	if err != nil {
		return fmt.Errorf("instantiating instagram provider: %w", err)
	}

	socialProvider, ok := provider.(providers.SocialProvider)
	if !ok {
		return fmt.Errorf("provider is not a social provider")
	}

	// Use Private Reply to send a DM in response to the comment
	if err := socialProvider.SendPrivateReply(ctx, commentID, text); err != nil {
		// [IGTRACE] TODO remover — o par (origem do comentário, recusa do
		// private reply) é o que sustenta a tese de que a Meta só aceita o
		// comment_id que ELA empurra pelo webhook.
		logger.From(ctx, s.logger).Warn(TracePrefixIG+"reply refused by Instagram",
			zap.String("comment_id", commentID),
		)
		logger.From(ctx, s.logger).Warn("failed to send private reply to instagram comment",
			zap.String("store_id", storeID),
			zap.String("comment_id", commentID),
			zap.Error(err),
		)
		return err
	}

	// Instagram allows exactly ONE private reply per comment — record that this
	// one is spent so the resend lookup picks a different (fresh) comment.
	if err := s.repo.MarkLiveCommentPrivateReplyUsed(ctx, commentID); err != nil {
		logger.From(ctx, s.logger).Warn("failed to mark private reply as used",
			zap.String("comment_id", commentID), zap.Error(err))
	}

	logger.From(ctx, s.logger).Info("instagram private reply sent",
		zap.String("store_id", storeID),
		zap.String("comment_id", commentID),
	)
	return nil
}

// resolveInstagramSocialProvider returns the active Instagram social provider
// for a store, or an error when the integration is missing/inactive.
func (s *Service) resolveInstagramSocialProvider(ctx context.Context, storeID string) (providers.SocialProvider, error) {
	integration, err := s.repo.GetByProvider(ctx, storeID, "social", "instagram")
	if err != nil {
		return nil, fmt.Errorf("instagram integration unavailable: %w", err)
	}
	if integration.Status != "active" {
		return nil, fmt.Errorf("instagram integration is not active (status=%s)", integration.Status)
	}
	provider, err := s.createProviderFromRow(ctx, integration)
	if err != nil {
		return nil, fmt.Errorf("instantiating instagram provider: %w", err)
	}
	socialProvider, ok := provider.(providers.SocialProvider)
	if !ok {
		return nil, fmt.Errorf("provider is not a social provider")
	}
	return socialProvider, nil
}

// PublicReplyToInstagramComment posts a PUBLIC reply comment under the buyer's
// comment (visible on the live/post), as opposed to ReplyToInstagramComment
// which sends a private DM. Used by the comment-moderation surface.
func (s *Service) PublicReplyToInstagramComment(ctx context.Context, storeID, commentID, text string) error {
	provider, err := s.resolveInstagramSocialProvider(ctx, storeID)
	if err != nil {
		return err
	}
	if err := provider.ReplyToComment(ctx, commentID, text); err != nil {
		logger.From(ctx, s.logger).Warn("failed to post public reply to instagram comment",
			zap.String("store_id", storeID),
			zap.String("comment_id", commentID),
			zap.Error(err),
		)
		return err
	}
	logger.From(ctx, s.logger).Info("instagram public reply posted",
		zap.String("store_id", storeID),
		zap.String("comment_id", commentID),
	)
	return nil
}

// HideInstagramComment hides or unhides a comment on the connected account.
func (s *Service) HideInstagramComment(ctx context.Context, storeID, commentID string, hidden bool) error {
	provider, err := s.resolveInstagramSocialProvider(ctx, storeID)
	if err != nil {
		return err
	}
	if err := provider.HideComment(ctx, commentID, hidden); err != nil {
		logger.From(ctx, s.logger).Warn("failed to hide instagram comment",
			zap.String("store_id", storeID),
			zap.String("comment_id", commentID),
			zap.Bool("hidden", hidden),
			zap.Error(err),
		)
		return err
	}
	// Mirror the hidden state locally: a hidden comment can't receive a private
	// reply, so the resend lookup must skip it (and pick it back up on unhide).
	if err := s.repo.SetLiveCommentHidden(ctx, commentID, hidden); err != nil {
		logger.From(ctx, s.logger).Warn("failed to mirror comment hidden state",
			zap.String("comment_id", commentID), zap.Error(err))
	}
	logger.From(ctx, s.logger).Info("instagram comment hidden",
		zap.String("store_id", storeID),
		zap.String("comment_id", commentID),
		zap.Bool("hidden", hidden),
	)
	return nil
}

// DeleteInstagramComment deletes a comment on the connected account.
func (s *Service) DeleteInstagramComment(ctx context.Context, storeID, commentID string) error {
	provider, err := s.resolveInstagramSocialProvider(ctx, storeID)
	if err != nil {
		return err
	}
	if err := provider.DeleteComment(ctx, commentID); err != nil {
		logger.From(ctx, s.logger).Warn("failed to delete instagram comment",
			zap.String("store_id", storeID),
			zap.String("comment_id", commentID),
			zap.Error(err),
		)
		return err
	}
	// Mirror the deletion locally so the comment leaves the merchant's list, just
	// like it left the Instagram post. This also excludes it from the resend
	// lookup — a deleted comment can never receive a private reply.
	if err := s.repo.MarkLiveCommentDeleted(ctx, commentID); err != nil {
		logger.From(ctx, s.logger).Warn("failed to mirror comment deletion",
			zap.String("comment_id", commentID), zap.Error(err))
	}
	logger.From(ctx, s.logger).Info("instagram comment deleted",
		zap.String("store_id", storeID),
		zap.String("comment_id", commentID),
	)
	return nil
}

// FetchInstagramLives retrieves all active Instagram lives for a store.
// Returns an empty slice if no lives are currently streaming.
func (s *Service) FetchInstagramLives(ctx context.Context, storeID string) ([]providers.LiveMedia, error) {
	integration, err := s.repo.GetByProvider(ctx, storeID, "social", "instagram")
	if err != nil {
		return nil, fmt.Errorf("instagram integration unavailable: %w", err)
	}
	if integration.Status != "active" {
		return nil, fmt.Errorf("instagram integration is not active (status=%s)", integration.Status)
	}

	provider, err := s.createProviderFromRow(ctx, integration)
	if err != nil {
		return nil, fmt.Errorf("instantiating instagram provider: %w", err)
	}

	socialProvider, ok := provider.(providers.SocialProvider)
	if !ok {
		return nil, fmt.Errorf("provider is not a social provider")
	}

	lives, err := socialProvider.GetActiveLives(ctx)
	if err != nil {
		logger.From(ctx, s.logger).Warn("failed to fetch instagram lives",
			zap.String("store_id", storeID),
			zap.Error(err),
		)
		return nil, err
	}

	logger.From(ctx, s.logger).Info("fetched instagram lives",
		zap.String("store_id", storeID),
		zap.Int("count", len(lives)),
	)
	return lives, nil
}

// FetchInstagramMedia lists recent published posts/reels of the store's
// connected Instagram account (newest first), for the post-event selector.
// `after` pages through results.
func (s *Service) FetchInstagramMedia(ctx context.Context, storeID string, limit int, after string) (*providers.MediaPage, error) {
	provider, err := s.resolveInstagramSocialProvider(ctx, storeID)
	if err != nil {
		return nil, err
	}
	page, err := provider.GetUserMedia(ctx, limit, after)
	if err != nil {
		logger.From(ctx, s.logger).Warn("failed to fetch instagram media",
			zap.String("store_id", storeID),
			zap.Error(err),
		)
		return nil, err
	}
	logger.From(ctx, s.logger).Info("fetched instagram media",
		zap.String("store_id", storeID),
		zap.Int("count", len(page.Posts)),
	)
	return page, nil
}

// CreateInstagramPostEvent publishes an image post on the connected Instagram
// account and creates a post-commerce event bound to the new post, in one step.
// Reuses live.Service.CreatePostEvent so the post then sells via comments exactly
// like a manually-selected post.
func (s *Service) CreateInstagramPostEvent(ctx context.Context, input CreateInstagramPostInput) (live.CreateLiveOutput, error) {
	return s.publishWithIdempotency(ctx, input, "create_instagram_post",
		func() { s.deleteTransientImage(ctx, input.ImageKey) },
		func(prior priorPublishAttempt) (live.CreateLiveOutput, error) {
			return s.publishInstagramPostEvent(ctx, input, prior)
		},
	)
}

func (s *Service) publishInstagramPostEvent(ctx context.Context, input CreateInstagramPostInput, prior priorPublishAttempt) (live.CreateLiveOutput, error) {
	provider, err := s.resolveInstagramSocialProvider(ctx, input.StoreID)
	if err != nil {
		return live.CreateLiveOutput{}, err
	}

	// Publish the image post — a menos que a tentativa anterior já tenha
	// publicado e falhado só no vínculo: aí a mídia existe e é reusada.
	mediaID := prior.MediaID
	if mediaID == "" {
		mediaID, err = s.publishOrResume(ctx, provider, prior, false, func() (string, error) {
			return provider.PublishImagePost(ctx, input.ImageURL, input.Caption)
		})
		if err != nil {
			logger.From(ctx, s.logger).Warn("failed to publish instagram image post",
				zap.String("store_id", input.StoreID), zap.Error(err))
			// Clean up the uploaded image even when publishing fails, so a failed
			// attempt doesn't leave an orphan in storage.
			s.deleteTransientImage(ctx, input.ImageKey)
			if errors.Is(err, providers.ErrPublishOutcomeUnknown) {
				return live.CreateLiveOutput{}, errors.Join(
					httpx.DomainError(422, httpx.CodeIgPublishUnconfirmed, "Instagram did not confirm the publish — try again in a few seconds (the retry resumes this publish, it will not duplicate the post)"),
					err)
			}
			return live.CreateLiveOutput{}, httpx.ErrUnprocessable("failed to publish the post on Instagram")
		}

		// Instagram has now fetched and stored the image, so the transient upload
		// can be removed. Logged (not swallowed) so a delete failure is visible.
		s.deleteTransientImage(ctx, input.ImageKey)
	}

	// Fetch the new post's permalink/thumbnail (best-effort).
	permalink, thumbnail := "", ""
	if details, dErr := provider.GetMediaDetails(ctx, mediaID); dErr == nil && details != nil {
		permalink = details.Permalink
		thumbnail = details.ThumbnailURL
		if thumbnail == "" {
			thumbnail = details.MediaURL
		}
	}

	// Liga a publicação: sessão do evento informado, ou evento novo.
	out, err := s.attachPublishedMediaToEvent(ctx, input, live.SessionTypePost, mediaID, permalink, thumbnail)
	if err != nil {
		// The post is already live on Instagram; surface the event error so the
		// merchant can retry binding via "select a post".
		logger.From(ctx, s.logger).Error("post published but event creation failed",
			zap.String("store_id", input.StoreID),
			zap.String("media_id", mediaID),
			zap.Error(err))
		return live.CreateLiveOutput{}, &publishedUnboundError{MediaID: mediaID, Err: err}
	}

	logger.From(ctx, s.logger).Info("instagram post created and event bound",
		zap.String("store_id", input.StoreID),
		zap.String("media_id", mediaID),
		zap.String("event_id", out.ID),
	)
	return out, nil
}

// deleteTransientImage removes a just-published post image from storage. The
// result is logged (success or failure) so a stuck object is visible — the
// presigned URL also expires on its own, so this is best-effort.
func (s *Service) deleteTransientImage(ctx context.Context, key string) {
	if key == "" {
		return
	}
	if s.storage == nil {
		logger.From(ctx, s.logger).Warn("cannot delete post image: storage not wired", zap.String("key", key))
		return
	}
	if err := s.storage.DeleteByKey(ctx, key); err != nil {
		logger.From(ctx, s.logger).Error("failed to delete post image from storage",
			zap.String("key", key), zap.Error(err))
		return
	}
	logger.From(ctx, s.logger).Info("deleted transient post image", zap.String("key", key))
}

// publishWithIdempotency dedupes a publish+bind so a retried submit (e.g. the
// client timed out on a slow publish and the user resubmitted) returns the
// original event instead of publishing the same media to Instagram again.
//
// Dedup is by explicit client key when provided, falling back to a hash of the
// stable inputs within a short window — the uploaded media URL/key are excluded
// from that hash since they differ on every upload, so a resubmit of the same
// post still matches. On a cache hit, onDuplicate cleans up the just-uploaded
// transient media that won't be published.
func (s *Service) publishWithIdempotency(
	ctx context.Context,
	input CreateInstagramPostInput,
	operation string,
	onDuplicate func(),
	publish func(priorPublishAttempt) (live.CreateLiveOutput, error),
) (live.CreateLiveOutput, error) {
	integrationID := s.instagramIntegrationID(ctx, input.StoreID)
	// Without the idempotency service or a known integration we can't dedup
	// safely (the record FK needs the integration id) — publish directly.
	if s.idempotency == nil || integrationID == "" {
		return publish(priorPublishAttempt{})
	}

	// Stable dedup payload: everything that identifies the post EXCEPT the
	// per-upload media url/key, so resubmits of the same content collide.
	dedup := struct {
		Operation              string
		Title                  string
		Caption                string
		ProductIDs             []string
		StartsAt               *time.Time
		EndsAt                 *time.Time
		CartExpirationMinutes  *int
		CartMaxQuantityPerItem *int
	}{
		Operation:              operation,
		Title:                  input.Title,
		Caption:                input.Caption,
		ProductIDs:             input.ProductIDs,
		StartsAt:               input.StartsAt,
		EndsAt:                 input.EndsAt,
		CartExpirationMinutes:  input.CartExpirationMinutes,
		CartMaxQuantityPerItem: input.CartMaxQuantityPerItem,
	}

	req := idempotency.CheckRequest{
		IdempotencyKey: input.IdempotencyKey,
		StoreID:        input.StoreID,
		IntegrationID:  integrationID,
		Operation:      operation,
		Payload:        dedup,
	}

	if cached, err := s.idempotency.Check(ctx, req); err != nil {
		logger.From(ctx, s.logger).Warn("instagram publish idempotency check failed", zap.Error(err))
	} else if cached != nil && cached.Found {
		var out live.CreateLiveOutput
		if json.Unmarshal(cached.Response, &out) == nil && out.ID != "" {
			logger.From(ctx, s.logger).Info("instagram publish deduped, returning original event",
				zap.String("store_id", input.StoreID),
				zap.String("operation", operation),
				zap.String("event_id", out.ID))
			if onDuplicate != nil {
				onDuplicate()
			}
			return out, nil
		}
	}

	claim, err := s.idempotency.Claim(ctx, req)
	if err != nil {
		switch {
		case errors.Is(err, idempotency.ErrInFlight):
			// O antecessor tratava a colisão como "não consegui registrar" e
			// publicava assim mesmo — em 19/08/2026 isso criou o segundo
			// story enquanto o primeiro (timeout que ENTROU) ainda vivia.
			return live.CreateLiveOutput{}, httpx.DomainError(409, httpx.CodeIgPublishInFlight, "this publish is already in progress — wait a few seconds and check the event list")
		case errors.Is(err, idempotency.ErrPayloadMismatch):
			return live.CreateLiveOutput{}, httpx.DomainError(422, httpx.CodeIdempotencyKeyReused, "idempotency key reused with different content")
		}
		// Falha de infra na trava: publicar mesmo assim, para a
		// indisponibilidade do registro não derrubar a lojista.
		logger.From(ctx, s.logger).Warn("instagram publish idempotency claim failed", zap.Error(err))
		return publish(priorPublishAttempt{})
	}
	if claim.Completed != nil {
		var out live.CreateLiveOutput
		if json.Unmarshal(claim.Completed.Response, &out) == nil && out.ID != "" {
			logger.From(ctx, s.logger).Info("instagram publish deduped, returning original event",
				zap.String("store_id", input.StoreID),
				zap.String("operation", operation),
				zap.String("event_id", out.ID))
			if onDuplicate != nil {
				onDuplicate()
			}
			return out, nil
		}
		// Completou mas a resposta não é legível: repetir publicaria de novo.
		return live.CreateLiveOutput{}, httpx.DomainError(409, httpx.CodeIgAlreadyPublished, "this content was already published")
	}
	if claim.Unguarded {
		return publish(priorPublishAttempt{})
	}

	prior := priorPublishAttempt{}
	if claim.Reclaimed {
		prior = priorFromFailurePayload(claim.PriorResponse, claim.Record.CreatedAt)
		if prior.ContainerID != "" || prior.MediaID != "" {
			logger.From(ctx, s.logger).Info("instagram publish retry resuming prior attempt",
				zap.String("store_id", input.StoreID),
				zap.String("operation", operation),
				zap.String("container_id", prior.ContainerID),
				zap.String("media_id", prior.MediaID))
		}
	}

	out, pErr := publish(prior)
	if pErr != nil {
		var unknown *providers.PublishOutcomeUnknownError
		var unbound *publishedUnboundError
		switch {
		case errors.As(pErr, &unknown):
			// O container fica gravado para o retry RETOMAR — nunca duplicar.
			_ = s.idempotency.FailWithMeta(ctx, claim.Record.ID, pErr, map[string]any{
				"outcome":      "unknown",
				"container_id": unknown.ContainerID,
			})
		case errors.As(pErr, &unbound):
			// Publicou no Instagram e falhou só no vínculo com o evento: o
			// retry reusa a mídia publicada em vez de postar outra igual.
			_ = s.idempotency.FailWithMeta(ctx, claim.Record.ID, pErr, map[string]any{
				"outcome":  "published_unbound",
				"media_id": unbound.MediaID,
			})
		default:
			_ = s.idempotency.Fail(ctx, claim.Record.ID, pErr)
		}
		return out, pErr
	}
	_ = s.idempotency.Complete(ctx, claim.Record.ID, out)
	return out, nil
}

// priorPublishAttempt é o que uma tentativa anterior deixou para trás no
// registro de idempotência: um container de desfecho desconhecido para
// retomar, ou uma mídia já publicada cujo vínculo com o evento falhou.
type priorPublishAttempt struct {
	ContainerID string
	MediaID     string
	NotBefore   time.Time
}

// publishedUnboundError marca "publicou no Instagram, falhou no vínculo com o
// evento" — a mídia existe e o retry precisa dela, não de uma cópia.
type publishedUnboundError struct {
	MediaID string
	Err     error
}

func (e *publishedUnboundError) Error() string {
	return "media " + e.MediaID + " published but event binding failed: " + e.Err.Error()
}

func (e *publishedUnboundError) Unwrap() error { return e.Err }

// priorFromFailurePayload lê do payload de falha (gravado por FailWithMeta) o
// que a tentativa anterior deixou para o retry retomar.
func priorFromFailurePayload(payload []byte, createdAt time.Time) priorPublishAttempt {
	if len(payload) == 0 {
		return priorPublishAttempt{}
	}
	var meta struct {
		Outcome     string `json:"outcome"`
		ContainerID string `json:"container_id"`
		MediaID     string `json:"media_id"`
	}
	if json.Unmarshal(payload, &meta) != nil {
		return priorPublishAttempt{}
	}
	switch meta.Outcome {
	case "unknown":
		return priorPublishAttempt{ContainerID: meta.ContainerID, NotBefore: createdAt}
	case "published_unbound":
		return priorPublishAttempt{MediaID: meta.MediaID, NotBefore: createdAt}
	}
	return priorPublishAttempt{}
}

// publishOrResume aplica a regra central do incidente dos 2 stories: retry de
// publicação NUNCA cria outro container às cegas. Se a tentativa anterior
// deixou um container de desfecho desconhecido, primeiro pergunta ao
// Instagram o que aconteceu com ele; só um container comprovadamente morto
// libera uma publicação nova.
func (s *Service) publishOrResume(ctx context.Context, provider providers.SocialProvider, prior priorPublishAttempt, isStory bool, fresh func() (string, error)) (string, error) {
	resumer, ok := provider.(interface {
		ResumeContainerPublish(ctx context.Context, containerID string, isStory bool, notBefore time.Time) (string, error)
	})
	if prior.ContainerID == "" || !ok {
		return fresh()
	}
	mediaID, err := resumer.ResumeContainerPublish(ctx, prior.ContainerID, isStory, prior.NotBefore)
	switch {
	case err == nil:
		return mediaID, nil
	case errors.Is(err, providers.ErrContainerDead):
		logger.From(ctx, s.logger).Info("prior publish container is dead; publishing fresh",
			zap.String("container_id", prior.ContainerID))
		return fresh()
	default:
		// Desfecho continua desconhecido — parar preserva a trava.
		return "", err
	}
}

// instagramIntegrationID returns the store's Instagram integration id, or ""
// when none is available (used as the idempotency record FK).
func (s *Service) instagramIntegrationID(ctx context.Context, storeID string) string {
	integ, err := s.repo.GetByProvider(ctx, storeID, "social", "instagram")
	if err != nil || integ == nil {
		return ""
	}
	return integ.ID
}

// CreateInstagramReelEvent publishes a Reel from a public video URL and creates
// the bound post event. The transient video (input.ImageKey) is deleted after.
func (s *Service) CreateInstagramReelEvent(ctx context.Context, input CreateInstagramPostInput, videoURL string) (live.CreateLiveOutput, error) {
	return s.publishWithIdempotency(ctx, input, "create_instagram_reel",
		func() { s.deleteTransientImage(ctx, input.ImageKey) },
		func(prior priorPublishAttempt) (live.CreateLiveOutput, error) {
			return s.publishInstagramReelEvent(ctx, input, videoURL, prior)
		},
	)
}

func (s *Service) publishInstagramReelEvent(ctx context.Context, input CreateInstagramPostInput, videoURL string, prior priorPublishAttempt) (live.CreateLiveOutput, error) {
	provider, err := s.resolveInstagramSocialProvider(ctx, input.StoreID)
	if err != nil {
		return live.CreateLiveOutput{}, err
	}

	mediaID := prior.MediaID
	if mediaID == "" {
		mediaID, err = s.publishOrResume(ctx, provider, prior, false, func() (string, error) {
			return provider.PublishReel(ctx, videoURL, input.Caption)
		})
		if err != nil {
			logger.From(ctx, s.logger).Warn("failed to publish instagram reel",
				zap.String("store_id", input.StoreID), zap.Error(err))
			s.deleteTransientImage(ctx, input.ImageKey)
			if errors.Is(err, providers.ErrPublishOutcomeUnknown) {
				return live.CreateLiveOutput{}, errors.Join(
					httpx.DomainError(422, httpx.CodeIgPublishUnconfirmed, "Instagram did not confirm the publish — try again in a few seconds (the retry resumes this publish, it will not duplicate the reel)"),
					err)
			}
			return live.CreateLiveOutput{}, httpx.ErrUnprocessable("failed to publish the reel on Instagram")
		}

		// Instagram has fetched and processed the video; remove the transient upload.
		s.deleteTransientImage(ctx, input.ImageKey)
	}

	permalink, thumbnail := "", ""
	if details, dErr := provider.GetMediaDetails(ctx, mediaID); dErr == nil && details != nil {
		permalink = details.Permalink
		thumbnail = details.ThumbnailURL
		if thumbnail == "" {
			thumbnail = details.MediaURL
		}
	}

	// D3: até aqui todo Reel era gravado como 'post' e ficava indistinguível de
	// um post de feed. A sessão passa a dizer 'reel'.
	out, err := s.attachPublishedMediaToEvent(ctx, input, live.SessionTypeReel, mediaID, permalink, thumbnail)
	if err != nil {
		logger.From(ctx, s.logger).Error("reel published but event creation failed",
			zap.String("store_id", input.StoreID),
			zap.String("media_id", mediaID), zap.Error(err))
		return live.CreateLiveOutput{}, &publishedUnboundError{MediaID: mediaID, Err: err}
	}

	logger.From(ctx, s.logger).Info("instagram reel created and event bound",
		zap.String("store_id", input.StoreID),
		zap.String("media_id", mediaID),
		zap.String("event_id", out.ID),
	)
	return out, nil
}

// CreateInstagramStoryEvent publishes a Story (photo or video) from a public URL
// and creates the bound story-commerce event (type='story', 24h window). Buyers
// reply to the Story via DM; ProcessInstagramMessage feeds those into the same
// cart pipeline. The transient media (input.ImageKey) is deleted after publish.
func (s *Service) CreateInstagramStoryEvent(ctx context.Context, input CreateInstagramPostInput, mediaURL string, isVideo bool) (live.CreateLiveOutput, error) {
	return s.publishWithIdempotency(ctx, input, "create_instagram_story",
		func() { s.deleteTransientImage(ctx, input.ImageKey) },
		func(prior priorPublishAttempt) (live.CreateLiveOutput, error) {
			return s.publishInstagramStoryEvent(ctx, input, mediaURL, isVideo, prior)
		},
	)
}

func (s *Service) publishInstagramStoryEvent(ctx context.Context, input CreateInstagramPostInput, mediaURL string, isVideo bool, prior priorPublishAttempt) (live.CreateLiveOutput, error) {
	provider, err := s.resolveInstagramSocialProvider(ctx, input.StoreID)
	if err != nil {
		return live.CreateLiveOutput{}, err
	}

	mediaID := prior.MediaID
	if mediaID == "" {
		mediaID, err = s.publishOrResume(ctx, provider, prior, true, func() (string, error) {
			return provider.PublishStory(ctx, mediaURL, isVideo)
		})
		if err != nil {
			logger.From(ctx, s.logger).Warn("failed to publish instagram story",
				zap.String("store_id", input.StoreID), zap.Error(err))
			s.deleteTransientImage(ctx, input.ImageKey)
			if errors.Is(err, providers.ErrPublishOutcomeUnknown) {
				// 19/08/2026: o timeout daqui virou 422 seco, a lojista
				// reenviou e nasceu o segundo story. Agora o desfecho
				// desconhecido fica retomável — o retry NÃO duplica.
				return live.CreateLiveOutput{}, errors.Join(
					httpx.DomainError(422, httpx.CodeIgPublishUnconfirmed, "Instagram did not confirm the publish — try again in a few seconds (the retry resumes this publish, it will not duplicate the story)"),
					err)
			}
			return live.CreateLiveOutput{}, httpx.ErrUnprocessable("failed to publish the story on Instagram")
		}

		// Instagram has fetched the media; remove the transient upload.
		s.deleteTransientImage(ctx, input.ImageKey)
	}

	// Best-effort metadata (a Story may not expose a public permalink).
	permalink, thumbnail := "", ""
	if details, dErr := provider.GetMediaDetails(ctx, mediaID); dErr == nil && details != nil {
		permalink = details.Permalink
		thumbnail = details.ThumbnailURL
		if thumbnail == "" {
			thumbnail = details.MediaURL
		}
	}

	// Stories expire after 24h — the event ends then too (effective status is
	// derived from ends_at, no background job). Computed here (not in the dedup
	// input) so a retried publish still hashes identically.
	endsAt := time.Now().Add(24 * time.Hour)

	// A janela de 24h do Story só vale quando ele cria o próprio evento; entrando
	// num evento existente, quem manda no prazo é o evento.
	storyInput := input
	storyInput.EndsAt = &endsAt
	out, err := s.attachPublishedMediaToEvent(ctx, storyInput, live.SessionTypeStory, mediaID, permalink, thumbnail)
	if err != nil {
		// O story JÁ está no ar; sem o vínculo, cada resposta de compradora
		// cai em "no matching story session" e morre. O erro tipado deixa o
		// retry religar ESTA mídia em vez de publicar uma cópia.
		logger.From(ctx, s.logger).Error("story published but event creation failed",
			zap.String("store_id", input.StoreID),
			zap.String("media_id", mediaID), zap.Error(err))
		return live.CreateLiveOutput{}, &publishedUnboundError{MediaID: mediaID, Err: err}
	}

	logger.From(ctx, s.logger).Info("instagram story created and event bound",
		zap.String("store_id", input.StoreID),
		zap.String("media_id", mediaID),
		zap.String("event_id", out.ID),
	)
	return out, nil
}

// =============================================================================
// ERP OPERATIONS
// =============================================================================

// SearchProducts searches for products in an ERP integration.
// It lists products, then enriches each with full details (stock, images)
// via GetProduct, and filters to only return active products with stock > 0.
func (s *Service) SearchProducts(ctx context.Context, input SearchProductsInput) (*SearchProductsOutput, error) {
	erpProvider, err := s.GetERPProvider(ctx, input.IntegrationID, input.StoreID)
	if err != nil {
		return nil, err
	}

	pageSize := input.PageSize
	if pageSize <= 0 {
		pageSize = 20
	}

	baseParams := providers.ListProductsParams{
		PageSize:   pageSize,
		ActiveOnly: true,
	}

	type searchResult struct {
		field    string
		products []providers.ERPProduct
		err      error
	}

	type searchJob struct {
		field  string
		params providers.ListProductsParams
	}

	jobs := []searchJob{
		{"name", func() providers.ListProductsParams { p := baseParams; p.Search = input.Search; return p }()},
		{"sku", func() providers.ListProductsParams { p := baseParams; p.SKU = input.Search; return p }()},
	}
	if isGTIN(input.Search) {
		p := baseParams
		p.GTIN = input.Search
		jobs = append(jobs, searchJob{"gtin", p})
	}

	results := make([]searchResult, len(jobs))
	var wg sync.WaitGroup
	for i, j := range jobs {
		wg.Add(1)
		go func(i int, field string, params providers.ListProductsParams) {
			defer wg.Done()
			r, err := erpProvider.ListProducts(ctx, params)
			if err != nil {
				results[i] = searchResult{field: field, err: err}
				return
			}
			results[i] = searchResult{field: field, products: r.Products}
		}(i, j.field, j.params)
	}
	wg.Wait()

	merged := make([]providers.ERPProduct, 0)
	seen := make(map[string]struct{})
	allErrored := true
	allRateLimited := true
	var firstErr error
	var firstNonRateLimitErr error
	priority := []string{"gtin", "sku", "name"}
	for _, prio := range priority {
		if len(merged) >= pageSize {
			break
		}
		for _, r := range results {
			if r.field != prio {
				continue
			}
			if r.err != nil {
				if firstErr == nil {
					firstErr = r.err
				}
				var rl *ratelimit.ErrRateLimited
				if !errors.As(r.err, &rl) {
					allRateLimited = false
					if firstNonRateLimitErr == nil {
						firstNonRateLimitErr = r.err
					}
				}
				logger.From(ctx, s.logger).Warn("ERP product search partial failure",
					zap.String("field", r.field),
					zap.String("integration_id", input.IntegrationID),
					zap.Bool("rate_limited", rl != nil),
					zap.Error(r.err),
				)
				continue
			}
			allErrored = false
			allRateLimited = false
			for _, p := range r.products {
				if _, ok := seen[p.ID]; ok {
					continue
				}
				seen[p.ID] = struct{}{}
				merged = append(merged, p)
				if len(merged) >= pageSize {
					break
				}
			}
		}
	}

	if allErrored {
		// Estrangulado na LISTAGEM: diz que está estrangulado.
		//
		// Aqui se devolvia lista vazia com sucesso, para fugir do toast de erro
		// interno. A troca saiu cara: a tela mostrava "nada encontrado", a
		// lojista concluía que o produto não existia e clicava de novo — em
		// 16/08 foram ~20 buscas em rajada no meio da live, cada uma somando
		// pressão no limitador que já estava recusando.
		//
		// "Não achei" e "não posso olhar agora" pedem ações opostas do lojista.
		// O caminho do enriquecimento logo abaixo já devolve ERP_THROTTLED com
		// o texto certo; a listagem tinha ficado para trás.
		if allRateLimited {
			s.handleProviderError(ctx, input.IntegrationID, "search_products", firstErr)
			logger.From(ctx, s.logger).Warn("ERP product search throttled",
				zap.String("integration_id", input.IntegrationID),
			)
			return nil, httpx.DomainError(503, httpx.CodeErpThrottled,
				"O ERP está limitando as consultas neste momento. Aguarde alguns segundos e busque de novo.")
		}
		// At least one job failed for a non-rate-limit reason — escalate.
		s.handleProviderError(ctx, input.IntegrationID, "search_products", firstNonRateLimitErr)
		return nil, fmt.Errorf("searching products: %w", firstNonRateLimitErr)
	}

	if len(merged) == 0 {
		return nil, httpx.ErrNotFound("Produto não encontrado no ERP")
	}

	result := &providers.ProductListResult{
		Products: merged,
		HasMore:  false,
	}

	// Enrich each product with full details (stock, image, description)
	// The list endpoint doesn't return stock or images — GetProduct does.
	//
	// Parents with variations (Tiny tipo=V) carry no stock themselves; the real
	// stock is on each child. We aggregate it for the parent so the
	// "out of stock" filter doesn't accidentally hide products that have
	// inventory on at least one variation, and we surface the variations so the
	// front-end can let the user pick a SKU.
	var products []ERPProductResponse
	foundButNoStock := false
	// A listagem ACHOU produtos. Guardamos isso porque, se o enriquecimento
	// abaixo derrubar todos, "não encontrado no ERP" seria mentira — o produto
	// existe, o Tiny é que não deixou ler o detalhe.
	enrichThrottled := false
	for _, listed := range result.Products {
		detailed, err := erpProvider.GetProduct(ctx, listed.ID)
		if err != nil {
			var rl *ratelimit.ErrRateLimited
			if errors.As(err, &rl) {
				// Estrangulado: PARA. Insistir nos que faltam só empilha 429 a
				// 1 req/s — foi o que fez a busca levar 15-20s e o front
				// desistir com "A busca demorou demais". Devolver rápido o que
				// já temos (ou um erro honesto) vale mais que uma lista
				// completa que ninguém espera.
				enrichThrottled = true
				logger.From(ctx, s.logger).Warn("ERP throttled while loading product details, stopping enrichment",
					zap.String("product_id", listed.ID),
					zap.Int("enriched_so_far", len(products)),
					zap.Int("listed", len(result.Products)),
				)
				break
			}
			logger.From(ctx, s.logger).Warn("failed to get product details, skipping",
				zap.String("product_id", listed.ID),
				zap.Error(err),
			)
			continue
		}

		isParent := detailed.IsParent && len(detailed.Variants) > 0
		effectiveStock := detailed.Stock
		var variantsResp []ERPVariantResponse
		if isParent {
			// NOTE: NOT calling enrichVariantsFromIndividualGets here on
			// purpose — that helper does N extra Tiny GetProducts (one per
			// variation) just to pull imageUrl + per-variant shipping for the
			// picker preview. With Tiny's 1 req/s rate limit and products
			// carrying 9+ variations, the search request was taking ~15-20s
			// and the front was timing out ("A busca demorou demais").
			// The per-variation GetProduct happens later when the merchant
			// actually imports the product, where the latency is acceptable.
			// At search time we settle for whatever came in the parent's
			// `variacoes[]` (id/sku/stock/attributes) — enough to render the
			// picker.

			effectiveStock = 0
			variantsResp = make([]ERPVariantResponse, len(detailed.Variants))
			for i, v := range detailed.Variants {
				effectiveStock += v.Stock
				variantsResp[i] = ERPVariantResponse{
					ID:         v.ID,
					SKU:        v.SKU,
					GTIN:       v.GTIN,
					Name:       v.Name,
					Price:      v.Price,
					Stock:      v.Stock,
					Active:     v.Active,
					ImageURL:   v.ImageURL,
					Shipping:   shippingPreviewFromERP(v.Shipping, v.WeightGramsHint),
					Attributes: v.Attributes,
				}
			}
		}

		if effectiveStock <= 0 {
			foundButNoStock = true
			continue
		}

		products = append(products, ERPProductResponse{
			ID:          detailed.ID,
			SKU:         detailed.SKU,
			GTIN:        detailed.GTIN,
			Name:        detailed.Name,
			Description: detailed.Description,
			Price:       detailed.Price,
			Stock:       effectiveStock,
			ImageURL:    detailed.ImageURL,
			ImageURLs:   detailed.ImageURLs,
			Active:      detailed.Active,
			Shipping:    shippingPreviewFromERP(detailed.Shipping, detailed.WeightGramsHint),
			IsParent:    isParent,
			Variants:    variantsResp,
		})
	}

	if len(products) == 0 {
		if foundButNoStock {
			return nil, httpx.ErrUnprocessable("Produto encontrado, mas sem estoque disponível no momento")
		}
		// O produto EXISTE — a listagem o devolveu — e o ERP recusou entregar o
		// detalhe. Dizer "não encontrado no ERP" aqui é acusar o lojista de
		// procurar o que não existe, e foi o que aconteceu na prática: ele
		// buscava um produto que estava lá e recebia que não estava.
		if enrichThrottled {
			return nil, httpx.DomainError(503, httpx.CodeErpThrottled,
				"O ERP está limitando as consultas neste momento. Aguarde alguns segundos e busque de novo.")
		}
		return nil, httpx.ErrNotFound("Produto não encontrado no ERP")
	}

	// Flag products already in the store's catalog so the FE can badge them
	// "já cadastrado" and block re-importing (single batch query — no
	// pagination gaps). Best-effort: a failure here just leaves the flags off.
	if s.productSyncer != nil {
		externalIDs := make([]string, 0, len(products))
		for _, p := range products {
			externalIDs = append(externalIDs, p.ID)
		}
		registered, err := s.productSyncer.FilterRegisteredExternalIDs(ctx, input.StoreID, string(erpProvider.Name()), externalIDs)
		if err != nil {
			logger.From(ctx, s.logger).Warn("failed to check already-imported products",
				zap.String("integration_id", input.IntegrationID),
				zap.Error(err),
			)
		} else if len(registered) > 0 {
			registeredSet := make(map[string]struct{}, len(registered))
			for _, id := range registered {
				registeredSet[id] = struct{}{}
			}
			for i := range products {
				if _, ok := registeredSet[products[i].ID]; ok {
					products[i].AlreadyImported = true
				}
			}
		}
	}

	return &SearchProductsOutput{
		Products:   products,
		TotalCount: len(products),
		HasMore:    result.HasMore,
	}, nil
}

// inheritShippingFromParent fills detailed.Shipping with the parent product's
// shipping profile when the ERP returned a variation child without its own
// dimensions. Common Tiny setup: merchant fills `dimensoes` only on the parent
// and every variation reuses the same packaging — without this, syncing a
// variation would leave it un-shippable.
//
// No-op when the product is not a variation, already has its own shipping,
// or when fetching the parent fails (best-effort).
func (s *Service) inheritShippingFromParent(ctx context.Context, erpProvider providers.ERPProvider, detailed *providers.ERPProduct) {
	if detailed == nil {
		return
	}
	if detailed.Shipping != nil {
		logger.From(ctx, s.logger).Debug("variant already has its own shipping, no parent lookup needed",
			zap.String("tiny_id", detailed.ID))
		return
	}
	if detailed.ParentExternalID == "" {
		logger.From(ctx, s.logger).Debug("product has no parent, cannot inherit shipping",
			zap.String("tiny_id", detailed.ID),
			zap.Bool("is_parent", detailed.IsParent))
		return
	}
	parent, err := erpProvider.GetProduct(ctx, detailed.ParentExternalID)
	if err != nil {
		logger.From(ctx, s.logger).Warn("failed to fetch parent for shipping inheritance",
			zap.String("variant_id", detailed.ID),
			zap.String("parent_id", detailed.ParentExternalID),
			zap.Error(err))
		return
	}
	if parent == nil || parent.Shipping == nil {
		logger.From(ctx, s.logger).Info("parent has no shipping either — variation will land without dimensions",
			zap.String("variant_id", detailed.ID),
			zap.String("parent_id", detailed.ParentExternalID),
			zap.Bool("parent_returned", parent != nil))
		return
	}
	detailed.Shipping = parent.Shipping
	logger.From(ctx, s.logger).Info("inherited shipping from parent",
		zap.String("variant_id", detailed.ID),
		zap.String("parent_id", detailed.ParentExternalID))
}

// enrichVariantsFromIndividualGets fetches GET /produtos/{idVariacao} for each
// variation in parallel and merges the response back. Tiny's
// VariacaoProdutoResponseModel inside the parent's `variacoes[]` does NOT carry
// imageUrl, dimensoes or per-variant flat dimensions — those only exist on the
// individual GET. Without this hop we'd discard per-variant shipping that the
// merchant actually cadastrou no ERP.
//
// Bounded concurrency keeps us under the per-account rate limit (60 req/min on
// Tiny basic). Failures are silent — variant keeps whatever it had.
func (s *Service) enrichVariantsFromIndividualGets(ctx context.Context, erpProvider providers.ERPProvider, parent *providers.ERPProduct) {
	if parent == nil || len(parent.Variants) == 0 {
		return
	}
	const enrichConcurrency = 5
	sem := make(chan struct{}, enrichConcurrency)
	var wg sync.WaitGroup
	for i := range parent.Variants {
		idx := i
		childID := parent.Variants[idx].ID
		if childID == "" {
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			child, err := erpProvider.GetProduct(ctx, childID)
			if err != nil || child == nil {
				return
			}
			// O saldo da variação também vem daqui.
			//
			// O payload do pai traz `estoque.quantidade` de cada variação, que é
			// sempre o saldo FÍSICO — a resposta do produto não tem a quebra de
			// reservado/disponível. O GET individual acima já passou pela regra
			// da loja, então é ele quem sabe qual saldo vale, e esse número
			// chega no banco por productgroup/adapter.
			//
			// De graça: a chamada já estava sendo feita para imagem e frete.
			parent.Variants[idx].Stock = child.Stock

			if child.ImageURL != "" {
				parent.Variants[idx].ImageURL = child.ImageURL
			}
			if child.Shipping != nil {
				parent.Variants[idx].Shipping = child.Shipping
			}
			if child.WeightGramsHint > 0 {
				parent.Variants[idx].WeightGramsHint = child.WeightGramsHint
			}
		}()
	}
	wg.Wait()
}

// applyStoreDefaultDimensions completes detailed.Shipping (and each variant's)
// using the merchant-configured store defaults when the ERP returned weight
// without dimensions, or vice-versa. No-op when the store has no defaults
// configured or the product is already shippable.
//
// Precedence (per product/variant):
//  1. Use what the ERP gave us as-is when complete.
//  2. Combine ERP weight (WeightGramsHint) with store default H/W/L when ERP
//     returned weight only.
//  3. When ERP returned no weight AND store has both default weight and H/W/L,
//     build a fully synthetic profile.
//  4. Otherwise leave Shipping nil (current behavior — merchant edits later).
func (s *Service) applyStoreDefaultDimensions(ctx context.Context, storeID string, detailed *providers.ERPProduct) {
	if detailed == nil {
		return
	}
	defaults, err := s.repo.GetStoreShippingDefaults(ctx, storeID)
	if err != nil {
		logger.From(ctx, s.logger).Warn("failed to load store shipping defaults",
			zap.String("store_id", storeID), zap.Error(err))
		return
	}
	if !defaults.IsUsableForDimensionFallback() {
		return
	}

	completeFromDefaults := func(p *providers.ERPProduct) {
		if p.Shipping != nil {
			return
		}
		weight := p.WeightGramsHint
		if weight <= 0 {
			weight = defaults.WeightGrams
		}
		if weight <= 0 {
			return
		}
		format := defaults.PackageFormat
		if format == "" {
			format = "box"
		}
		p.Shipping = &providers.ERPShippingProfile{
			WeightGrams:   weight,
			HeightCm:      defaults.HeightCm,
			WidthCm:       defaults.WidthCm,
			LengthCm:      defaults.LengthCm,
			PackageFormat: format,
		}
		logger.From(ctx, s.logger).Info("completed shipping with store defaults",
			zap.String("erp_id", p.ID),
			zap.Int("weight_g", weight),
			zap.Int("h_cm", defaults.HeightCm),
			zap.Int("w_cm", defaults.WidthCm),
			zap.Int("l_cm", defaults.LengthCm))
	}

	completeFromDefaults(detailed)
	for i := range detailed.Variants {
		completeFromDefaults(&detailed.Variants[i])
	}
}

// ImportERPProduct imports a product from the ERP into the LiveCart catalog.
// For products with variations, it creates a product_group + N variants in one
// transaction (filtered by VariantIDs when present). For simple products, it
// creates a single product.
func (s *Service) ImportERPProduct(ctx context.Context, input ImportERPProductInput) (*ImportERPProductOutput, error) {
	if s.productSyncer == nil {
		return nil, httpx.ErrUnprocessable("product syncer not configured")
	}

	erpProvider, err := s.GetERPProvider(ctx, input.IntegrationID, input.StoreID)
	if err != nil {
		return nil, err
	}
	integration, err := s.repo.GetByID(ctx, input.IntegrationID, input.StoreID)
	if err != nil {
		return nil, err
	}

	detailed, err := erpProvider.GetProduct(ctx, input.TinyProductID)
	if err != nil {
		s.handleProviderError(ctx, input.IntegrationID, "import_get_product", err)
		return nil, fmt.Errorf("fetching product from ERP: %w", err)
	}

	// === Simple product (no variations) ===
	if !detailed.IsParent || len(detailed.Variants) == 0 {
		if len(input.VariantIDs) > 0 {
			return nil, httpx.ErrUnprocessable("variantIds informado mas o produto não possui variações")
		}
		exists, err := s.productSyncer.HasProduct(ctx, input.StoreID, detailed.ID, integration.Provider)
		if err != nil {
			return nil, fmt.Errorf("checking product existence: %w", err)
		}
		if exists {
			return nil, httpx.ErrConflict("produto já importado neste catálogo")
		}
		// Same shipping completion chain we run for variants: parent inheritance
		// (no-op for simples) and store-default fallback. Without these the
		// product would land with an empty shipping profile when Tiny returns
		// only weight / partial dimensions.
		s.inheritShippingFromParent(ctx, erpProvider, detailed)
		s.applyStoreDefaultDimensions(ctx, input.StoreID, detailed)
		productID, err := s.productSyncer.ImportProduct(ctx, input.StoreID, integration.Provider, *detailed)
		if err != nil {
			return nil, fmt.Errorf("importing simple product: %w", err)
		}
		return &ImportERPProductOutput{
			ProductID: productID,
			IsParent:  false,
			Imported: []ImportedERPVariantSummary{{
				ExternalID: detailed.ID,
				SKU:        detailed.SKU,
			}},
		}, nil
	}

	// === Parent with variations ===
	if s.productGroupSyncer == nil {
		return nil, httpx.ErrUnprocessable("product group syncer not configured")
	}

	// Filter variants if a subset was requested.
	if len(input.VariantIDs) > 0 {
		want := make(map[string]struct{}, len(input.VariantIDs))
		for _, id := range input.VariantIDs {
			want[id] = struct{}{}
		}
		filtered := make([]providers.ERPProduct, 0, len(input.VariantIDs))
		for _, v := range detailed.Variants {
			if _, ok := want[v.ID]; ok {
				filtered = append(filtered, v)
			}
		}
		if len(filtered) == 0 {
			return nil, httpx.ErrUnprocessable("nenhuma das variantIds informadas existe no produto Tiny")
		}
		if len(filtered) != len(input.VariantIDs) {
			return nil, httpx.ErrUnprocessable("uma ou mais variantIds informadas não existem no produto Tiny")
		}
		detailed.Variants = filtered
	}

	// Tiny doesn't include imageUrl, dimensoes or flat dimensions inside
	// variacoes[] of a parent response — fetch each child individually so we
	// pick up per-variant images AND per-variant shipping the merchant
	// cadastrou no ERP.
	s.enrichVariantsFromIndividualGets(ctx, erpProvider, detailed)

	// Fall back to merchant-configured store defaults for any variant whose
	// shipping is still incomplete after the Tiny payload + parent inheritance.
	s.applyStoreDefaultDimensions(ctx, input.StoreID, detailed)

	groupID, importedIDs, err := s.productGroupSyncer.ImportFromERP(ctx, input.StoreID, integration.Provider, *detailed)
	if err != nil {
		return nil, fmt.Errorf("importing product group: %w", err)
	}

	imported := make([]ImportedERPVariantSummary, 0, len(importedIDs))
	for _, extID := range importedIDs {
		for _, v := range detailed.Variants {
			if v.ID == extID {
				imported = append(imported, ImportedERPVariantSummary{
					ExternalID: v.ID,
					SKU:        v.SKU,
					Attributes: v.Attributes,
				})
				break
			}
		}
	}

	logger.From(ctx, s.logger).Info("ERP product imported into catalog",
		zap.String("integration_id", input.IntegrationID),
		zap.String("tiny_product_id", input.TinyProductID),
		zap.String("group_id", groupID),
		zap.Int("variants_imported", len(imported)),
	)

	return &ImportERPProductOutput{
		GroupID:  groupID,
		IsParent: true,
		Imported: imported,
	}, nil
}

// SyncProductManual fetches the latest product data from the ERP and updates the local product.
func (s *Service) SyncProductManual(ctx context.Context, input SyncProductInput) (*SyncProductOutput, error) {
	if s.productSyncer == nil {
		return nil, httpx.ErrUnprocessable("product syncer not configured")
	}

	// Get the product from LiveCart to find its external ID
	externalID, externalSource, err := s.productSyncer.GetProduct(ctx, input.StoreID, input.ProductID)
	if err != nil {
		return nil, err
	}

	if externalID == "" {
		return nil, httpx.ErrUnprocessable("produto não possui ID externo vinculado a um ERP")
	}

	// Verify integration belongs to this store and is an ERP
	erpProvider, err := s.GetERPProvider(ctx, input.IntegrationID, input.StoreID)
	if err != nil {
		return nil, err
	}

	// Verify the integration provider matches the product's external source
	integration, err := s.repo.GetByID(ctx, input.IntegrationID, input.StoreID)
	if err != nil {
		return nil, err
	}
	if integration.Provider != externalSource {
		return nil, httpx.ErrUnprocessable("integração não corresponde à origem do produto")
	}

	// Fetch latest product data from the ERP
	detailed, err := erpProvider.GetProduct(ctx, externalID)
	if err != nil {
		s.handleProviderError(ctx, input.IntegrationID, "manual_sync_product", err)
		return nil, fmt.Errorf("fetching product from ERP: %w", err)
	}

	// If this is a variation child without its own dimensions, inherit from
	// the parent — common in Tiny when the merchant only filled the parent's
	// dimensoes and let every variation use the same packaging.
	s.inheritShippingFromParent(ctx, erpProvider, detailed)
	// Last resort: complete with merchant-configured store defaults when ERP
	// only carries weight (or nothing).
	s.applyStoreDefaultDimensions(ctx, input.StoreID, detailed)

	// Update the local product. Manual sync always refreshes stock and pulls
	// dimensions if the ERP returned them (detailed.Shipping non-nil).
	if err := s.productSyncer.SyncProduct(ctx, input.StoreID, externalSource, *detailed, false); err != nil {
		return nil, fmt.Errorf("syncing product: %w", err)
	}

	logger.From(ctx, s.logger).Info("product synced manually",
		zap.String("integration_id", input.IntegrationID),
		zap.String("product_id", input.ProductID),
		zap.String("external_id", externalID),
		zap.String("store_id", input.StoreID),
	)

	return &SyncProductOutput{
		ProductID:  input.ProductID,
		ExternalID: externalID,
		Name:       detailed.Name,
		Price:      detailed.Price,
		Stock:      detailed.Stock,
		ImageURL:   detailed.ImageURL,
		Active:     detailed.Active,
	}, nil
}

const productWebhookMaxRetries = 3

// Por que o saldo do ERP entrou — ou não.
//
// Um booleano não bastava. "Não apliquei" agrupava dois casos com destinos
// opostos: o produto que o lojista nunca importou (repetir não muda nada, o ERP
// notifica sobre o catálogo inteiro dele) e a leitura que venceu porque um
// movimento nosso aterrissou depois dela (só uma leitura NOVA resolve).
//
// Tratar os dois como iguais custava caro: a venda no e-commerce era descartada
// em silêncio e ninguém ia buscá-la de novo. O comentário no ponto do descarte
// apostava no "próximo webhook ou na reconciliação", mas se aquela venda foi o
// último movimento do lojista, próximo webhook não existe — e a reconciliação
// só reporta divergência, não conserta.
type stockMirrorOutcome int

const (
	stockMirrorApplied  stockMirrorOutcome = iota // o saldo do ERP entrou
	stockMirrorNoTarget                           // produto não importado aqui
	stockMirrorStale                              // leitura vencida: reler resolve
)

// ProcessProductWebhook checks if the product exists in LiveCart, then fetches
// full details from the ERP and syncs locally. Ignores unknown products.
// Retries on transient failures to avoid losing sync events.
//
// The boolean return reports whether the LOCAL STOCK counter was actually
// overwritten by this sync ("stock applied"). The waitlist backstop keys off
// it: promoting after a sync that skipped stock (guard armed) or failed would
// act on a stale/poisoned counter.
func (s *Service) ProcessProductWebhook(ctx context.Context, storeID, provider, externalProductID string) (bool, error) {
	if s.productSyncer == nil {
		logger.From(ctx, s.logger).Warn("product syncer not configured, skipping product webhook")
		return false, nil
	}

	// Resolve integration from store_id + provider
	integration, err := s.repo.GetActiveByProvider(ctx, storeID, "erp", provider)
	if err != nil {
		return false, fmt.Errorf("no active ERP integration found for store %s provider %s: %w", storeID, provider, err)
	}

	// Check if product exists in LiveCart before calling the ERP API
	exists, err := s.productSyncer.HasProduct(ctx, integration.StoreID, externalProductID, integration.Provider)
	if err != nil {
		return false, fmt.Errorf("checking product existence: %w", err)
	}
	if !exists {
		logger.From(ctx, s.logger).Debug("product not registered in livecart, ignoring webhook",
			zap.String("store_id", storeID),
			zap.String("integration_id", integration.ID),
			zap.String("external_product_id", externalProductID),
		)
		return false, nil
	}

	var lastErr error
	for attempt := 0; attempt <= productWebhookMaxRetries; attempt++ {
		if attempt > 0 {
			backoff := time.Duration(1<<uint(attempt-1)) * time.Second
			logger.From(ctx, s.logger).Warn("retrying product webhook processing",
				zap.String("store_id", storeID),
				zap.String("integration_id", integration.ID),
				zap.String("product_id", externalProductID),
				zap.Int("attempt", attempt+1),
				zap.Duration("backoff", backoff),
			)
			select {
			case <-ctx.Done():
				return false, ctx.Err()
			case <-time.After(backoff):
			}
		}

		outcome, syncErr := s.processProductSync(ctx, integration, externalProductID)
		if syncErr == nil {
			// Leitura vencida é o único desfecho que pede outra rodada: a
			// próxima passada faz um GetProduct NOVO, e é a leitura nova — não
			// um palpite sobre a velha — que corrige o contador.
			if outcome != stockMirrorStale {
				return outcome == stockMirrorApplied, nil
			}
			lastErr = nil
			continue
		}
		lastErr = syncErr
	}

	// Esgotou as tentativas ainda vencido. Só acontece com movimento nosso
	// constante no mesmo SKU durante a janela inteira, e o custo é real: aquela
	// venda do lojista em outro canal ainda não está no nosso contador. Alto de
	// propósito — é o gancho para a reconciliação e para o alerta.
	if lastErr == nil {
		logger.From(ctx, s.logger).Error("ERP balance never landed: stale on every attempt",
			zap.String("store_id", storeID),
			zap.String("integration_id", integration.ID),
			zap.String("product_id", externalProductID),
			zap.Int("attempts", productWebhookMaxRetries+1),
		)
		return false, nil
	}

	logger.From(ctx, s.logger).Error("product webhook processing failed after retries",
		zap.String("store_id", storeID),
		zap.String("integration_id", integration.ID),
		zap.String("product_id", externalProductID),
		zap.Int("max_retries", productWebhookMaxRetries),
		zap.Error(lastErr),
	)

	return false, lastErr
}

func (s *Service) processProductSync(ctx context.Context, integration *IntegrationRow, externalProductID string) (stockMirrorOutcome, error) {
	provider, err := s.createProviderFromRow(ctx, integration)
	if err != nil {
		return stockMirrorNoTarget, fmt.Errorf("creating provider: %w", err)
	}

	erpProvider, ok := provider.(providers.ERPProvider)
	if !ok {
		return stockMirrorNoTarget, fmt.Errorf("integration %s is not an ERP provider", integration.ID)
	}

	// O seq ANTES da consulta ao ERP. E o comprovante da leitura: se mudar ate a
	// hora de aplicar, um movimento nosso entrou no meio e este saldo descreve
	// um estado que ja nao existe.
	//
	// Ler depois nao serviria — a corrida mora exatamente entre a consulta e a
	// escrita, que e onde a reserva de outro comprador cabe.
	localProductID, seenSeq, seqErr := s.repo.ProductSeqByExternalID(ctx, integration.StoreID, integration.Provider, externalProductID)

	detailed, err := erpProvider.GetProduct(ctx, externalProductID)
	if err != nil {
		s.handleProviderError(ctx, integration.ID, "webhook_get_product", err)
		return stockMirrorNoTarget, fmt.Errorf("fetching product from ERP: %w", err)
	}

	// Variant-aware branch: if the ERP returned a parent product with children
	// (e.g. Tiny tipo=V), enrich each variant from its individual GET (where
	// per-variant shipping actually lives) before delegating the whole tree
	// to the productgroup syncer.
	if detailed.IsParent && len(detailed.Variants) > 0 && s.productGroupSyncer != nil {
		s.enrichVariantsFromIndividualGets(ctx, erpProvider, detailed)
		s.applyStoreDefaultDimensions(ctx, integration.StoreID, detailed)
		if err := s.productGroupSyncer.SyncFromERP(ctx, integration.StoreID, integration.Provider, *detailed); err != nil {
			return stockMirrorNoTarget, fmt.Errorf("syncing product group: %w", err)
		}
		return stockMirrorApplied, nil
	}

	// Variation child without its own dimensions inherits from the parent,
	// then falls back to merchant-configured store defaults.
	s.inheritShippingFromParent(ctx, erpProvider, detailed)
	s.applyStoreDefaultDimensions(ctx, integration.StoreID, detailed)

	// Guard do overwrite de estoque. Enquanto houver reserva ativa numa live
	// OU finalização ERP em voo para o produto, o webhook de estoque do Tiny
	// não pode SUBIR o contador local (a reversão de reservas na finalização
	// infla o saldo do Tiny por segundos → oferta falsa → promoção fantasma da
	// waitlist). Mas REDUÇÕES do lojista no Tiny durante a live são legítimas e
	// devem refletir — então na janela do guard usamos "downgrade-only": aplica
	// só quando o valor do ERP é menor que o local (direção segura, nunca
	// causa promoção fantasma). Fora da janela, sync normal. Fail-safe: em erro
	// de DB, preserva o local inteiro.
	// O ESTOQUE nao passa pelo sync generico: e aplicado a parte, com trava
	// otimista. O `true` no SyncProduct abaixo diz "cuide de nome, preco e
	// dimensoes; do saldo cuido eu".
	//
	// O guard de reserva ativa deixou de existir aqui. Ele nascera para decidir
	// se o saldo do ERP podia ser aplicado, e cada versao errava de um jeito:
	// suprimir enquanto houvesse reserva cegava o LiveCart para os outros canais
	// do lojista pela live inteira; downgrade-only deixava passar o eco do nosso
	// proprio movimento. A trava responde a mesma pergunta sem heuristica — ou a
	// leitura e do presente, ou nao e.
	outcome := stockMirrorNoTarget
	switch {
	case seqErr != nil:
		logger.From(ctx, s.logger).Warn("could not read product seq; skipping stock this round",
			zap.String("external_product_id", externalProductID), zap.Error(seqErr))
		outcome = stockMirrorStale
	case localProductID == "":
		// Produto que o lojista nao importou. O ERP notifica sobre o catalogo
		// inteiro dele; nos so espelhamos o que existe aqui.
	default:
		// O saldo do ERP é verdadeiro para o ERP e mentiroso para nós: ele não
		// desconta o que a live já prometeu e cuja reserva ainda não confirmou.
		// Gravá-lo cru reabastece o portão com estoque que já tem dono — foi
		// assim que 25 admissões saíram de 20 unidades em 26/08, com o Tiny
		// terminando em −13. A regra conservadora vive em erpwrite.Admissivel.
		saldoParaOPortao := detailed.Stock
		if emVoo, voErr := s.repo.SumInFlightOutMovements(ctx, externalProductID); voErr != nil {
			logger.From(ctx, s.logger).Warn("could not read in-flight movements; mirroring the raw ERP balance",
				zap.String("external_product_id", externalProductID), zap.Error(voErr))
		} else if emVoo > 0 {
			saldoParaOPortao = erpwrite.Admissivel(detailed.Stock, emVoo)
			logger.From(ctx, s.logger).Info("stock mirror discounted in-flight reservations",
				zap.String("external_product_id", externalProductID),
				zap.Int("erp_stock", detailed.Stock),
				zap.Int("in_flight", emVoo),
				zap.Int("admissible", saldoParaOPortao))
		}

		applied, applyErr := s.repo.ApplyERPStockMirror(ctx, localProductID, saldoParaOPortao, seenSeq)
		switch {
		case applyErr != nil:
			logger.From(ctx, s.logger).Warn("failed to apply ERP stock mirror",
				zap.String("external_product_id", externalProductID), zap.Error(applyErr))
			outcome = stockMirrorStale
		case !applied:
			// Um movimento nosso foi confirmado entre a leitura e agora. Aquele
			// saldo e passado, e nao da para saber quanto dele ja estava velho —
			// so uma leitura NOVA responde. Quem chama repete a rodada inteira,
			// com um GetProduct novo; descartar sem repetir era perder a venda do
			// lojista em outro canal, em silencio.
			logger.From(ctx, s.logger).Info("ERP balance stale: one of our movements landed after the read; re-reading",
				zap.String("external_product_id", externalProductID),
				zap.Int64("seen_seq", seenSeq), zap.Int("erp_stock", detailed.Stock))
			outcome = stockMirrorStale
		default:
			outcome = stockMirrorApplied
		}
	}

	if err := s.productSyncer.SyncProduct(ctx, integration.StoreID, integration.Provider, *detailed, true); err != nil {
		return stockMirrorNoTarget, fmt.Errorf("syncing product: %w", err)
	}

	// O backstop de waitlist só deve rodar quando o estoque pôde AUMENTAR (sync
	// normal). Na janela do guard nunca subimos o local, então não promove;
	// uma redução não libera unidade para ninguém.
	logger.From(ctx, s.logger).Info("product synced from webhook",
		zap.String("integration_id", integration.ID),
		zap.String("external_product_id", externalProductID),
		zap.String("store_id", integration.StoreID),
		zap.Bool("stock_applied", outcome == stockMirrorApplied),
		zap.Int("erp_stock", detailed.Stock),
	)

	// Backstop de waitlist so quando o saldo foi de fato aplicado: so ai uma
	// unidade pode ter aparecido para promover.
	return outcome, nil
}

// isGTIN checks if a string looks like a GTIN/barcode (8+ digits).
func isGTIN(s string) bool {
	if len(s) < 8 {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// =============================================================================
// PAYMENT OPERATIONS
// =============================================================================
//
// These stateless fast-paths were moved to internal/payment (strangler-fig
// B1b); the methods below are now thin delegations that preserve the public
// signatures so the existing call sites keep working until B1e repoints them at
// payment.Service directly. The provider-specific Pagar.me webhook helpers
// (GetPagarmeWebhookStatus / Test* / RunPagarmeWebhookLiveTest) stay here.

// GetPagarmeWebhookStatus checks whether the merchant has registered our
// webhook URL on the Pagar.me dashboard by inspecting recent delivery history
// (GET /hooks). Pagar.me v5 has no public API to list webhook subscriptions,
// so we infer "configured" from at least one matching delivery in the recent
// window, and surface health via the last attempt's response_status.
func (s *Service) GetPagarmeWebhookStatus(ctx context.Context, integrationID, storeID string) (*PagarmeWebhookStatusOutput, error) {
	paymentProvider, err := s.GetPaymentProvider(ctx, integrationID, storeID)
	if err != nil {
		return nil, err
	}
	pagarme, ok := paymentProvider.(*payment.Pagarme)
	if !ok {
		return nil, httpx.ErrUnprocessable("integration is not Pagar.me")
	}

	urls := buildProviderURLs("pagarme", storeID)
	expectedURL := urls.WebhookURL

	deliveries, err := pagarme.ListRecentHookDeliveries(ctx, 30)
	if err != nil {
		s.handleProviderError(ctx, integrationID, "list_hook_deliveries", err)
		return nil, fmt.Errorf("listing hook deliveries: %w", err)
	}

	out := &PagarmeWebhookStatusOutput{
		ExpectedURL: expectedURL,
		Configured:  false,
	}
	// Pagar.me returns deliveries newest-first. Walk once: count matches,
	// stamp the most recent attempt, and capture the last response_status
	// so the UI can warn when the URL is configured but failing (e.g. 5xx
	// during a deploy, or 401 because basic auth drifted).
	for _, d := range deliveries {
		if d.URL != expectedURL {
			continue
		}
		out.Configured = true
		out.MatchCount++
		if out.LastDeliveryAt.IsZero() || d.LastAttempt.After(out.LastDeliveryAt) {
			out.LastDeliveryAt = d.LastAttempt
			out.LastDeliveryStatus = d.Status
			out.LastResponseStatus = d.ResponseStatus
			out.LastEvent = d.Event
		}
	}
	return out, nil
}

// pagarmeWebhookTestType marks the synthetic event the loopback self-test
// POSTs to our own webhook URL. The receiver (HandlePagarme) recognizes it and
// returns 200 as a pure no-op — no audit row, no ping stamp, no cart change —
// so the delivery-history probe stays honest about real Pagar.me deliveries.
const pagarmeWebhookTestType = "livecart.webhook_test"

// TestPagarmeWebhookEndpoint runs a loopback self-test: it POSTs a synthetic
// event to the store's OWN public Pagar.me webhook URL (reconstructing the
// stored Basic Auth) and reports whether our endpoint is reachable and healthy.
// This lets the merchant validate the webhook right after configuring it,
// instead of waiting for a real customer payment. It never touches Pagar.me.
func (s *Service) TestPagarmeWebhookEndpoint(ctx context.Context, integrationID, storeID string) (*PagarmeWebhookTestOutput, error) {
	// Load the same integration + credentials the inbound path validates
	// against, so the test exercises the real Basic Auth check.
	row, err := s.repo.GetByProvider(ctx, storeID, string(providers.ProviderTypePayment), string(providers.ProviderPagarme))
	if err != nil {
		return nil, httpx.ErrNotFound("integração Pagar.me não encontrada")
	}
	creds, err := s.decryptCredentials(row.Credentials)
	if err != nil {
		return nil, fmt.Errorf("decrypting credentials: %w", err)
	}
	webhookUser, _ := creds.Extra["webhook_username"].(string)
	webhookPass, _ := creds.Extra["webhook_password"].(string)
	authConfigured := webhookUser != "" || webhookPass != ""

	url := buildProviderURLs("pagarme", storeID).WebhookURL
	if url == "" {
		return nil, httpx.ErrUnprocessable("URL de webhook indisponível — verifique a configuração do servidor")
	}

	out := &PagarmeWebhookTestOutput{URL: url, AuthConfigured: authConfigured}

	// Synthetic payload shaped like a Pagar.me v5 event so it flows through the
	// exact same parsing as a real one. The nonce only aids log correlation —
	// the caller observes the HTTP response directly, so no async matching is
	// needed. The URL is built from our own config + a UUID storeId, never from
	// user input, so there is no SSRF surface.
	nonce := uuid.New().String()
	payload := map[string]any{
		"id":         "livecart_test_" + nonce,
		"type":       pagarmeWebhookTestType,
		"created_at": time.Now().UTC().Format(time.RFC3339),
		"data":       map[string]any{"id": "", "nonce": nonce},
	}
	bodyBytes, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshaling test payload: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, fmt.Errorf("building test request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if authConfigured {
		basic := base64.StdEncoding.EncodeToString([]byte(webhookUser + ":" + webhookPass))
		req.Header.Set("Authorization", "Basic "+basic)
	}

	client := &http.Client{Timeout: 10 * time.Second}
	start := time.Now()
	resp, err := client.Do(req)
	out.LatencyMs = time.Since(start).Milliseconds()
	if err != nil {
		// Transport-level failure: DNS, TLS, connection refused, timeout — the
		// endpoint is not reachable over the public internet.
		out.Reachable = false
		out.Healthy = false
		out.Message = "Não foi possível alcançar o endpoint (timeout ou falha de conexão). Verifique se a API está no ar e acessível publicamente."
		logger.From(ctx, s.logger).Warn("pagarme webhook self-test unreachable",
			zap.String("store_id", storeID),
			zap.String("url", url),
			zap.Error(err),
		)
		return out, nil
	}
	defer resp.Body.Close()

	out.Reachable = true
	out.HTTPStatus = resp.StatusCode
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		out.Healthy = true
		out.Message = "Endpoint no ar e respondendo. O lado do LiveCart está pronto para receber webhooks."
	case resp.StatusCode == http.StatusUnauthorized:
		out.Healthy = false
		out.Message = "O endpoint respondeu 401. As credenciais Basic Auth salvas aqui não conferem — confira usuário/senha do webhook."
	default:
		out.Healthy = false
		out.Message = fmt.Sprintf("O endpoint respondeu HTTP %d. Verifique os logs da API.", resp.StatusCode)
	}
	return out, nil
}

// pagarmeWebhookTestOrderPrefix marks the throwaway order created by the live
// (round-trip) webhook test, so its order code can be matched in Pagar.me's
// delivery history.
const pagarmeWebhookTestOrderPrefix = "LCWHTEST-"

// sameWebhookURL compares the URL Pagar.me reports in its delivery history
// against the URL we expect, ignoring differences that do NOT change where the
// request lands: surrounding whitespace, a trailing slash (Fiber routes
// /path and /path/ to the same handler), case of scheme/host, and any query
// string the merchant may have appended.
//
// Field bug (first client, 18/07/2026): the merchant pasted the URL with a
// trailing slash. Pagar.me delivered every event correctly — our handler logged
// them — but the strict string comparison reported "URL diferente da nossa",
// sending the merchant to fix something that was already right.
func sameWebhookURL(a, b string) bool {
	norm := func(raw string) string {
		raw = strings.TrimSpace(raw)
		u, err := url.Parse(raw)
		if err != nil || u.Host == "" {
			return strings.TrimRight(raw, "/")
		}
		return strings.ToLower(u.Scheme) + "://" + strings.ToLower(u.Host) + strings.TrimRight(u.Path, "/")
	}
	return norm(a) == norm(b)
}

// RunPagarmeWebhookLiveTest validates the REAL webhook delivery path on demand:
// it creates a throwaway PIX order so Pagar.me fires a real order.created
// webhook to the merchant's configured endpoint, then polls Pagar.me's own
// delivery history (GET /hooks) for that order to confirm the event reached our
// URL and what status we returned. The order is canceled at the end. Unlike the
// loopback self-test, this exercises the merchant's dashboard config end-to-end
// without waiting for a real customer payment.
func (s *Service) RunPagarmeWebhookLiveTest(ctx context.Context, integrationID, storeID string) (*PagarmeWebhookLiveTestOutput, error) {
	paymentProvider, err := s.GetPaymentProvider(ctx, integrationID, storeID)
	if err != nil {
		return nil, err
	}
	pagarme, ok := paymentProvider.(*payment.Pagarme)
	if !ok {
		return nil, httpx.ErrUnprocessable("integration is not Pagar.me")
	}

	expectedURL := buildProviderURLs("pagarme", storeID).WebhookURL
	if expectedURL == "" {
		return nil, httpx.ErrUnprocessable("URL de webhook indisponível — verifique a configuração do servidor")
	}

	code := pagarmeWebhookTestOrderPrefix + uuid.New().String()[:8]
	order, err := pagarme.CreateWebhookTestOrder(ctx, code)
	if err != nil {
		s.handleProviderError(ctx, integrationID, "create_webhook_test_order", err)
		return nil, httpx.ErrUnprocessable("não foi possível criar o pedido de teste na Pagar.me: " + err.Error())
	}

	// Always clean up the throwaway charge, whatever the outcome. Detached
	// context so cleanup runs even if the request context is canceled.
	defer func() {
		if order.ChargeID == "" {
			return
		}
		if cErr := pagarme.CancelTestCharge(context.Background(), order.ChargeID); cErr != nil {
			// A cobrança de teste costuma FALHAR sozinha na Pagar.me (é um Pix
			// descartável que ninguém paga) e aí não há o que cancelar — ela já
			// está em estado terminal. Isso não é problema: baixa o nível para
			// não poluir os alertas com um warn a cada teste de webhook.
			if strings.Contains(cErr.Error(), "cannot be canceled") {
				logger.From(ctx, s.logger).Info("webhook test charge already in a terminal state, nothing to cancel",
					zap.String("store_id", storeID),
					zap.String("charge_id", order.ChargeID),
				)
			} else {
				logger.From(ctx, s.logger).Warn("failed to cancel webhook test charge",
					zap.String("store_id", storeID),
					zap.String("charge_id", order.ChargeID),
					zap.Error(cErr),
				)
			}
		}
	}()

	out := &PagarmeWebhookLiveTestOutput{ExpectedURL: expectedURL, OrderCode: code}

	// Delivery is async — Pagar.me usually posts order.created within a few
	// seconds. Poll its delivery history for OUR order for up to ~18s.
	deadline := time.Now().Add(18 * time.Second)
	for {
		deliveries, derr := pagarme.ListRecentHookDeliveries(ctx, 30)
		if derr == nil {
			for _, d := range deliveries {
				// Match order.* deliveries by order id/code, and charge.*
				// deliveries by the charge id — both belong to our test order.
				if d.OrderCode != code && d.OrderID != order.OrderID && d.OrderID != order.ChargeID {
					continue
				}
				out.Delivered = true
				out.Event = d.Event
				out.HTTPStatus = d.ResponseStatus
				out.ResponseRaw = d.ResponseRaw
				out.DeliveredURL = d.URL
				switch {
				case !sameWebhookURL(d.URL, expectedURL):
					// Delivered our test event, but to a different URL than
					// ours — the dashboard endpoint doesn't match.
					out.Healthy = false
					out.Message = fmt.Sprintf(
						"A Pagar.me entregou o evento de teste, mas para uma URL diferente da nossa (entregue em %q, esperada %q). Ajuste a URL do webhook no painel para a exibida acima.",
						d.URL, expectedURL,
					)
				case d.ResponseStatus >= 200 && d.ResponseStatus < 300:
					out.Healthy = true
					out.Message = "Webhook validado de ponta a ponta: a Pagar.me disparou um evento real e nosso endpoint recebeu com sucesso (HTTP " + fmt.Sprintf("%d", d.ResponseStatus) + ")."
				default:
					out.Healthy = false
					msg := fmt.Sprintf("A Pagar.me entregou o evento, mas nosso endpoint respondeu HTTP %d.", d.ResponseStatus)
					if d.ResponseStatus == http.StatusUnauthorized {
						msg += " As credenciais Basic Auth do painel não conferem com as cadastradas aqui."
					}
					out.Message = msg
				}
				return out, nil
			}
		}
		if time.Now().After(deadline) {
			break
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}

	// No delivery for our order within the window: either the merchant didn't
	// subscribe to order.created, or the dashboard URL differs, or nothing was
	// wired at all.
	out.Delivered = false
	out.Healthy = false
	out.Message = "Criamos um pedido de teste, mas a Pagar.me não registrou nenhuma entrega no nosso endpoint. Confirme no painel da Pagar.me que a URL do webhook é idêntica à exibida acima e que o evento \"order.created\" (ou \"order.*\") está marcado."
	return out, nil
}

// =============================================================================
// WEBHOOK OPERATIONS
// =============================================================================

// StoreWebhookEvent stores a webhook event for processing.
func (s *Service) StoreWebhookEvent(ctx context.Context, input StoreWebhookInput) error {
	// Resolve integration from store_id + provider
	integrationType := "payment"
	switch input.Provider {
	case "tiny":
		integrationType = "erp"
	case "instagram":
		integrationType = "social"
	}
	integration, err := s.repo.GetActiveByProvider(ctx, input.StoreID, integrationType, input.Provider)
	if err != nil {
		return fmt.Errorf("no active integration found for store %s provider %s: %w", input.StoreID, input.Provider, err)
	}
	input.IntegrationID = integration.ID

	// Check for duplicate event
	existing, err := s.repo.GetWebhookEventByEventID(ctx, input.IntegrationID, input.EventID)
	if err != nil {
		return err
	}
	if existing != nil {
		logger.From(ctx, s.logger).Debug("duplicate webhook event, skipping",
			zap.String("event_id", input.EventID),
		)
		return nil
	}

	_, err = s.repo.CreateWebhookEvent(ctx, input)
	return err
}

// DispatchPaymentProcess and ProcessPaymentNotification were extracted to the
// payment.Service (strangler-fig B1d) and the call sites now dispatch straight
// to it (B1e): the webhook edge holds a payment.PaymentDispatcher, and the
// asynq consumer in main.newApp calls paymentSvc.ProcessPaymentNotification
// directly. integration.Service keeps only the CartPaymentGateway adapters
// below, so the consumer still runs against the SAME repository — same guarded
// UpdateCartPaymentStatus, same payment×expiration serialization.

// --- payment.CartPaymentGateway adapters (B1d) ---
// These let the extracted payment.Service run the payment webhook consumer
// against the SAME integration.Repository — same pool, same guarded
// UpdateCartPaymentStatus, same outbox. Declared in the payment package,
// implemented here. No new repo, no new advisory lock.
var _ paymentdomain.CartPaymentGateway = (*Service)(nil)

// ResolvePaymentProvider resolves the store's active payment provider for the
// webhook path (by store + provider name), returning it with the integration id
// used for provider-error reporting. Mirrors the resolution the inline
// ProcessPaymentNotification used to run.
func (s *Service) ResolvePaymentProvider(ctx context.Context, storeID, provider string) (providers.PaymentProvider, string, error) {
	integration, err := s.repo.GetActiveByProvider(ctx, storeID, "payment", provider)
	if err != nil {
		return nil, "", fmt.Errorf("no active payment integration found for store %s provider %s: %w", storeID, provider, err)
	}
	prov, err := s.createProviderFromRow(ctx, integration)
	if err != nil {
		return nil, "", err
	}
	paymentProvider, ok := prov.(providers.PaymentProvider)
	if !ok {
		return nil, integration.ID, fmt.Errorf("integration is not a payment provider")
	}
	return paymentProvider, integration.ID, nil
}

// RestoreCancelledCartAsPaid satisfies payment.CartPaymentGateway (LIV-84 inverse
// race): delega ao repo, que numa única tx restaura o cart cancelado pelo lojista
// para pago e retoma o estoque. Mesmo repo/pool do resto do consumer.
func (s *Service) RestoreCancelledCartAsPaid(ctx context.Context, cartID, storeID, paymentStatus, paymentID string, paidAt *time.Time, paymentMethod string) (bool, string, error) {
	return s.repo.RestoreCancelledCartAsPaid(ctx, cartID, storeID, paymentStatus, paymentID, paidAt, paymentMethod)
}

// CartPaymentStatus returns the cart's current payment status ("" when the cart
// no longer exists), swallowing the same not-found path GetCartByID does.
func (s *Service) CartPaymentStatus(ctx context.Context, cartID string) (string, error) {
	cart, err := s.repo.GetCartByID(ctx, cartID)
	if err != nil {
		return "", err
	}
	if cart == nil {
		return "", nil
	}
	return cart.PaymentStatus, nil
}

// UpdateCartPaymentStatus applies the guarded cart payment write. It returns
// paymentdomain.ErrCartNotPayable (the sentinel the repo now returns) when the
// cart expired/cancelled between the charge and the webhook, and the cart's
// live_event_id (from the same RETURNING row) on success.
func (s *Service) UpdateCartPaymentStatus(ctx context.Context, cartID, paymentStatus, paymentID string, paidAt *time.Time, paymentMethod string) (string, error) {
	return s.repo.UpdateCartPaymentStatus(ctx, cartID, paymentStatus, paymentID, paidAt, paymentMethod)
}

// CartGMVCents returns the pure item sum (excludes shipping and coupon) via the
// canonical GetCartGMVCents. It preserves the original two-tier behaviour: a
// malformed cart id yields (0, nil) (skip, no warn); a query failure yields
// (0, err) so the caller warns and emits gmv=0.
func (s *Service) CartGMVCents(ctx context.Context, cartID string) (int64, error) {
	cid, err := uuid.Parse(cartID)
	if err != nil {
		return 0, nil
	}
	return s.repo.queries.GetCartGMVCents(ctx, pgtype.UUID{Bytes: cid, Valid: true})
}

// EmitEvent emits a domain fact on the transactional outbox (events.Emit) using
// the repository's queries — the same outbox ProcessPaymentNotification used.
func (s *Service) EmitEvent(ctx context.Context, env events.Envelope) error {
	return events.Emit(ctx, s.repo.queries, env)
}

// EmitInternalCommand emits an internal command on the outbox (events.EmitInternal).
func (s *Service) EmitInternalCommand(ctx context.Context, name events.Name, dedupKey string, payload any) error {
	return events.EmitInternal(ctx, s.repo.queries, name, dedupKey, payload)
}

// OnCartCancelled runs the inline payment-cancel email hook, no-op when unwired.
func (s *Service) OnCartCancelled(ctx context.Context, cartID string) {
	if s.postCheckoutHook != nil {
		s.postCheckoutHook.OnCartCancelled(ctx, cartID)
	}
}

// ReactCartPaid is the cart.paid fan-out reactor (L3): records GMV on the billing
// ledger (billingGate.OnCartPaid). The customer post-payment flow no longer runs
// here — o tracking_token e a timeline `payment_confirmed` nascem na materialização
// da Order (order/listeners.OnCartPaid, Fatia A4), que roda ANTES deste reactor.
// The coupon redemption confirmation also reacts to cart.paid as its own Coupon
// reactor (coupon/listeners.OnCartPaid → ConfirmRedemption), alongside the buyer
// receipt (notification/listeners.OnCartPaid → SendPaidReceipt) and the waitlist
// fulfilment (inventory/listeners.OnCartPaid) reactors that react to the fact
// instead of being called inline in this fan-out.
// The billing step is idempotent (ledger UNIQUE), so asynq redelivery/retry is safe.
// ERP finalisation is a separate reactor (ReactOrderPaidERP, triggered by order.paid)
// because it needs the gateway snapshot.
func (s *Service) ReactCartPaid(ctx context.Context, cartID, storeID string, gmvCents int64) error {
	if s.billingGate != nil {
		if err := s.billingGate.OnCartPaid(ctx, storeID, cartID, gmvCents); err != nil {
			return fmt.Errorf("billing ledger on cart.paid: %w", err)
		}
	}
	return nil
}

// ReactCartRefunded is the cart.refunded fan-out reactor (L3): credits the success
// fee on the billing ledger (billingGate.OnCartRefunded). The coupon redemption
// refund is NO LONGER here: it reacts to cart.refunded as its own Coupon reactor
// (coupon/listeners.OnCartRefunded → RefundRedemption). The billing step is
// idempotent (ledger UNIQUE), so asynq redelivery/retry is safe. The ERP refund
// moved to ReactOrderRefundedERP (triggered by the order.refunded fact, Fatia
// 11b-2), and the refund EMAIL reacts to cart.refunded as its own Notification
// reactor (notification/listeners.OnCartRefunded) — both decoupled from this fan-out.
func (s *Service) ReactCartRefunded(ctx context.Context, cartID, storeID string) error {
	// O reembolso mata a venda; o carrinho morre junto. Sem este flip o pedido
	// ficava 'active'+refunded para sempre: fora de "Cancelados" (que filtra por
	// status do carrinho) e preso em "Precisam atenção" (o matcher casa
	// payment_status='refunded'), sem NENHUMA ação que o tirasse de lá — o
	// cancelamento manual recusa carrinho não-pendente. Idempotente (guard na
	// query), então a redelivery do asynq re-passa em silêncio.
	flipped, err := s.repo.CancelCartOnRefund(ctx, cartID)
	if err != nil {
		return fmt.Errorf("cancelling cart on refund: %w", err)
	}
	if flipped {
		logger.From(ctx, s.logger).Info("refunded cart moved to cancelled",
			zap.String("cart_id", cartID))
	}
	if s.billingGate != nil {
		if err := s.billingGate.OnCartRefunded(ctx, storeID, cartID); err != nil {
			return fmt.Errorf("billing ledger on cart.refunded: %w", err)
		}
	}
	return nil
}

// emitERPOrderFinalized publishes the group G erp.order_finalized fact for a
// cart that reached the terminal 'done' state (legacy [S4], inverted, resume or
// confirm). Best-effort and dedup by cart — the terminal marker is monotonic,
// so the fact rides the single transition to done. Reloads the external order
// id best-effort for the payload.
func (s *Service) emitERPOrderFinalized(ctx context.Context, storeID, cartID string) {
	externalOrderID := ""
	if fresh, err := s.repo.GetCartForPaidOrder(ctx, cartID); err == nil {
		externalOrderID = fresh.ExternalOrderID
	}
	_ = events.EmitInternal(ctx, s.repo.queries, events.ERPOrderFinalized, "erp.order_finalized:"+cartID, struct {
		StoreID         string `json:"store_id"`
		CartID          string `json:"cart_id"`
		ExternalOrderID string `json:"external_order_id"`
		Provider        string `json:"provider"`
	}{StoreID: storeID, CartID: cartID, ExternalOrderID: externalOrderID, Provider: "tiny"})
}

// reverseCartReservationsPerRow estorna todas as reservas 'active' do cart,
// marcando cada row somente após o Tiny confirmar a entrada E correspondente.
// NÃO altera erp_finalisation_status — quem decide o efeito de uma falha é o
// caller (a finalização pós-pago marca 'failed'; a conversão pré-pagamento do
// design C não toca nessa coluna).
func (s *Service) reverseCartReservationsPerRow(ctx context.Context, erpProvider providers.ERPProvider, storeID, cartID string) error {
	reservations, err := s.repo.ListActiveReservationsByCart(ctx, cartID)
	if err != nil {
		return fmt.Errorf("listing cart reservations: %w", err)
	}
	// Reivindica ANTES de chamar o ERP. A ordem inversa que vivia aqui já vinha
	// documentada como perigosa em comentário — "um retry re-estornaria esta row
	// e duplicaria a entrada E" — e em 08/08 aconteceu: o extrato do Tiny ficou
	// com duas entradas idênticas e o produto ganhou unidades que não existiam.
	// Ver erp.ReverseReservationsClaimFirst.
	rows := make([]erp.ReversibleReservation, 0, len(reservations))
	for _, r := range reservations {
		rows = append(rows, erp.ReversibleReservation{
			ID:                r.ID,
			ExternalProductID: r.ExternalProductID,
			Quantity:          r.Quantity,
			CartID:            r.CartID,
			EventID:           r.EventID,
			ProductID:         r.ProductID,
		})
	}
	if _, allResolved := erp.ReverseReservationsClaimFirst(ctx, s.logger, s.repo, erpProvider, rows,
		func(erp.ReversibleReservation) string {
			return fmt.Sprintf("Estorno reserva pós-pagamento - Cart %s", cartID)
		}, s.erpStock().ReversalLedgerHooks(storeID)); !allResolved {
		return fmt.Errorf("reversing reservations for cart %s: estorno de reserva pendente", cartID)
	}
	return nil
}

// markFinalisationFailed grava o estado 'failed' com o erro e loga falhas da
// própria marcação (nunca as propaga — o sinal primário é o erro do caller).
func (s *Service) markFinalisationFailed(ctx context.Context, cartID, msg string, snapshot []byte) {
	if markErr := s.repo.MarkCartERPFinalisationFailed(ctx, cartID, msg, snapshot); markErr != nil {
		logger.From(ctx, s.logger).Error("failed to mark cart ERP finalisation failed",
			zap.String("cart_id", cartID),
			zap.Error(markErr),
		)
	}
	s.emitERPFinalizationFailed(ctx, cartID, msg)
}

// emitERPFinalizationFailed publishes the group G erp.finalization_failed fact
// for a cart whose ERP finalisation aborted (retryable). Best-effort. Dedup is
// intentionally coarse (cart-scoped) so a retry-after-failure re-surfaces the
// signal on the outbox rather than being silently collapsed; storeID is
// reloaded best-effort for the payload.
func (s *Service) emitERPFinalizationFailed(ctx context.Context, cartID, reason string) {
	storeID := ""
	externalOrderID := ""
	if fresh, err := s.repo.GetCartForPaidOrder(ctx, cartID); err == nil {
		storeID = fresh.StoreID
		externalOrderID = fresh.ExternalOrderID
	}
	_ = events.EmitInternal(ctx, s.repo.queries, events.ERPFinalizationFailed, "erp.finalization_failed:"+cartID, struct {
		StoreID         string `json:"store_id"`
		CartID          string `json:"cart_id"`
		ExternalOrderID string `json:"external_order_id"`
		Provider        string `json:"provider"`
		Reason          string `json:"reason"`
	}{StoreID: storeID, CartID: cartID, ExternalOrderID: externalOrderID, Provider: "tiny", Reason: reason})
}

// reReserveAfterFailedFinalisation re-creates Tiny saída-manual exits for the
// reservations we reversed during the failed finalisation, and inserts new
// active rows in stock_reservations so the cart still owns the units. Errors
// here are logged but never returned — the caller's primary signal is the
// upstream createFinalERPOrder error, and we don't want to mask it. Even a
// partial success keeps more stock held than the alternative of releasing
// everything.
func (s *Service) reReserveAfterFailedFinalisation(ctx context.Context, erpProvider providers.ERPProvider, cartID, eventID string, snapshot []StockReservationRow) {
	if len(snapshot) == 0 {
		return
	}
	logger.From(ctx, s.logger).Warn("re-reserving stock after failed ERP finalisation",
		zap.String("cart_id", cartID),
		zap.Int("reservations_count", len(snapshot)),
	)
	restored := 0
	for _, r := range snapshot {
		obs := fmt.Sprintf("Re-reserva pós-falha de finalização - Cart %s", cartID)
		movementID, reserveErr := erpProvider.ReserveStock(ctx, r.ExternalProductID, r.Quantity, 0, obs)
		if reserveErr != nil {
			logger.From(ctx, s.logger).Error("failed to re-reserve stock after finalisation failure",
				zap.String("cart_id", cartID),
				zap.String("external_product_id", r.ExternalProductID),
				zap.Int("quantity", r.Quantity),
				zap.Error(reserveErr),
			)
			continue
		}
		if _, dbErr := s.repo.CreateStockReservation(ctx, CreateStockReservationParams{
			EventID:           eventID,
			CartID:            cartID,
			ProductID:         r.ProductID,
			ExternalProductID: r.ExternalProductID,
			Quantity:          r.Quantity,
			ERPMovementID:     movementID,
		}); dbErr != nil {
			// Tiny holds the stock, but our DB row failed. The reservation
			// is real on the merchant's side — flag loudly so we can
			// reconcile manually instead of silently losing the link.
			logger.From(ctx, s.logger).Error("re-reserved on Tiny but failed to persist reservation row",
				zap.String("cart_id", cartID),
				zap.String("external_product_id", r.ExternalProductID),
				zap.String("erp_movement_id", movementID),
				zap.Int("quantity", r.Quantity),
				zap.Error(dbErr),
			)
			continue
		}
		restored++
	}
	logger.From(ctx, s.logger).Info("re-reservation after failed ERP finalisation completed",
		zap.String("cart_id", cartID),
		zap.Int("requested", len(snapshot)),
		zap.Int("succeeded", restored),
	)
}

// =============================================================================
// MELHOR ENVIO WEBHOOK
// =============================================================================

// ApplyMelhorEnvioWebhookInput is the normalised payload extracted from a
// melhor_envio webhook. The handler validates and parses; the service applies
// the side effects (status update + tracking event).
type ApplyMelhorEnvioWebhookInput struct {
	StoreID           string
	ProviderOrderID   string
	Event             string
	Status            string
	TrackingCode      string
	PublicTrackingURL string
	PostedAt          *string
	DeliveredAt       *string
	CanceledAt        *string
	ExpiredAt         *string
}

// ApplyMelhorEnvioWebhook updates the local shipment row to reflect a ME
// order.* webhook and appends a corresponding tracking event. No-op when the
// shipment isn't known locally — that happens when the ME account fires
// webhooks for orders created outside LiveCart and we don't want to leak
// errors back to the provider (which disables the webhook URL after enough
// failures).
func (s *Service) ApplyMelhorEnvioWebhook(ctx context.Context, in ApplyMelhorEnvioWebhookInput) error {
	if in.ProviderOrderID == "" {
		return nil
	}
	shipment, err := s.repo.GetShipmentByProviderOrderID(ctx, "melhor_envio", in.ProviderOrderID)
	if err != nil {
		return fmt.Errorf("looking up me shipment: %w", err)
	}
	if shipment == nil {
		logger.From(ctx, s.logger).Debug("melhor_envio webhook for unknown shipment — ignoring",
			zap.String("store_id", in.StoreID),
			zap.String("provider_order_id", in.ProviderOrderID),
		)
		return nil
	}

	normalised := mapMelhorEnvioWebhookStatus(in.Event, in.Status)

	// Persist the canonical status + tracking code on the parent row.
	if err := s.repo.UpdateShipmentStatus(ctx, shipment.ID, string(normalised), 0, in.Status, in.TrackingCode); err != nil {
		return fmt.Errorf("updating shipment status: %w", err)
	}
	// Mirror the public tracking URL when the carrier eventually exposes one
	// (it lands on order.posted).
	if in.PublicTrackingURL != "" || in.TrackingCode != "" {
		if err := s.repo.UpdateShipmentLabels(ctx, shipment.ID, shipment.LabelURL, in.PublicTrackingURL, in.TrackingCode); err != nil {
			logger.From(ctx, s.logger).Warn("failed to mirror tracking url onto shipment",
				zap.String("shipment_id", shipment.ID),
				zap.Error(err),
			)
		}
	}

	// Append the event to the timeline. We pick the most specific timestamp
	// available for the event so the FE timeline matches what the merchant
	// sees on the ME panel.
	eventAt := pickMelhorEnvioEventTime(in)
	if err := s.repo.InsertTrackingEvents(ctx, shipment.ID, []TrackingEventInput{{
		Status:      string(normalised),
		RawCode:     0,
		RawName:     in.Event,
		Observation: meWebhookObservation(in.Event),
		EventAt:     eventAt,
		Source:      "webhook",
	}}); err != nil {
		return fmt.Errorf("inserting tracking event: %w", err)
	}

	// Group H fact (best-effort): carrier-level status transition from a ME
	// webhook (posted/in_transit/out_for_delivery/returned/etc). Dedup by
	// shipment + normalised status so redelivered webhooks collapse per state.
	_ = events.EmitInternal(ctx, s.repo.queries, events.ShipmentStatusUpdated, "shipment.status_updated:"+shipment.ID+":"+string(normalised), struct {
		StoreID      string `json:"store_id"`
		ShipmentID   string `json:"shipment_id"`
		OrderID      string `json:"order_id"`
		Provider     string `json:"provider"`
		Status       string `json:"status"`
		TrackingCode string `json:"tracking_code"`
	}{StoreID: in.StoreID, ShipmentID: shipment.ID, OrderID: shipment.CartID, Provider: "melhor_envio", Status: string(normalised), TrackingCode: in.TrackingCode})

	// Group H fact (best-effort): delivery confirmed. Dedup by shipment id.
	if normalised == providers.TrackingStatusDelivered {
		_ = events.EmitInternal(ctx, s.repo.queries, events.DeliveryConfirmed, "shipment.delivered:"+shipment.ID, struct {
			StoreID      string `json:"store_id"`
			ShipmentID   string `json:"shipment_id"`
			OrderID      string `json:"order_id"`
			Provider     string `json:"provider"`
			TrackingCode string `json:"tracking_code"`
		}{StoreID: in.StoreID, ShipmentID: shipment.ID, OrderID: shipment.CartID, Provider: "melhor_envio", TrackingCode: in.TrackingCode})
	}

	// Customer-facing notification on first dispatch — same hook the manual
	// CreateShipment flow uses when the carrier returns the tracking inline.
	if normalised == providers.TrackingStatusInTransit && in.TrackingCode != "" && s.postCheckoutHook != nil {
		s.postCheckoutHook.OnShipmentPosted(ctx, shipment.CartID, in.TrackingCode)
	}
	return nil
}

func mapMelhorEnvioWebhookStatus(event, raw string) providers.TrackingStatus {
	switch event {
	case "order.posted":
		return providers.TrackingStatusInTransit
	case "order.delivered":
		return providers.TrackingStatusDelivered
	case "order.cancelled", "order.canceled":
		return providers.TrackingStatusCanceled
	case "order.undelivered":
		return providers.TrackingStatusNotDelivered
	case "order.suspended":
		return providers.TrackingStatusShipmentBlocked
	case "order.released":
		return providers.TrackingStatusAwaitingPickup
	case "order.created", "order.pending":
		return providers.TrackingStatusPending
	}
	// Fallback: trust the raw `status` field on the payload.
	return mapMelhorEnvioStatusRaw(raw)
}

func mapMelhorEnvioStatusRaw(raw string) providers.TrackingStatus {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "pending":
		return providers.TrackingStatusPending
	case "released":
		return providers.TrackingStatusAwaitingPickup
	case "posted":
		return providers.TrackingStatusInTransit
	case "delivered":
		return providers.TrackingStatusDelivered
	case "cancelled", "canceled":
		return providers.TrackingStatusCanceled
	case "undelivered":
		return providers.TrackingStatusNotDelivered
	case "suspended":
		return providers.TrackingStatusShipmentBlocked
	default:
		return providers.TrackingStatusUnknown
	}
}

func pickMelhorEnvioEventTime(in ApplyMelhorEnvioWebhookInput) time.Time {
	candidates := []*string{in.DeliveredAt, in.CanceledAt, in.ExpiredAt, in.PostedAt}
	for _, c := range candidates {
		if c == nil || *c == "" {
			continue
		}
		if t, err := time.Parse(time.RFC3339, *c); err == nil {
			return t
		}
	}
	return time.Now()
}

func meWebhookObservation(event string) string {
	switch event {
	case "order.created":
		return "Etiqueta gerada"
	case "order.released":
		return "Etiqueta paga"
	case "order.posted":
		return "Postado"
	case "order.delivered":
		return "Entregue"
	case "order.cancelled", "order.canceled":
		return "Cancelado"
	case "order.undelivered":
		return "Não entregue"
	case "order.suspended":
		return "Suspenso"
	case "order.pending":
		return "Aguardando pagamento"
	default:
		return event
	}
}

// =============================================================================
// INSTAGRAM WEBHOOK OPERATIONS
// =============================================================================

// DispatchCommentReceived is the inbound edge of the inverted comment flow: the
// webhook validates and dispatches a canonical comment.received event (via the
// transactional outbox) instead of processing inline. The domain work runs in
// the event consumer, which calls ProcessInstagramComment. The origin
// (live/story/dm) travels as the event Source, keeping the event canonical.
//
// Idempotency: ProcessInstagramComment already dedups by platform_comment_id,
// so at-least-once redelivery is safe. dedup_key mirrors that for visibility.
func (s *Service) DispatchCommentReceived(ctx context.Context, input ProcessInstagramCommentInput, source events.Source) error {
	return s.liveService.DispatchCommentReceived(ctx, input.CommentID, input.MediaID, source, input)
}

// DispatchMessageReceived é a borda HTTP do fluxo invertido de DM: valida o
// mínimo, grava message.received no outbox e volta. Nada de lookup, Graph ou
// ERP — o trabalho roda em HandleMessageReceived, no consumidor.
//
// A Meta exige 200 em ≤5s e desinscreve o app após 1 hora de falha contínua.
// O caminho antigo fazia tudo em linha e chegava a ~90s no pior caso (dois
// POSTs à Graph com timeout de 30s, mais refresh de token).
//
// Descartar echo aqui é de propósito: é a nossa PRÓPRIA mensagem voltando, e
// enfileirá-la só para o consumidor ignorar seria gastar uma tarefa por DM
// enviada. O mesmo vale para evento sem `mid` (recibo de leitura, reação),
// que não é mensagem — o discriminador é o mesmo usado no handler.
func (s *Service) DispatchMessageReceived(ctx context.Context, input ProcessInstagramMessageInput) error {
	if input.IsEcho || input.MessageID == "" {
		return nil
	}
	body, err := json.Marshal(input)
	if err != nil {
		return fmt.Errorf("marshaling message.received payload: %w", err)
	}
	return s.repo.EmitMessageReceived(ctx, events.Envelope{
		Name:     events.MessageReceived,
		Source:   events.SourceInstagramDM,
		DedupKey: "message.received:" + input.MessageID,
		Metadata: map[string]string{"account_id": input.AccountID, "message_id": input.MessageID},
		Payload:  body,
	})
}

// HandleMessageReceived processes a DM from Instagram webhook. Runs in the event
// consumer (ver DispatchMessageReceived), não mais dentro do request.
func (s *Service) HandleMessageReceived(ctx context.Context, input ProcessInstagramMessageInput) error {
	logger.From(ctx, s.logger).Info("processing instagram message",
		zap.String("account_id", input.AccountID),
		zap.String("sender_id", input.SenderID),
		zap.String("message_id", input.MessageID),
		zap.String("text", input.Text),
		zap.String("reply_to_story_id", input.ReplyToStoryID),
		zap.Bool("is_echo", input.IsEcho),
	)

	// Skip echoes of our own outbound messages.
	if input.IsEcho {
		return nil
	}

	// Story-commerce: a DM that replies to one of our published Stories carries
	// reply_to.story.id. Resolve the store from the story EVENT (not the account
	// lookup, which the comment/live paths also avoid) and feed it into the same
	// intent→cart pipeline as comments, answering via DM.
	if input.ReplyToStoryID != "" {
		if err := s.processStoryReply(ctx, input); err != nil {
			logger.From(ctx, s.logger).Warn("failed to process story reply",
				zap.String("story_id", input.ReplyToStoryID),
				zap.Error(err))
		}
		return nil
	}

	// Non-story DM: resolve the store from the Instagram account ID for audit +
	// the "Testar notificação" setup capture.
	integration, err := s.repo.GetByInstagramUserID(ctx, input.AccountID)
	if err != nil {
		logger.From(ctx, s.logger).Error("failed to find integration by instagram account",
			zap.String("account_id", input.AccountID),
			zap.Error(err),
		)
		return nil // Don't fail the webhook, just skip storage
	}
	if integration == nil {
		logger.From(ctx, s.logger).Warn("no integration found for instagram account",
			zap.String("account_id", input.AccountID),
		)
		return nil
	}

	// Store resolved (account_id → integration): enrich the ctx for the logs below.
	ctx = logger.WithStore(ctx, integration.StoreID, "")

	// Store webhook event for audit trail
	if len(input.RawPayload) > 0 {
		if err := s.StoreWebhookEvent(ctx, StoreWebhookInput{
			StoreID:        integration.StoreID,
			Provider:       "instagram",
			EventType:      "messaging",
			EventID:        input.MessageID,
			Payload:        input.RawPayload,
			SignatureValid: input.SignatureValid,
		}); err != nil {
			logger.From(ctx, s.logger).Error("failed to store instagram dm webhook event",
				zap.String("message_id", input.MessageID),
				zap.Error(err),
			)
			// Don't return error - continue processing
		}
	}

	// If the message text matches an active "Testar notificação" setup code,
	// capture this sender as the store's test recipient. We swallow errors
	// here because a webhook should never fail on optional bookkeeping.
	if s.notificationService != nil && input.Text != "" {
		storeID, setupErr := s.notificationService.CompleteTestRecipientSetup(ctx, input.Text, input.SenderID, "")
		if setupErr != nil {
			logger.From(ctx, s.logger).Warn("failed to complete test recipient setup",
				zap.String("account_id", input.AccountID),
				zap.Error(setupErr),
			)
		} else if storeID != "" {
			logger.From(ctx, s.logger).Info("test recipient configured",
				zap.String("store_id", storeID),
				zap.String("sender_id", input.SenderID),
			)
		}
	}

	return nil
}

// processStoryReply turns a DM reply to a published Story into a purchase intent.
// It resolves the bound story-commerce event by the replied-to story media id,
// best-effort resolves the buyer's @handle, then reuses the comment→cart
// pipeline with the DM reply channel (answers go straight to the buyer's DM).
func (s *Service) processStoryReply(ctx context.Context, input ProcessInstagramMessageInput) error {
	// Only act on replies to one of our story-commerce events. The event carries
	// the store, so we don't need the (separate, sometimes-missing) account lookup.
	//
	// D3: quem diz que a mídia é um story é a SESSÃO dona dela — o tipo desceu
	// para live_sessions e este caminho já resolve pela mídia, então é o lookup
	// mais direto que existe.
	session, err := s.liveService.GetSessionByPlatformLiveID(ctx, input.ReplyToStoryID)
	if err != nil {
		return fmt.Errorf("resolving story session: %w", err)
	}
	event, err := s.liveService.GetEventByPlatformLiveID(ctx, input.ReplyToStoryID)
	if err != nil {
		return fmt.Errorf("resolving story event: %w", err)
	}
	if session == nil || event == nil || session.Type != live.SessionTypeStory {
		// Reply to a non-commerce story (or unknown) — nothing to do.
		logger.From(ctx, s.logger).Info("story reply ignored: no matching story session",
			zap.String("story_id", input.ReplyToStoryID))
		return nil
	}
	storeID := event.StoreID
	// Store resolved (reply_to_story → event): enrich the ctx for the logs below.
	ctx = logger.WithStore(ctx, storeID, "")

	// Resolve the buyer's @handle from their IGSID (best-effort — the DM still
	// works without it; we just fall back to a generic label).
	username := ""
	if provider, perr := s.resolveInstagramSocialProvider(ctx, storeID); perr == nil {
		if uname, uerr := provider.GetUsername(ctx, input.SenderID); uerr == nil {
			username = uname
		} else {
			logger.From(ctx, s.logger).Warn("failed to resolve story buyer username",
				zap.String("sender_id", input.SenderID), zap.Error(uerr))
		}
	}
	if username == "" {
		username = "cliente"
	}

	// Inverted flow: dispatch canonical comment.received with the story origin.
	// Channel="dm" is preserved in the payload so the consumer replies via DM.
	return s.DispatchCommentReceived(ctx, ProcessInstagramCommentInput{
		AccountID:  input.AccountID,
		MediaID:    input.ReplyToStoryID,
		CommentID:  input.MessageID, // DM mid — dedup key
		UserID:     input.SenderID,  // IGSID — DM recipient
		Username:   username,
		Text:       input.Text,
		Timestamp:  input.Timestamp,
		Channel:    "dm",
		RawPayload: input.RawPayload,
		// A venda por story nasce de um DM: o resultado da assinatura da
		// requisição que trouxe o DM é o mesmo que vale para o comentário
		// canônico gerado a partir dele. Não repassar faria a auditoria do
		// story registrar "assinatura inválida" para tráfego legítimo da Meta —
		// o erro simétrico, e igualmente capaz de travar o deploy 2.
		SignatureValid: input.SignatureValid,
	}, events.SourceInstagramStory)
}

// MarkMediaWebhookActive flags THIS media as webhook-driven, so the polling
// capture stops for ela (e só para ela). Delegates to the live service.
func (s *Service) MarkMediaWebhookActive(ctx context.Context, mediaID string) error {
	return s.liveService.MarkMediaWebhookActive(ctx, mediaID)
}

// StartLiveSessionSweep launches a background loop that closes live sessions
// whose broadcast has gone off air. It no longer captures comments: those
// arrive by webhook only.
//
// O laço sobreviveu à remoção do polling porque ele não era só captura. Sem
// este encerramento a sessão fica "no ar" para sempre no painel — o lojista
// fecha o Instagram e vai embora, e voltar para clicar em "Encerrar" é o passo
// que ninguém dá.
func (s *Service) StartLiveSessionSweep(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(liveSessionSweepInterval)
		defer ticker.Stop()
		logger.From(ctx, s.logger).Info("live session sweep started")
		for {
			select {
			case <-ctx.Done():
				logger.From(ctx, s.logger).Info("live session sweep stopped")
				return
			case <-ticker.C:
				s.endStaleLiveSessionsOnce(ctx)
			}
		}
	}()
}

// A varredura só precisa notar que a transmissão acabou; não há comprador
// esperando por ela, então a cadência antiga de ocioso serve.
const liveSessionSweepInterval = 20 * time.Second

// isMediaGoneError reports whether an Instagram Graph error means the media is
// permanently unreachable for us (deleted or no longer accessible) rather than a
// transient failure. Graph signals this with code 100 / subcode 33 and the
// "does not exist, cannot be loaded due to missing permissions" message.
func isMediaGoneError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "error_subcode\":33") ||
		strings.Contains(msg, "does not exist, cannot be loaded")
}

// =============================================================================

// A CÓPIA da ingestão de post/story que morava aqui FOI REMOVIDA
// (resolvePostEventProduct, savePostComment e as respostas replyPost*). Era
// código órfão: quem processa comentário é live.Service.ProcessInstagramComment,
// chamado logo acima — este pacote só entrega o webhook.
//
// A única coisa que a cópia tinha de melhor que o original era o bypass
// "lista vazia = vende tudo"; ele foi PORTADO para live/comment.go
// (resolvePostEventProduct) antes desta remoção, e é lá que a regra vive agora.

// replyOutOfWindow responde quem comentou fora da janela de venda (RN-28,
// gatilho 1). Um funil para os três sub-casos, porque a única coisa que muda
// entre eles é o template e o conjunto de variáveis que faz sentido preencher.
//
// O texto vem das settings da loja, não do código: era exatamente isto que
// faltava. As três frases viviam hardcoded aqui, fora do subsistema de
// Comunicações — o lojista não conseguia editar, o envio não consultava
// ShouldNotify e nada era registrado em notification_logs. Passar pelo
// notification.Service resolve os três de uma vez e é o que faz a RN-38 ter o
// que listar: a mensagem que não sai vira linha com motivo, não silêncio.
//
// CommentCreatedAt carrega o carimbo do comentário para que o serviço decida
// entre tentar e registrar não-entrega. DirectOnly marca o caminho de story,
// em que a resposta é DM porque não existe comentário público para responder.
func (s *Service) replyOutOfWindow(
	ctx context.Context,
	event *live.EventOutput,
	session *live.SessionOutput,
	input ProcessInstagramCommentInput,
	notifType notification.NotificationType,
) {
	if s.notificationService == nil {
		return
	}

	shouldNotify, err := s.notificationService.ShouldNotify(ctx, event.StoreID, notifType, false)
	if err != nil {
		logger.From(ctx, s.logger).Warn("failed to check out-of-window notification settings",
			zap.String("type", string(notifType)),
			zap.Error(err),
		)
		return
	}
	if !shouldNotify {
		return
	}

	vars := notification.TemplateVariables{
		Handle:     "@" + input.Username,
		LiveTitulo: event.Title,
	}
	if storeInfo, err := s.repo.GetStoreInfo(ctx, event.StoreID); err == nil {
		vars.Loja = storeInfo.Name
	}

	switch notifType {
	case notification.TypeOutOfWindowScheduled:
		// A data é opcional de propósito: um evento 'scheduled' sem instante
		// marcado também cai aqui (D19). Antes o ponteiro era desreferenciado
		// só depois de um if; agora o vazio simplesmente não substitui nada, e
		// é responsabilidade do texto padrão não prometer data que não existe.
		if event.ScheduledAt != nil {
			vars.ComecaEm = live.FormatBRT(*event.ScheduledAt)
		}
	case notification.TypeOutOfWindowSessionEnded:
		// Único sub-caso em que a campanha AINDA vende: o texto redireciona, e
		// para isso precisa do link do carrinho que o comprador já tem.
		if session != nil {
			vars.Sessao = live.SessionLabel(session.Type, session.SequenceOrder)
		}
		vars.Link = s.existingCartLink(ctx, event.ID, input.UserID)
	}

	commentAt := time.Time{}
	if secs := epochSeconds(input.Timestamp); secs > 0 {
		commentAt = time.Unix(secs, 0)
	}
	directOnly := input.Channel == "dm"
	commentID := input.CommentID
	if directOnly {
		commentID = ""
	}

	if _, err := s.notificationService.Send(ctx, notification.SendInput{
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
}

// existingCartLink devolve o link do carrinho que o comprador já tem nesta
// campanha, ou vazio quando não há. Best-effort: a mensagem sem {link} continua
// fazendo sentido, e uma falha de leitura não pode calar a resposta.
func (s *Service) existingCartLink(ctx context.Context, eventID, platformUserID string) string {
	cart, err := s.repo.GetCartByEventAndUser(ctx, eventID, platformUserID)
	if err != nil || cart == nil {
		return ""
	}
	token, err := s.repo.GetCartTokenByID(ctx, cart.ID)
	if err != nil || token == "" {
		return ""
	}
	return fmt.Sprintf("%s/cart/%s", config.FrontendURL.StringOr("http://localhost:3000"), token)
}

// privateReplyWindow é o limite do private reply do Instagram: 7 dias a contar
// do comentário, e uma única vez por comentário (N9/RN-37). Depois disso a
// mensagem não sai de qualquer forma — tentar é só gastar chamada de API para
// receber erro.
//
// O número vive no domínio da notificação, que é quem decide entre "tenta" e
// "registra não entregue com motivo" (RN-38). Aqui é alias: duas cópias do
// mesmo prazo divergiriam e a ingestão passaria a descartar num limite e o
// registro a classificar noutro.
const privateReplyWindow = notification.PrivateReplyWindow

// commentTooOldToReply reporta se o comentário já passou da janela de private
// reply. Sem carimbo de tempo (ts <= 0) responde false: erra para o lado de
// TENTAR enviar — silenciar por falta de dado seria pior do que uma chamada
// perdida.
//
// ATENÇÃO: o último chamador de produção saiu junto com a cópia órfã da
// ingestão de post/story (acima). A ingestão VIVA, em live/comment.go, responde
// ao comprador sem passar por este guard — a RN-37 vale hoje só no caminho de
// notificação. Fica aqui, com o teste, porque é a definição do prazo; ligar o
// guard em live é fatia própria, não desta rodada.
//
// Normaliza ms→s aqui também, e não só na borda (E41): este é O guard, e o bug
// que ele deixou passar por meses foi exatamente um carimbo em milissegundos
// chegando de um chamador. Um caminho novo que esqueça de converter é
// silenciosamente inofensivo — a diferença aparece como "sempre recente",
// nunca como erro.
func commentTooOldToReply(ts int64, now time.Time) bool {
	secs := epochSeconds(ts)
	if secs <= 0 {
		return false
	}
	return now.Sub(time.Unix(secs, 0)) > privateReplyWindow
}

// parseGraphTimestamp converte o timestamp ISO8601 do Graph
// ("2026-08-01T20:00:00+0000") para unix seconds. Devolve 0 quando não dá.
func parseGraphTimestamp(v string) int64 {
	if v == "" {
		return 0
	}
	for _, layout := range []string{"2006-01-02T15:04:05-0700", time.RFC3339} {
		if t, err := time.Parse(layout, v); err == nil {
			return t.Unix()
		}
	}
	return 0
}

// =============================================================================
// CART → ERP STOCK RESERVATION
// =============================================================================

// ReserveStockInERP delega para erp.Service (Bloco B2b — a lógica vive em
// internal/erp). A assinatura pública é preservada: checkout, waitlist e os
// call sites internos continuam chamando integration.Service. A troca dos call
// sites para erp.Service direto é B2e.
func (s *Service) ReserveStockInERP(ctx context.Context, storeID, cartID, eventID, productID string, quantity int, unitPrice int64, platformHandle string) error {
	return s.erpStock().ReserveStockInERP(ctx, storeID, cartID, eventID, productID, quantity, unitPrice, platformHandle)
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
	return s.erpStock().AdjustStockReservationDelta(ctx, storeID, cartID, eventID, productID, delta, unitPrice, platformHandle, op)
}

// createFinalERPOrder creates a single paid sales order in the ERP for a cart
// whose payment was just confirmed. Uses the customer identity + shipping
// address captured at checkout and the payment details from the provider.
// approve diz se a VENDA está fechada — separado de paymentStatus, que diz
// apenas quem lança o recebimento. Pagamento recebido por fora fecha a venda
// (aprova) sem trazer financeiro; a conversão pré-pagamento não fecha nada.
func (s *Service) createFinalERPOrder(ctx context.Context, erpProvider providers.ERPProvider, integration *IntegrationRow, storeID, eventID string, cart CartRow, paymentStatus *providers.PaymentStatus, launchStock, approve bool) error {
	// Resolve contact — enriched with customer identity when available, so the
	// Tiny contact ends up with CPF/email/phone instead of just the @handle.
	contactID, err := s.resolveERPContact(ctx, erpProvider, integration, storeID, cart.PlatformUserID, cart.PlatformHandle, cart.CustomerName, cart.CustomerDocument, cart.CustomerEmail, cart.CustomerPhone)
	if err != nil {
		return fmt.Errorf("resolving ERP contact: %w", err)
	}

	// Collect non-waitlisted items
	items, err := s.repo.ListNonWaitlistedCartItems(ctx, cart.ID)
	if err != nil {
		return fmt.Errorf("listing cart items: %w", err)
	}

	var erpItems []providers.ERPOrderItem
	var totalAmount int64
	for _, item := range items {
		if item.ProductExternalID == "" {
			continue
		}
		erpItems = append(erpItems, providers.ERPOrderItem{
			ProductID: item.ProductExternalID,
			Name:      item.ProductName,
			Quantity:  item.Quantity,
			UnitPrice: item.UnitPrice,
		})
		totalAmount += item.UnitPrice * int64(item.Quantity)
	}

	if len(erpItems) == 0 {
		logger.From(ctx, s.logger).Warn("paid cart has no ERP-linked items, skipping order creation",
			zap.String("cart_id", cart.ID),
			zap.Int("cart_items_total", len(items)),
		)
		return nil
	}

	logger.From(ctx, s.logger).Info("ERP order items prepared",
		zap.String("cart_id", cart.ID),
		zap.Int("erp_items", len(erpItems)),
		zap.Int("cart_items_total", len(items)),
		zap.Int64("items_total_cents", totalAmount),
		zap.String("contact_id", contactID),
	)

	order := providers.ERPOrder{
		ExternalID:  cart.ID,
		ContactID:   contactID,
		Items:       erpItems,
		TotalAmount: totalAmount,
		Observation: fmt.Sprintf("LiveCart - Evento %s - @%s", eventID, cart.PlatformHandle),
	}
	order.Approve = approve

	// Attach the delivery address from the cart when the customer submitted one.
	if len(cart.ShippingAddress) > 0 {
		var addr struct {
			ZipCode      string `json:"zipCode"`
			Street       string `json:"street"`
			Number       string `json:"number"`
			Complement   string `json:"complement"`
			Neighborhood string `json:"neighborhood"`
			City         string `json:"city"`
			State        string `json:"state"`
		}
		if err := json.Unmarshal(cart.ShippingAddress, &addr); err == nil && addr.Street != "" {
			order.ShippingAddress = &providers.ERPShippingAddress{
				RecipientName: cart.CustomerName,
				Document:      cart.CustomerDocument,
				Phone:         cart.CustomerPhone,
				ZipCode:       addr.ZipCode,
				Street:        addr.Street,
				Number:        addr.Number,
				Complement:    addr.Complement,
				Neighborhood:  addr.Neighborhood,
				City:          addr.City,
				State:         addr.State,
			}
		} else if err != nil {
			logger.From(ctx, s.logger).Warn("failed to parse cart shipping_address",
				zap.String("cart_id", cart.ID),
				zap.Error(err),
			)
		}
	}

	// Attach the chosen shipping option (carrier + service + real cost) so the
	// ERP records the shipment alongside the sales order. Use the real cost
	// (merchant visibility) even when the event applied free shipping to the
	// customer, so the ERP order reflects the actual freight expense.
	if cart.ShippingServiceName != "" {
		order.Shipping = &providers.ERPOrderShipping{
			Carrier:      cart.ShippingCarrier,
			Service:      cart.ShippingServiceName,
			CostCents:    cart.ShippingRealCost,
			DeadlineDays: cart.ShippingDeadline,
		}
	}

	// Flag the order as already paid using the provider-reported details.
	if paymentStatus != nil {
		paidAt := time.Now()
		if paymentStatus.PaidAt != nil {
			paidAt = *paymentStatus.PaidAt
		}
		order.Payment = &providers.ERPOrderPayment{
			Method:           paymentStatus.PaymentMethod,
			PaymentID:        paymentStatus.PaymentID,
			Installments:     paymentStatus.Installments,
			PaidAt:           paidAt,
			Amount:           totalAmount,
			MoneyReleaseDate: paymentStatus.MoneyReleaseDate,
			FeeAmountCents:   paymentStatus.FeeAmountCents,
			NetAmountCents:   paymentStatus.NetAmountCents,
		}

		// Snapshot of the financial breakdown right before we hand the
		// order to the ERP adapter. This is the single point that
		// connects "what the gateway told us" to "what we asked the ERP
		// to record" — if the two ever drift (rounding, fee changes,
		// refund), the diff shows up between this log and the
		// `tiny CreateOrder sending payload` log that follows.
		logger.From(ctx, s.logger).Info("ERP order payment snapshot prepared",
			zap.String("cart_id", cart.ID),
			zap.String("payment_id", paymentStatus.PaymentID),
			zap.String("payment_method", paymentStatus.PaymentMethod),
			zap.String("payment_status", string(paymentStatus.Status)),
			zap.Int("installments", paymentStatus.Installments),
			zap.Int64("order_total_cents", totalAmount),
			zap.Int64("paid_amount_cents", paymentStatus.Amount),
			zap.Int64("fee_amount_cents", paymentStatus.FeeAmountCents),
			zap.Int64("net_amount_cents", paymentStatus.NetAmountCents),
			zap.Bool("has_money_release_date", paymentStatus.MoneyReleaseDate != nil),
		)
	}

	result, err := erpProvider.CreateOrder(ctx, order)
	if err != nil {
		return fmt.Errorf("creating ERP order: %w", err)
	}

	// Save external order ID on cart first — ensures idempotency if we retry
	if err := s.repo.UpdateCartExternalOrderID(ctx, cart.ID, result.OrderID); err != nil {
		return fmt.Errorf("saving external order ID: %w", err)
	}

	// Group G fact (best-effort): the order now exists in the ERP. This is the
	// single point that creates it (both legacy and Design C conversion paths
	// funnel through here). Dedup by the ERP external order id.
	_ = events.EmitInternal(ctx, s.repo.queries, events.ERPOrderCreated, "erp.order_created:"+result.OrderID, struct {
		StoreID         string `json:"store_id"`
		CartID          string `json:"cart_id"`
		ExternalOrderID string `json:"external_order_id"`
		Provider        string `json:"provider"`
	}{StoreID: storeID, CartID: cart.ID, ExternalOrderID: result.OrderID, Provider: "tiny"})

	// Launch stock (permanent decrement). O fluxo invertido (Fase 3) passa
	// launchStock=false e orquestra o launch no caller, com fallback próprio.
	if launchStock {
		if err := erpProvider.LaunchOrderStock(ctx, result.OrderID); err != nil {
			return fmt.Errorf("launching stock for order %s: %w", result.OrderID, err)
		}
	}

	logFields := []zap.Field{
		zap.String("cart_id", cart.ID),
		zap.String("erp_order_id", result.OrderID),
		zap.Int("items", len(erpItems)),
	}
	// paymentStatus é nil na conversão pré-pagamento do design C (pedido
	// nasce Aberta, sem parcelas).
	if paymentStatus != nil {
		logFields = append(logFields,
			zap.String("payment_id", paymentStatus.PaymentID),
			zap.String("payment_method", paymentStatus.PaymentMethod),
			zap.Int64("fee_amount_cents", paymentStatus.FeeAmountCents),
			zap.Int64("net_amount_cents", paymentStatus.NetAmountCents),
		)
	}
	logger.From(ctx, s.logger).Info("ERP order created for cart", logFields...)

	return nil
}

// =============================================================================
// ERP HELPERS
// =============================================================================

// resolveERPContact finds or creates an ERP contact for the platform user.
// When the cart carries checkout customer data (name, document, email, phone)
// the contact is enriched on every paid order — without this, a contact
// created earlier under the Instagram @handle stays with the @handle as its
// nome forever and Tiny orders display the handle instead of the real name.
func (s *Service) resolveERPContact(ctx context.Context, erpProvider providers.ERPProvider, integration *IntegrationRow, storeID, platformUserID, platformHandle, customerName, customerDocument, customerEmail, customerPhone string) (string, error) {
	enrich := providers.ERPContactInput{
		Name:       customerName,
		CpfCnpj:    customerDocument,
		Email:      customerEmail,
		Phone:      customerPhone,
		PersonType: "F",
	}

	// Check cache first
	cachedID, err := s.repo.GetERPContact(ctx, storeID, integration.ID, platformUserID)
	if err != nil {
		return "", err
	}
	if cachedID != "" {
		logger.From(ctx, s.logger).Info("ERP contact resolved from cache",
			zap.String("contact_id", cachedID),
			zap.String("platform_user_id", platformUserID),
		)
		s.bestEffortUpdateContact(ctx, erpProvider, cachedID, enrich)
		return cachedID, nil
	}

	// Search by document — most reliable key in Tiny.
	if customerDocument != "" {
		results, err := erpProvider.SearchContacts(ctx, providers.SearchContactsParams{
			CpfCnpj: customerDocument,
		})
		if err == nil && len(results) > 0 {
			logger.From(ctx, s.logger).Info("ERP contact resolved by document",
				zap.String("contact_id", results[0].ContactID),
				zap.String("platform_user_id", platformUserID),
			)
			_ = s.repo.UpsertERPContact(ctx, storeID, integration.ID, platformUserID, platformHandle, results[0].ContactID)
			s.bestEffortUpdateContact(ctx, erpProvider, results[0].ContactID, enrich)
			return results[0].ContactID, nil
		}
	}

	// Note: previously fell back to SearchContacts(Name: platformHandle).
	// That found contacts left over from prior orders whose nome was the
	// @handle and reused them — masking the customerName the buyer typed at
	// checkout. Removed: when there's no document match we just create a
	// fresh contact so the new order carries the real name.

	// Create new contact in ERP. Prefer the real customer name over the handle.
	contactName := customerName
	if contactName == "" {
		contactName = platformHandle
	}
	contact, err := erpProvider.CreateContact(ctx, providers.ERPContactInput{
		Name:       contactName,
		CpfCnpj:    customerDocument,
		Email:      customerEmail,
		Phone:      customerPhone,
		PersonType: "F",
	})
	if err != nil {
		return "", fmt.Errorf("creating ERP contact: %w", err)
	}

	logger.From(ctx, s.logger).Info("ERP contact created",
		zap.String("contact_id", contact.ContactID),
		zap.String("platform_user_id", platformUserID),
		zap.String("contact_name", contactName),
	)

	// Cache
	_ = s.repo.UpsertERPContact(ctx, storeID, integration.ID, platformUserID, platformHandle, contact.ContactID)
	return contact.ContactID, nil
}

// bestEffortUpdateContact pushes the latest checkout customer data into a
// long-lived ERP contact. Skipped when we have nothing useful to send. Errors
// are logged and swallowed — the order must still go through even if the
// enrichment call fails.
func (s *Service) bestEffortUpdateContact(ctx context.Context, erpProvider providers.ERPProvider, contactID string, contact providers.ERPContactInput) {
	if contact.Name == "" && contact.CpfCnpj == "" && contact.Email == "" && contact.Phone == "" {
		return
	}
	if err := erpProvider.UpdateContact(ctx, contactID, contact); err != nil {
		logger.From(ctx, s.logger).Warn("failed to enrich ERP contact, proceeding with order anyway",
			zap.String("contact_id", contactID),
			zap.Error(err),
		)
	}
}

// getERPProvider gets the ERP provider from an integration row.
func (s *Service) getERPProvider(ctx context.Context, integration *IntegrationRow) (providers.ERPProvider, error) {
	provider, err := s.createProviderFromRow(ctx, integration)
	if err != nil {
		return nil, err
	}
	erpProvider, ok := provider.(providers.ERPProvider)
	if !ok {
		return nil, fmt.Errorf("integration %s is not an ERP provider", integration.ID)
	}
	return erpProvider, nil
}

// =============================================================================
// LAZY EXPIRATION & WAITLIST PROCESSING
// =============================================================================
//
// O núcleo concorrente da waitlist (ExpireCart, ProcessWaitlistForProduct, o
// sweep de 'notified' e o backstop do webhook de estoque) migrou para
// internal/inventory (Bloco B3b). Os métodos públicos abaixo permanecem como
// delegações finas (assinatura preservada) para os call sites de checkout/
// webhook/testes; o advisory lock, o gate atômico e a ordem de operações que
// seguram a corrida vivem agora na inventory.Service. ScheduleExpiry/
// RunScheduledExpiry (a ponte asynq) e cartExpiryTerminal ficam aqui.

// CartExpiryScheduler arms an ETA task that fires a cart's expiry at its
// expires_at, so expiration is precise instead of waiting on the 5-min sweep.
// Implemented over the asynq client in main.go (events package must not import
// domain packages). The sweep remains as a safety net for any lost task.
type CartExpiryScheduler interface {
	ScheduleCartExpiry(ctx context.Context, cartID string, at time.Time) error
	// RescheduleCartExpiry MOVE um agendamento já armado. Existe separado
	// porque ScheduleCartExpiry é deduplicado por TaskID: enquanto a task
	// pendente existir, um novo agendamento é engolido como "já armado" e o
	// horário novo é ignorado — em qualquer direção. Sem isto, a extensão de
	// prazo da fila (RN-10) nunca chegava ao asynq.
	RescheduleCartExpiry(ctx context.Context, cartID string, at time.Time) error
}

// SetCartExpiryScheduler wires the ETA scheduler (optional — when unset, only
// the sweep expires carts, preserving today's behaviour).
func (s *Service) SetCartExpiryScheduler(sch CartExpiryScheduler) { s.expiryScheduler = sch }

// ScheduleExpiry ARMA a task cart.expire no expires_at atual do carrinho. Usar
// quando o prazo acabou de nascer (fechamento do evento, regeneração de
// checkout) — se já houver task armada, o agendamento é deduplicado.
func (s *Service) ScheduleExpiry(ctx context.Context, cartID string) error {
	return s.scheduleExpiry(ctx, cartID, false)
}

// RescheduleExpiry MOVE a task cart.expire para o expires_at atual. Usar quando
// o prazo MUDOU depois de já ter sido armado (extensão da fila, RN-10). Arm e
// move não são a mesma operação no asynq: enquanto a task pendente existir, um
// arm é engolido como "já armado" e o horário novo é perdido.
func (s *Service) RescheduleExpiry(ctx context.Context, cartID string) error {
	return s.scheduleExpiry(ctx, cartID, true)
}

// scheduleExpiry lê o snapshot e delega. Best-effort: falha ou task perdida não
// deixa o carrinho eterno enquanto houver quem re-arme. Ignora carrinho
// terminal ou sem janela.
func (s *Service) scheduleExpiry(ctx context.Context, cartID string, move bool) error {
	if s.expiryScheduler == nil {
		return nil
	}
	snap, err := s.repo.GetCartExpirySnapshot(ctx, cartID)
	if err != nil || snap == nil {
		return err
	}
	if snap.ExpiresAt == nil || cartExpiryTerminal(snap) {
		return nil
	}
	if move {
		return s.expiryScheduler.RescheduleCartExpiry(ctx, cartID, *snap.ExpiresAt)
	}
	return s.expiryScheduler.ScheduleCartExpiry(ctx, cartID, *snap.ExpiresAt)
}

// RunScheduledExpiry is the cart.expire task handler. It is guard-first against
// the window-extension case (waitlist promotion pushes expires_at out): if the
// cart is now terminal it is a no-op; if the window moved into the future it
// re-arms instead of expiring; otherwise it runs the same guarded ExpireCart as
// the sweep. Idempotent — safe under asynq at-least-once redelivery.
func (s *Service) RunScheduledExpiry(ctx context.Context, cartID string) error {
	snap, err := s.repo.GetCartExpirySnapshot(ctx, cartID)
	if err != nil {
		return err
	}
	if snap == nil || snap.ExpiresAt == nil || cartExpiryTerminal(snap) {
		return nil
	}
	if snap.ExpiresAt.After(time.Now().UTC()) {
		// Window extended after the task was armed — re-arm for the new time.
		if s.expiryScheduler != nil {
			return s.expiryScheduler.ScheduleCartExpiry(ctx, cartID, *snap.ExpiresAt)
		}
		return nil
	}
	s.ExpireCart(ctx, cartID, snap.StoreID)
	return nil
}

// WaitlistCloseScheduler arma a task ETA que mata a fila não atendida de um
// evento em "fim do evento + carência" (RN-32). Implementada sobre o cliente
// asynq em main.go — o pacote events não pode importar domínio.
type WaitlistCloseScheduler interface {
	ScheduleEventWaitlistClose(ctx context.Context, eventID string, at time.Time) error
}

// SetWaitlistCloseScheduler liga o agendador da RN-32 (opcional — sem ele a
// fila não atendida simplesmente não é encerrada, que é o comportamento de
// antes desta fatia).
func (s *Service) SetWaitlistCloseScheduler(sch WaitlistCloseScheduler) { s.waitlistCloseSched = sch }

// ArmEventWaitlistClose é o reator de event.ended: agenda a morte da fila não
// atendida para "fim do evento + carência".
//
// A carência é o MESMO prazo que os carrinhos acabaram de receber no
// fechamento (RN-34: curto ou estendido conforme close_cart_on_event_end).
// Matar a fila antes disso tiraria do comprador um tempo que ele ainda tinha;
// matar depois deixaria o carrinho vivo além do próprio prazo.
func (s *Service) ArmEventWaitlistClose(ctx context.Context, eventID string) error {
	if s.waitlistCloseSched == nil {
		return nil
	}
	minutes, err := s.repo.GetEventCartExpirationMinutes(ctx, eventID)
	if err != nil {
		return err
	}
	if minutes <= 0 {
		// A 000106 impede isso (CHECK >= 15 nas duas pontas). Se acontecer,
		// não agendar seria voltar ao carrinho eterno — 24h é o mesmo piso que
		// a migration usou ao converter o antigo 0.
		minutes = 1440
	}
	at := time.Now().UTC().Add(time.Duration(minutes) * time.Minute)
	return s.waitlistCloseSched.ScheduleEventWaitlistClose(ctx, eventID, at)
}

// RunEventWaitlistClose é o handler da task event.waitlist_close (RN-32).
//
// Encerra os itens de fila não atendidos do evento e RE-ARMA cart.expire nos
// carrinhos que estavam bloqueados. O re-arm é a metade que faz a regra valer:
// o prazo desses carrinhos já venceu enquanto o guard do ExpireCart vetava, e
// a task original deles já disparou e saiu sem fazer nada — sem sweep de
// carrinhos, nada mais os alcançaria.
//
// Idempotente: a segunda passada não encontra item vivo e não re-arma nada.
func (s *Service) RunEventWaitlistClose(ctx context.Context, eventID string) error {
	entries, err := s.repo.ExpireEventWaitlist(ctx, eventID)
	if err != nil {
		return err
	}
	if len(entries) == 0 {
		return nil
	}

	// Um carrinho pode ter vários itens na fila: dedup para re-armar cart.expire
	// uma vez só por carrinho.
	cartIDs := make([]string, 0, len(entries))
	seen := make(map[string]struct{}, len(entries))
	for _, e := range entries {
		if e.CartID == "" {
			continue // item de fila sem carrinho vinculado
		}
		if _, dup := seen[e.CartID]; dup {
			continue
		}
		seen[e.CartID] = struct{}{}
		cartIDs = append(cartIDs, e.CartID)
	}

	logger.From(ctx, s.logger).Info("event waitlist closed",
		zap.String("event_id", eventID),
		zap.Int("items_expired", len(entries)),
		zap.Int("carts_unblocked", len(cartIDs)),
	)

	// A DM vem ANTES do re-arm de propósito. O re-arm devolve ao carrinho a
	// capacidade de expirar, e o prazo dele já venceu enquanto o guard vetava —
	// ou seja, a expiração pode disparar em seguida. Avisar depois seria contar
	// ao comprador que o item não liberou num carrinho que já morreu.
	s.notifyWaitlistUnfulfilled(ctx, eventID, entries)

	for _, cartID := range cartIDs {
		if err := s.RescheduleExpiry(ctx, cartID); err != nil {
			// Best-effort por carrinho: um erro não pode impedir os outros de
			// voltarem a poder expirar.
			logger.From(ctx, s.logger).Warn("failed to re-arm expiry after waitlist close",
				zap.String("cart_id", cartID), zap.Error(err))
		}
	}
	return nil
}

// notifyWaitlistUnfulfilled avisa quem esperava na fila que o produto não
// liberou até o fim da campanha (RN-28, gatilho 5).
//
// Uma DM por (comprador, produto): o comprador que esperava duas peças precisa
// saber quais duas. O texto padrão sempre aponta para o que ainda existe (o
// resto do carrinho) — é a única mensagem do deck que dá notícia ruim, e
// terminar no "não deu" perde a venda que sobrou.
func (s *Service) notifyWaitlistUnfulfilled(ctx context.Context, eventID string, entries []ExpiredWaitlistEntry) {
	if s.notificationService == nil || len(entries) == 0 {
		return
	}

	storeID, eventTitle, err := s.repo.GetEventOwner(ctx, eventID)
	if err != nil || storeID == "" {
		logger.From(ctx, s.logger).Warn("waitlist close: could not resolve event for notification",
			zap.String("event_id", eventID), zap.Error(err))
		return
	}

	shouldNotify, err := s.notificationService.ShouldNotify(ctx, storeID, notification.TypeWaitlistUnfulfilled, false)
	if err != nil || !shouldNotify {
		return
	}

	storeName := ""
	if storeInfo, err := s.repo.GetStoreInfo(ctx, storeID); err == nil {
		storeName = storeInfo.Name
	}
	frontendURL := config.FrontendURL.StringOr("http://localhost:3000")

	for _, e := range entries {
		vars := notification.TemplateVariables{
			Handle:     "@" + e.PlatformHandle,
			Produto:    e.ProductName,
			Loja:       storeName,
			LiveTitulo: eventTitle,
		}
		if e.CartToken != "" {
			vars.Link = fmt.Sprintf("%s/cart/%s", frontendURL, e.CartToken)
		}

		// A entrega precisa do comentário: a fila fecha por task, dias depois,
		// e um DM por IGSID sem janela aberta é recusado pelo Instagram
		// (2534022). Sem o alvo, o gatilho nasceria sem caminho de entrega e
		// todo envio viraria linha "não entregue". A idade vai junto para
		// classificar o motivo quando de fato não houver mais janela.
		target, _ := s.liveService.GetLatestReplyTarget(ctx, eventID, e.PlatformUserID)
		commentAt := time.Time{}
		if target.CreatedAt != nil {
			commentAt = *target.CreatedAt
		}

		if _, err := s.notificationService.Send(ctx, notification.SendInput{
			StoreID:           storeID,
			EventID:           eventID,
			CartID:            e.CartID,
			CartToken:         e.CartToken,
			PlatformUserID:    e.PlatformUserID,
			PlatformHandle:    e.PlatformHandle,
			PlatformCommentID: target.CommentID,
			NotificationType:  notification.TypeWaitlistUnfulfilled,
			Variables:         vars,
			CommentCreatedAt:  commentAt,
		}); err != nil {
			logger.From(ctx, s.logger).Warn("waitlist unfulfilled notification error",
				zap.String("event_id", eventID),
				zap.String("platform_user_id", e.PlatformUserID),
				zap.Error(err))
		}
	}
}

// cartExpiryTerminal reports whether a cart is already in a state where expiry
// must not run (paid/refunded or already expired/cancelled).
func cartExpiryTerminal(s *CartExpirySnapshot) bool {
	return s.Status == "expired" || s.Status == "cancelled" ||
		s.PaymentStatus == "paid" || s.PaymentStatus == "refunded"
}

// ExpireCart delega para inventory.Service (Bloco B3b — o núcleo concorrente vive
// em internal/inventory). Assinatura pública preservada: RunScheduledExpiry, o
// sweep e os testes de corrida continuam chamando integration.Service.
func (s *Service) ExpireCart(ctx context.Context, cartID, storeID string) {
	s.inventory().ExpireCart(ctx, cartID, storeID)
}

// ProcessExpiredCartsForProduct handles expired carts that contain the given product.
// DEPRECATED: substituída pela expiração por-cart (schedule cart.expire → ExpireCart).
// Não é mais chamada em produção — o único caller remanescente é teste. Mantida
// temporariamente para não quebrar o teste de resume; não usar em código novo.
func (s *Service) ProcessExpiredCartsForProduct(ctx context.Context, eventID, productID string) {
	carts, err := s.repo.ListExpiredCartsByEventAndProduct(ctx, eventID, productID)
	if err != nil {
		logger.From(ctx, s.logger).Error("failed to list expired carts", zap.Error(err))
		return
	}

	for _, cart := range carts {
		// Mark cart as expired
		if err := s.repo.UpdateCartStatus(ctx, cart.ID, "expired"); err != nil {
			logger.From(ctx, s.logger).Error("failed to expire cart", zap.String("cart_id", cart.ID), zap.Error(err))
			continue
		}

		// Cart convertido (design C): quem devolve o estoque é o cancelamento
		// do PEDIDO (situação 2 → estorno incondicional). As saídas manuais já
		// foram estornadas na conversão — o branch de reservas abaixo no-opa.
		if st, stErr := s.repo.GetCartERPOrderState(ctx, cart.ID); stErr == nil &&
			st.State != erpOrderStateNone && st.State != erpOrderStateCancelled {
			if cancelErr := s.CancelERPOrderForCart(ctx, cart.ID, cart.StoreID); cancelErr != nil {
				logger.From(ctx, s.logger).Error("failed to cancel converted cart order on expiry",
					zap.String("cart_id", cart.ID),
					zap.Error(cancelErr),
				)
			}
		}

		// Release stock back to product — the real non-waitlisted quantity of
		// the item, not 1: o add da live decrementou (quantity -
		// waitlisted_quantity) unidades e é isso que a expiração devolve.
		// Antes deste fix o valor era hardcoded em 1 e o local sub-contava
		// para itens com qty>1, "consertado" por acaso pelo overwrite do
		// webhook — que o guard novo passou a segurar.
		releaseQty, qtyErr := s.repo.GetCartItemAvailableQty(ctx, cart.ID, productID)
		if qtyErr != nil || releaseQty < 1 {
			if qtyErr != nil {
				logger.From(ctx, s.logger).Warn("failed to read cart item quantity for expiry release, falling back to 1",
					zap.String("cart_id", cart.ID),
					zap.String("product_id", productID),
					zap.Error(qtyErr),
				)
			}
			releaseQty = 1
		}
		if err := s.repo.IncrementProductStock(ctx, productID, releaseQty); err != nil {
			logger.From(ctx, s.logger).Error("failed to release stock", zap.String("product_id", productID), zap.Error(err))
		}

		// Reverse ERP stock reservations for this cart+product
		reservations, resErr := s.repo.ListActiveReservationsByCartAndProduct(ctx, cart.ID, productID)
		if resErr != nil {
			logger.From(ctx, s.logger).Error("failed to list reservations for expired cart",
				zap.String("cart_id", cart.ID),
				zap.String("product_id", productID),
				zap.Error(resErr),
			)
		}
		if len(reservations) > 0 {
			erpReversed := false
			integration, intErr := s.repo.GetActiveByProvider(ctx, cart.StoreID, "erp", "tiny")
			if intErr != nil {
				logger.From(ctx, s.logger).Warn("no active ERP integration for expired cart reversal, marking reservations as reversed locally only",
					zap.String("store_id", cart.StoreID),
				)
			} else {
				erpProvider, provErr := s.erpProviderFor(ctx, integration)
				if provErr != nil {
					logger.From(ctx, s.logger).Error("failed to create ERP provider for expired cart reversal",
						zap.String("cart_id", cart.ID),
						zap.Error(provErr),
					)
				} else {
					// Reivindica antes de chamar o ERP — ver
					// erp.ReverseReservationsClaimFirst. A marcação em bloco
					// logo abaixo (ReverseReservationsByCartAndProduct) não
					// protegia nada: ela roda DEPOIS do laço, então uma
					// retentativa que chegasse no meio reenviava as entradas já
					// aplicadas.
					rows := make([]erp.ReversibleReservation, 0, len(reservations))
					for _, res := range reservations {
						rows = append(rows, erp.ReversibleReservation{
							ID:                res.ID,
							ExternalProductID: res.ExternalProductID,
							Quantity:          res.Quantity,
							CartID:            res.CartID,
							EventID:           res.EventID,
							ProductID:         res.ProductID,
						})
					}
					_, erpReversed = erp.ReverseReservationsClaimFirst(ctx, s.logger, s.repo, erpProvider, rows,
						func(erp.ReversibleReservation) string {
							return fmt.Sprintf("Estorno expiração carrinho LiveCart - Cart %s", cart.ID)
						}, s.erpStock().ReversalLedgerHooks(cart.StoreID))
				}
			}
			if markErr := s.repo.ReverseReservationsByCartAndProduct(ctx, cart.ID, productID); markErr != nil {
				logger.From(ctx, s.logger).Error("failed to mark reservations as reversed",
					zap.String("cart_id", cart.ID),
					zap.String("product_id", productID),
					zap.Error(markErr),
				)
			}
			if !erpReversed {
				logger.From(ctx, s.logger).Warn("ERP stock reservations NOT reversed for expired cart — manual reconciliation may be needed",
					zap.String("cart_id", cart.ID),
					zap.String("product_id", productID),
				)
			}
		}

		logger.From(ctx, s.logger).Info("expired cart processed",
			zap.String("cart_id", cart.ID),
			zap.String("product_id", productID),
		)
	}

	// Após liberar todo o estoque dos carts expirados, tenta promover o
	// próximo da fila. Idempotente: se ninguém está esperando ou se o
	// stock zerou no meio do caminho, ProcessWaitlistForProduct no-ops.
	if len(carts) > 0 {
		// fire-and-forget: o release acima já completou e os logs vão
		// indicar se a promoção rodou; não faz sentido bloquear o caller.
		s.ProcessWaitlistForProduct(ctx, eventID, productID, carts[0].StoreID)
	}
}

// CancelOpenCartsForBlockedHandle is invoked by the customer service after a
// handle is blocked: it sweeps every non-paid cart of that handle in the
// store, releases local + ERP stock for each item, marks the carts as
// cancelled with reason='customer_blocked', then promotes the waitlist for
// each freed product.
//
// All errors are logged; the function never fails hard — the block row is
// already persisted by the caller, so even if some cleanup fails the block
// itself is in effect (future comments are filtered).
// ActivateVipCartsForHandle é o efeito colateral de promover um @ a VIP: os
// carrinhos abertos que ele JÁ tem viram eternos (never_expires=true) e a
// agenda de expiração é anulada (expires_at NULL). Nenhuma task cart.expire
// precisa ser cancelada explicitamente — com expires_at NULL o guard do
// ExpireCart/RunScheduledExpiry já a torna no-op. Devolve quantos carrinhos
// foram convertidos. Satisfaz customer.VipCartActivator.
func (s *Service) ActivateVipCartsForHandle(ctx context.Context, storeID, handle string) (int, error) {
	ids, err := s.repo.ActivateEternalCartsForHandle(ctx, storeID, handle)
	if err != nil {
		return 0, fmt.Errorf("activating eternal carts for vip handle: %w", err)
	}
	if len(ids) > 0 {
		logger.From(ctx, s.logger).Info("vip promotion made existing carts eternal",
			zap.String("store_id", storeID),
			zap.String("handle", handle),
			zap.Int("carts", len(ids)),
		)
	}
	return len(ids), nil
}

func (s *Service) CancelOpenCartsForBlockedHandle(ctx context.Context, storeID, handle string) error {
	carts, err := s.repo.ListOpenCartsByHandle(ctx, storeID, handle)
	if err != nil {
		return fmt.Errorf("listing open carts for blocked handle: %w", err)
	}
	if len(carts) == 0 {
		return nil
	}

	type freed struct {
		eventID, productID string
	}
	freedKeys := make(map[freed]bool)

	// Resolve the ERP integration once — same one applies to every cart in
	// this store. nil is OK: handle the no-ERP case by skipping the remote
	// reversal step and only flipping the local DB.
	var erpProvider providers.ERPProvider
	if integration, intErr := s.repo.GetActiveByProvider(ctx, storeID, "erp", "tiny"); intErr == nil {
		if provider, provErr := s.erpProviderFor(ctx, integration); provErr == nil {
			erpProvider = provider
		} else {
			logger.From(ctx, s.logger).Warn("failed to build ERP provider for block sweep",
				zap.String("store_id", storeID),
				zap.Error(provErr),
			)
		}
	}

	for _, cart := range carts {
		items, listErr := s.repo.ListNonWaitlistedCartItems(ctx, cart.ID)
		if listErr != nil {
			logger.From(ctx, s.logger).Error("failed to list cart items for blocked cart",
				zap.String("cart_id", cart.ID),
				zap.Error(listErr),
			)
			continue
		}

		// 1. Release local stock per item (waitlisted items don't hold stock
		//    so we ignore them — they auto-cancel when the cart is killed).
		for _, item := range items {
			if err := s.stock.Release(ctx, ReleaseParams{Op: StockOpCancelBlocked, ProductID: item.ProductID, Quantity: item.Quantity, CartID: cart.ID, EventID: cart.EventID}); err != nil {
				logger.From(ctx, s.logger).Error("failed to release local stock for blocked cart",
					zap.String("cart_id", cart.ID),
					zap.String("product_id", item.ProductID),
					zap.Error(err),
				)
			}
			freedKeys[freed{eventID: cart.EventID, productID: item.ProductID}] = true
		}

		// 2. Reverse ERP stock reservations (saída-manual) so Tiny gets its
		//    inventory back. Best-effort: if the ERP call fails we still mark
		//    the DB rows reversed and surface a warning for manual reconciliation.
		reservations, resErr := s.repo.ListActiveReservationsByCart(ctx, cart.ID)
		if resErr != nil {
			logger.From(ctx, s.logger).Error("failed to list reservations for blocked cart",
				zap.String("cart_id", cart.ID),
				zap.Error(resErr),
			)
		}
		if len(reservations) > 0 && erpProvider != nil {
			// Reivindica antes de chamar o ERP — ver
			// erp.ReverseReservationsClaimFirst.
			rows := make([]erp.ReversibleReservation, 0, len(reservations))
			for _, r := range reservations {
				rows = append(rows, erp.ReversibleReservation{
					ID:                r.ID,
					ExternalProductID: r.ExternalProductID,
					Quantity:          r.Quantity,
					CartID:            r.CartID,
					EventID:           r.EventID,
					ProductID:         r.ProductID,
				})
			}
			erp.ReverseReservationsClaimFirst(ctx, s.logger, s.repo, erpProvider, rows,
				func(erp.ReversibleReservation) string {
					return fmt.Sprintf("Estorno cliente bloqueado - Cart %s", cart.ID)
				}, s.erpStock().ReversalLedgerHooks(storeID))
		}
		if len(reservations) > 0 {
			if err := s.repo.ReverseReservationsByCart(ctx, cart.ID); err != nil {
				logger.From(ctx, s.logger).Error("failed to mark reservations reversed for blocked cart",
					zap.String("cart_id", cart.ID),
					zap.Error(err),
				)
			}
		}

		// 3. Mark cart as cancelled (no-op if it was paid in the gap).
		if err := s.repo.CancelCartAsBlocked(ctx, cart.ID); err != nil {
			logger.From(ctx, s.logger).Error("failed to cancel cart as blocked",
				zap.String("cart_id", cart.ID),
				zap.Error(err),
			)
			continue
		}

		logger.From(ctx, s.logger).Info("cart cancelled by customer block",
			zap.String("cart_id", cart.ID),
			zap.String("handle", handle),
			zap.Int("items_released", len(items)),
		)
	}

	// 4. Try to promote the next person in line for each freed product.
	//    Fire-and-forget within this request — same pattern as cart-expiration.
	for key := range freedKeys {
		s.ProcessWaitlistForProduct(ctx, key.eventID, key.productID, storeID)
	}

	return nil
}

// ProcessWaitlistForProduct delega para inventory.Service (Bloco B3b — o núcleo
// concorrente de promoção da fila vive em internal/inventory). Assinatura pública
// preservada: checkout, expiração e o backstop do webhook continuam chamando
// integration.Service. (acquireCartFinalisationLockRetry moveu junto para lá.)
func (s *Service) ProcessWaitlistForProduct(ctx context.Context, eventID, productID, storeID string) {
	s.inventory().ProcessWaitlistForProduct(ctx, eventID, productID, storeID)
}

// NotifyWaitlistPromoted satisfies inventory.WaitlistCollaborators and does
// nothing on purpose: a promoção da fila NÃO avisa o comprador.
//
// Não é omissão nossa, é regra do Instagram. A janela de 24h para mandar DM só
// abre quando o COMPRADOR manda mensagem para a conta; comentar não abre —
// comentário concede um private reply, permissão distinta e de uso único, já
// gasta no aviso de entrada na fila. Quando o estoque volta não existe
// comentário novo para responder, então sobrava o DM, e ele voltava 403
// "outside of allowed window" em 100% das tentativas (3 de 3 na live de 16/08).
//
// O item continua entrando no carrinho sozinho e o prazo continua sendo
// estendido — o que muda é a promessa: o texto da entrada na fila agora manda
// o comprador ficar de olho no carrinho, em vez de garantir um aviso que a
// plataforma não deixa entregar.
func (s *Service) NotifyWaitlistPromoted(context.Context, inventory.WaitlistNotifiedInput) {}

// CancelWaitlistForCartProduct mata a fila de um produto do carrinho. Chamada
// pelo checkout quando a redução de quantidade esvazia a parcela em fila: sem
// isso a linha fica órfã e a próxima promoção a reivindica, consumindo estoque
// para um comprador que já desistiu daquela parcela.
//
// Best-effort no chamador: falhar aqui não pode derrubar a alteração que o
// comprador acabou de fazer.
func (s *Service) CancelWaitlistForCartProduct(ctx context.Context, cartID, productID string) (int, error) {
	return s.repo.CancelWaitlistForCartProduct(ctx, cartID, productID)
}

// ListActiveWaitlistByCart delega para inventory.Service (Bloco B3a — a lógica
// vive em internal/inventory). Assinatura pública preservada: o checkout continua
// chamando integration.Service.
func (s *Service) ListActiveWaitlistByCart(ctx context.Context, cartID string) ([]ListActiveByCartRow, error) {
	return s.inventory().ListActiveWaitlistByCart(ctx, cartID)
}

// CancelWaitlistItem delega para inventory.Service (Bloco B3a). Assinatura
// pública preservada: o endpoint "sair da fila" continua chamando
// integration.Service.
func (s *Service) CancelWaitlistItem(ctx context.Context, waitlistItemID, cartID string) (bool, error) {
	return s.inventory().CancelWaitlistItem(ctx, waitlistItemID, cartID)
}

// ExpireNotifiedWaitlistItem delega para inventory.Service (Bloco B3b).
// Assinatura pública preservada.
func (s *Service) ExpireNotifiedWaitlistItem(ctx context.Context, item WaitlistItemRow) error {
	return s.inventory().ExpireNotifiedWaitlistItem(ctx, item)
}

// ExpireNotifiedWaitlistSweep delega para inventory.Service (Bloco B3b).
// Assinatura pública preservada: o checkout continua chamando integration.Service.
func (s *Service) ExpireNotifiedWaitlistSweep(ctx context.Context) (int, error) {
	return s.inventory().ExpireNotifiedWaitlistSweep(ctx)
}

// ProcessWaitlistAfterStockWebhook delega para inventory.Service (Bloco B3b).
// Assinatura pública preservada: o webhook handler continua chamando
// integration.Service.
func (s *Service) ProcessWaitlistAfterStockWebhook(ctx context.Context, storeID, externalSource, externalProductID string) error {
	return s.inventory().ProcessWaitlistAfterStockWebhook(ctx, storeID, externalSource, externalProductID)
}

// =============================================================================
// HELPERS
// =============================================================================

// UpdateERPStockSource escolhe qual saldo do ERP o LiveCart passa a espelhar.
//
// Invariante: só integração de ERP tem os dois saldos. Pagamento e frete não
// têm estoque nenhum, e aceitar a configuração neles gravaria uma chave que
// ninguém lê — configuração que não faz nada é pior que configuração ausente,
// porque o lojista acredita ter ligado alguma coisa.
//
// A escrita é um merge: o metadata carrega outras chaves (environment, dados de
// OAuth) e substituí-lo inteiro apagaria o resto.
// ERPResyncNotifier avisa a loja que a releitura em massa terminou.
//
// Interface aqui (e não import do notification_inbox) pelo mesmo motivo do
// idea.NotificationWriter: manter os módulos desacoplados e sem ciclo.
type ERPResyncNotifier interface {
	NotifyERPResyncFinished(ctx context.Context, storeID, provider string, synced, failed int) error
}

// ERPResyncScheduler enfileira a releitura em massa dos produtos de uma loja.
type ERPResyncScheduler interface {
	ScheduleERPResync(ctx context.Context, storeID, integrationID string) error
}

// erpResyncChunk é de quantos em quantos produtos a releitura respira.
//
// O limitador adaptativo já espaça as chamadas pelos headers do Tiny, então a
// pausa não existe para respeitar o limite — existe para não MONOPOLIZÁ-LO. Sem
// ela, uma releitura de 300 produtos ocupa a cota inteira e o webhook de estoque
// de uma live em andamento fica na fila atrás dela.
const erpResyncChunk = 25

// erpResyncBreath é quanto a releitura para entre um bloco e outro.
const erpResyncBreath = 2 * time.Second

// StartERPResync agenda a releitura e devolve quantos produtos entrarão nela.
//
// Existe porque os produtos foram importados quando o LiveCart só sabia ler o
// saldo FÍSICO do ERP. Ligar a configuração de saldo disponível muda o que as
// PRÓXIMAS sincronizações gravam, mas não reescreve o que já está no banco: sem
// isto, cada produto só se corrigiria quando o lojista mexesse nele no ERP.
func (s *Service) StartERPResync(ctx context.Context, input StartERPResyncInput) (int, error) {
	integration, err := s.repo.GetByID(ctx, input.IntegrationID, input.StoreID)
	if err != nil {
		return 0, err
	}
	if integration.Type != "erp" {
		return 0, httpx.DomainError(422, httpx.CodeErpStockSourceUnsupported,
			"só integrações de ERP têm produtos para sincronizar")
	}
	if s.erpResyncScheduler == nil {
		return 0, httpx.ErrUnprocessable("sincronização em massa não está configurada")
	}

	posicoes, err := s.repo.ListStockPositionsForReconciliation(ctx, input.StoreID, integration.Provider)
	if err != nil {
		return 0, err
	}
	if len(posicoes) == 0 {
		return 0, nil
	}

	if err := s.erpResyncScheduler.ScheduleERPResync(ctx, input.StoreID, integration.ID); err != nil {
		return 0, fmt.Errorf("scheduling ERP resync: %w", err)
	}
	s.markResyncRunning(ctx, integration, true)
	return len(posicoes), nil
}

// markResyncRunning liga/desliga a marca de varredura em andamento.
//
// Best-effort: falhar aqui não pode impedir a varredura de começar nem de
// terminar. O pior desfecho de uma marca presa é o botão ficar desabilitado até
// a guarda de obsolescência expirar — chato, e muito melhor que duas varreduras
// simultâneas sobre a mesma cota do ERP.
func (s *Service) markResyncRunning(ctx context.Context, integration *IntegrationRow, running bool) {
	metadata := integration.Metadata
	if metadata == nil {
		metadata = map[string]any{}
	}
	if running {
		metadata[providers.MetadataResyncRunningSince] = time.Now().UTC().Format(time.RFC3339)
	} else {
		delete(metadata, providers.MetadataResyncRunningSince)
		delete(metadata, providers.MetadataResyncDone)
		delete(metadata, providers.MetadataResyncTotal)
	}
	if err := s.repo.UpdateMetadata(ctx, integration.ID, metadata); err != nil {
		logger.From(ctx, s.logger).Warn("could not update the ERP resync marker",
			zap.String("integration_id", integration.ID),
			zap.Bool("running", running),
			zap.Error(err))
		return
	}
	integration.Metadata = metadata
}

// erpResyncProgressEvery é de quantos em quantos produtos o progresso é gravado.
//
// Não a cada produto: seriam 154 escritas no metadata numa varredura comum, para
// um número que ninguém consegue ler mudando a cada seis segundos. Não a cada
// bloco de 25: com o ritmo que o ERP permite, o contador ficaria parado por
// minutos e voltaria a parecer travado — que é o problema que ele existe para
// resolver.
const erpResyncProgressEvery = 5

// markResyncProgress grava "vai em X de N".
func (s *Service) markResyncProgress(ctx context.Context, integration *IntegrationRow, done, total int) {
	metadata := integration.Metadata
	if metadata == nil {
		metadata = map[string]any{}
	}
	metadata[providers.MetadataResyncDone] = done
	metadata[providers.MetadataResyncTotal] = total
	if err := s.repo.UpdateMetadata(ctx, integration.ID, metadata); err != nil {
		// Progresso é conforto, não correção: perder uma atualização atrasa o
		// número na tela e não muda nada do que foi gravado no estoque.
		logger.From(ctx, s.logger).Debug("could not update the ERP resync progress",
			zap.String("integration_id", integration.ID), zap.Error(err))
		return
	}
	integration.Metadata = metadata
}

// erpResyncRateLimitRetries é quantas vezes um produto estrangulado é reposto na
// fila antes de contar como falha.
const erpResyncRateLimitRetries = 4

// resyncOneProduct relê um produto, insistindo quando o ERP estrangula.
//
// Estrangulamento não é falha do produto: é o provedor pedindo para esperar, e
// ele libera em segundos. Contar como falha e seguir era o defeito da primeira
// versão — na varredura de 14/08 um produto ("Arranjo com Sino Listrado") levou
// 429 e foi PULADO, ficando com o saldo físico enquanto os outros 139 iam para o
// disponível. Uma varredura que existe para consertar o catálogo inteiro não
// pode deixar buraco silencioso por causa de uma pausa do provedor.
//
// A espera cresce a cada tentativa porque o `retry_after` do Tiny volta zerado —
// respeitá-lo ao pé da letra seria bater na mesma porta no mesmo instante.
func (s *Service) resyncOneProduct(ctx context.Context, integration *IntegrationRow, externalID string) error {
	var ultimo error
	for tentativa := 0; tentativa <= erpResyncRateLimitRetries; tentativa++ {
		if tentativa > 0 {
			espera := time.Duration(tentativa*tentativa) * time.Second
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(espera):
			}
		}
		_, err := s.processProductSync(ctx, integration, externalID)
		if err == nil {
			return nil
		}
		ultimo = err

		var estrangulado *ratelimit.ErrRateLimited
		if !errors.As(err, &estrangulado) {
			// Erro que não é pausa do provedor (produto apagado no ERP, resposta
			// ilegível): insistir não muda o desfecho.
			return err
		}
	}
	return ultimo
}

// RunERPResync percorre os produtos vinculados relendo cada um do ERP.
//
// Um produto que falha não derruba os outros: a releitura existe para consertar
// um catálogo inteiro, e parar no primeiro erro deixaria o resto do estoque
// errado por causa de um SKU que o lojista talvez tenha apagado no ERP.
func (s *Service) RunERPResync(ctx context.Context, storeID, integrationID string) error {
	integration, err := s.repo.GetByID(ctx, integrationID, storeID)
	if err != nil {
		return err
	}

	// A marca sai no fim, dê no que der. Se ficar presa, o botão do lojista fica
	// desabilitado até a guarda de obsolescência soltar — e `context.WithoutCancel`
	// porque a limpeza precisa acontecer mesmo quando a varredura morreu por
	// timeout, que é justamente quando a marca ficaria mais tempo pendurada.
	defer s.markResyncRunning(context.WithoutCancel(ctx), integration, false)

	posicoes, err := s.repo.ListStockPositionsForReconciliation(ctx, storeID, integration.Provider)
	if err != nil {
		return err
	}

	lg := logger.From(ctx, s.logger)
	lg.Info("ERP resync started",
		zap.String("store_id", storeID),
		zap.String("integration_id", integrationID),
		zap.Int("products", len(posicoes)),
	)

	total := len(posicoes)
	s.markResyncProgress(ctx, integration, 0, total)

	var ok, falhou int
	for i, pos := range posicoes {
		if i > 0 && i%erpResyncChunk == 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(erpResyncBreath):
			}
		}
		if err := s.resyncOneProduct(ctx, integration, pos.ExternalID); err != nil {
			falhou++
			lg.Warn("ERP resync: product failed",
				zap.String("external_product_id", pos.ExternalID),
				zap.String("name", pos.Name),
				zap.Error(err))
			continue
		}
		ok++

		if (i+1)%erpResyncProgressEvery == 0 {
			s.markResyncProgress(ctx, integration, i+1, total)
		}
	}

	lg.Info("ERP resync finished",
		zap.String("store_id", storeID),
		zap.Int("synced", ok),
		zap.Int("failed", falhou),
	)

	// O aviso é o fim do trabalho, não parte dele: falhar aqui não desfaz nada
	// que já foi gravado, e devolver erro faria a asynq repetir a varredura
	// inteira — gastando a cota do ERP de novo para reescrever os mesmos saldos.
	if s.erpResyncNotifier != nil {
		if err := s.erpResyncNotifier.NotifyERPResyncFinished(
			ctx, storeID, integration.Provider, ok, falhou,
		); err != nil {
			lg.Warn("ERP resync finished but the merchant was not notified",
				zap.String("store_id", storeID), zap.Error(err))
		}
	}
	return nil
}

func (s *Service) UpdateERPStockSource(ctx context.Context, input UpdateERPStockSourceInput) (*IntegrationRow, error) {
	row, err := s.repo.GetByID(ctx, input.IntegrationID, input.StoreID)
	if err != nil {
		return nil, err
	}
	if row.Type != "erp" {
		return nil, httpx.DomainError(422, httpx.CodeErpStockSourceUnsupported,
			"essa configuração existe apenas para integrações de ERP")
	}

	metadata := row.Metadata
	if metadata == nil {
		metadata = map[string]any{}
	}
	metadata[providers.MetadataUseAvailableStock] = input.UseAvailableStock

	if err := s.repo.UpdateMetadata(ctx, row.ID, metadata); err != nil {
		return nil, err
	}
	row.Metadata = metadata

	logger.From(ctx, s.logger).Info("ERP stock source changed",
		zap.String("integration_id", row.ID),
		zap.String("store_id", input.StoreID),
		zap.String("provider", row.Provider),
		zap.Bool("use_available_stock", input.UseAvailableStock),
	)
	return row, nil
}

func (s *Service) createProviderFromRow(ctx context.Context, integration *IntegrationRow) (providers.Provider, error) {
	// Decrypt credentials
	creds, err := s.decryptCredentials(integration.Credentials)
	if err != nil {
		return nil, fmt.Errorf("decrypting credentials: %w", err)
	}

	// Log credential expiration info for debugging (only for OAuth providers)
	if creds.AccessToken != "" && integration.Provider != "pagarme" {
		logger.From(ctx, s.logger).Debug("checking token expiration",
			zap.String("integration_id", integration.ID),
			zap.String("provider", integration.Provider),
			zap.Time("expires_at", creds.ExpiresAt),
			zap.Bool("expires_at_is_zero", creds.ExpiresAt.IsZero()),
			zap.Bool("is_expired", creds.IsExpired()),
			zap.Bool("has_refresh_token", creds.RefreshToken != ""),
		)
	}

	// Check if token needs refresh
	if creds.IsExpired() {
		logger.From(ctx, s.logger).Info("token expired, attempting refresh",
			zap.String("integration_id", integration.ID),
			zap.String("provider", integration.Provider),
			zap.Time("expires_at", creds.ExpiresAt),
		)
		creds, err = s.refreshToken(ctx, integration, creds)
		if err != nil {
			logger.From(ctx, s.logger).Warn("failed to refresh token",
				zap.String("integration_id", integration.ID),
				zap.Error(err),
			)
			// Continue with possibly expired credentials
			// The provider will fail if they're truly invalid
		}
	}

	return s.factory.CreateProvider(providers.ProviderConfig{
		IntegrationID: integration.ID,
		StoreID:       integration.StoreID,
		Type:          providers.ProviderType(integration.Type),
		Name:          providers.ProviderName(integration.Provider),
		Credentials:   creds,
		Metadata:      integration.Metadata,
	})
}

func (s *Service) decryptCredentials(encrypted []byte) (*providers.Credentials, error) {
	if encrypted == nil || len(encrypted) == 0 {
		return nil, httpx.ErrUnprocessable("no credentials found")
	}

	var creds providers.Credentials
	if err := s.encryptor.DecryptJSON(encrypted, &creds); err != nil {
		return nil, fmt.Errorf("decrypting credentials: %w", err)
	}

	return &creds, nil
}

func (s *Service) refreshToken(ctx context.Context, integration *IntegrationRow, creds *providers.Credentials) (*providers.Credentials, error) {
	provider, err := s.factory.CreateProvider(providers.ProviderConfig{
		IntegrationID: integration.ID,
		StoreID:       integration.StoreID,
		Type:          providers.ProviderType(integration.Type),
		Name:          providers.ProviderName(integration.Provider),
		Credentials:   creds,
		Metadata:      integration.Metadata,
	})
	if err != nil {
		return nil, err
	}

	newCreds, err := provider.RefreshToken(ctx)
	if err != nil {
		// Mark integration as error state
		_ = s.repo.UpdateStatus(ctx, integration.ID, "error")
		return nil, fmt.Errorf("refreshing token: %w", err)
	}

	if newCreds == nil {
		// Provider doesn't support token refresh
		return creds, nil
	}

	// Encrypt and save new credentials
	encrypted, err := s.encryptor.EncryptJSON(newCreds)
	if err != nil {
		return nil, fmt.Errorf("encrypting new credentials: %w", err)
	}

	var tokenExpiresAt *time.Time
	if !newCreds.ExpiresAt.IsZero() {
		tokenExpiresAt = &newCreds.ExpiresAt
	}

	if err := s.repo.UpdateCredentials(ctx, integration.ID, encrypted, tokenExpiresAt); err != nil {
		return nil, fmt.Errorf("saving new credentials: %w", err)
	}

	logger.From(ctx, s.logger).Info("token refreshed successfully",
		zap.String("integration_id", integration.ID),
	)

	return newCreds, nil
}

func (s *Service) toCreateOutput(row *IntegrationRow) *CreateIntegrationOutput {
	urls := buildProviderURLs(row.Provider, row.StoreID)

	var lastPing *time.Time
	if row.Metadata != nil {
		if raw, ok := row.Metadata["webhookLastPingAt"].(string); ok && raw != "" {
			if t, err := time.Parse(time.RFC3339, raw); err == nil {
				lastPing = &t
			}
		}
	}

	// Only surface a webhook status when the provider actually has a webhook
	// URL — otherwise the field is meaningless and would confuse consumers.
	var webhookStatus string
	if urls.WebhookURL != "" {
		if lastPing != nil {
			webhookStatus = "active"
		} else {
			webhookStatus = "pending"
		}
	}

	return &CreateIntegrationOutput{
		ID:                row.ID,
		StoreID:           row.StoreID,
		Type:              row.Type,
		Provider:          row.Provider,
		Status:            row.Status,
		Metadata:          row.Metadata,
		LastSyncedAt:      row.LastSyncedAt,
		CreatedAt:         row.CreatedAt,
		RedirectURL:       urls.RedirectURL,
		WebhookURL:        urls.WebhookURL,
		WebhookStatus:     webhookStatus,
		WebhookLastPingAt: lastPing,
		Priority:          row.Priority,
	}
}

// handleProviderError checks if a provider error is rate-limit related and logs accordingly.
// If the error is an ErrRateLimited, it logs at Error level and marks the integration as 'error'.
// HandleProviderError implements payment.IntegrationResolver: it lets the
// extracted payment.Service (B1b fast-paths) report a provider-call failure so
// the integration is flagged unhealthy on rate-limit errors. It delegates to
// the unexported handleProviderError, which stays here because the same logic
// is shared with the ERP/Social fast-paths.
func (s *Service) HandleProviderError(ctx context.Context, integrationID, operation string, err error) {
	s.handleProviderError(ctx, integrationID, operation, err)
}

func (s *Service) handleProviderError(ctx context.Context, integrationID string, operation string, err error) {
	if err == nil {
		return
	}

	var rateLimitErr *ratelimit.ErrRateLimited
	if errors.As(err, &rateLimitErr) {
		// Rate limit NÃO derruba a integração.
		//
		// Antes marcava status 'error', e isso custou três dias de ERP parado em
		// 09/08/2026: o Tiny devolveu um HTTP 429 às 16:56:59, a integração virou
		// 'error' na hora, e nada nunca reverteu — o botão de sincronizar sumiu
		// do painel (o front exige 'active') e só reconectar à mão resolveria,
		// caminho que também estava quebrado. O token só venceu 1h43 DEPOIS; o
		// 429 foi o que derrubou, não a expiração.
		//
		// Ser transitório é a definição de rate limit: o provedor libera em
		// segundos. Marcar estado permanente a partir de um sinal temporário é
		// trocar uma pausa por uma parada.
		//
		// A visibilidade não se perde: a chamada já vira linha em
		// integration_logs com status 'error' e a mensagem crua ("HTTP 429: ..."),
		// que é de onde este diagnóstico saiu. O status da integração descreve se
		// ela está utilizável, e durante um 429 ela está — daqui a pouco.
		logger.From(ctx, s.logger).Error("provider rate limited",
			zap.String("integration_id", integrationID),
			zap.String("operation", operation),
			zap.Duration("retry_after", rateLimitErr.RetryAfter),
		)
	}
}

// noteProviderSuccess devolve a integração para 'active' depois de uma chamada
// bem-sucedida, se ela estava em 'error'.
//
// É a saída que faltava: 'error' era estado terminal. Os únicos pontos que
// escreviam 'active' eram os fluxos de CONEXÃO, então uma falha transitória
// prendia o lojista até ele reconectar à mão.
//
// Best-effort e silencioso no caminho normal: roda em toda chamada de provider
// e é um UPDATE condicional que não acerta linha nenhuma quando já está
// saudável — barato o bastante para o caminho quente.
func (s *Service) noteProviderSuccess(ctx context.Context, integrationID string) {
	if integrationID == "" {
		return
	}
	healed, err := s.repo.HealFromError(ctx, integrationID)
	if err != nil {
		logger.From(ctx, s.logger).Warn("failed to heal integration status after success",
			zap.String("integration_id", integrationID), zap.Error(err))
		return
	}
	if healed {
		logger.From(ctx, s.logger).Info("integration recovered on its own after a successful call",
			zap.String("integration_id", integrationID))
	}
}

// LogIntegrationOperation logs an integration operation to the database.
// This is used by providers via the LogFunc callback.
func (s *Service) LogIntegrationOperation(ctx context.Context, log providers.IntegrationLog) error {
	// Ponto único por onde passa TODA chamada HTTP de provider (providers/base.go),
	// e por isso o lugar certo para a integração se curar sozinha: uma resposta
	// boa é a prova de que ela voltou a funcionar.
	if log.Status == "success" {
		s.noteProviderSuccess(ctx, log.IntegrationID)
	}
	return s.repo.CreateLog(
		ctx,
		log.IntegrationID,
		log.EntityType,
		log.EntityID,
		log.Direction,
		log.Status,
		log.RequestPayload,
		log.ResponsePayload,
		log.ErrorMessage,
	)
}

// endStaleLiveSessionsOnce encerra as sessões de LIVE cuja transmissão já saiu
// do ar no Instagram.
//
// Existe por causa de "o lojista esqueceu de encerrar", que é o caso normal e
// não a exceção: quem termina a live fecha o Instagram e vai embora — voltar ao
// painel para clicar em "Encerrar" é o passo que ninguém dá. Sem isto a sessão
// ficaria "no ar" para sempre no painel, mentindo que captura, e o polling de
// live continuaria consultando a Graph a cada 20s até o EVENTO acabar — uma
// semana inteira, num evento guarda-chuva.
//
// O sinal é exato e não é heurística: GET /me/live_media devolve, por
// definição, apenas o que está sendo transmitido NESTE instante ("Only live
// video media currently being broadcast at the time of the request will be
// returned"). Se a mídia da sessão não está mais lá, a transmissão acabou.
//
// UMA chamada por LOJA, não por mídia: a lista é da conta, então lojas com
// várias sessões abertas custam o mesmo que uma.
//
// Falha da Graph não encerra nada. Encerrar por engano uma live que continua no
// ar é pior do que continuar consultando: pararia de capturar venda. O teto de
// 12h de ListPollableMedia é quem cobre a falha persistente.
func (s *Service) endStaleLiveSessionsOnce(ctx context.Context) {
	medias, err := s.liveService.ListPollableMedia(ctx)
	if err != nil {
		return // sem lista de mídias não há sessão a encerrar
	}

	// Agrupa as mídias de live por loja, para uma consulta por conta.
	byStore := map[string][]live.MediaRef{}
	for _, m := range medias {
		if m.SessionType != live.SessionTypeLive || m.MediaID == "" {
			continue
		}
		byStore[m.StoreID] = append(byStore[m.StoreID], m)
	}

	for storeID, storeMedias := range byStore {
		storeCtx := logger.WithStore(ctx, storeID, "")

		// A inscrição de webhook é garantida AQUI, e não só no OAuth.
		//
		// Ela rodava em dois lugares: no callback do OAuth e num endpoint que
		// alguém precisa lembrar de chamar. Consequência: toda loja conectada
		// antes de uma mudança na lista de campos ficava com a lista velha para
		// sempre — foi assim que `live_comments` faltou na conta de uma loja
		// que vende por live, sem ninguém ter como perceber.
		//
		// Este é o momento certo: existe uma transmissão NO AR desta loja, ou
		// seja, é exatamente quando a captura precisa estar de pé. Uma vez por
		// loja por processo (ver subscriptionEnsured), então o custo é uma
		// chamada por lojista que fez live desde o último deploy.
		s.ensureWebhookSubscriptionOnce(storeCtx, storeID)
		provider, err := s.resolveInstagramSocialProvider(storeCtx, storeID)
		if err != nil {
			continue
		}
		lister, ok := provider.(interface {
			GetActiveLives(ctx context.Context) ([]providers.LiveMedia, error)
		})
		if !ok {
			continue
		}
		lives, err := lister.GetActiveLives(storeCtx)
		if err != nil {
			logger.From(storeCtx, s.logger).Warn("live sweep: failed to list active lives",
				zap.Error(err))
			continue
		}

		onAir := make(map[string]struct{}, len(lives))
		for _, l := range lives {
			onAir[l.ID] = struct{}{}
		}

		for _, m := range storeMedias {
			if _, still := onAir[m.MediaID]; still {
				continue
			}
			if _, err := s.liveService.EndSession(storeCtx, live.EndSessionInput{
				StoreID:   storeID,
				EventID:   m.EventID,
				SessionID: m.SessionID,
			}); err != nil {
				logger.From(storeCtx, s.logger).Warn("live sweep: failed to end session",
					zap.String("session_id", m.SessionID), zap.Error(err))
				continue
			}
			// O EVENTO segue aberto de propósito: a transmissão acabou, a
			// campanha não. Os carrinhos continuam valendo até o fim dela.
			logger.From(storeCtx, s.logger).Info("live sweep: session ended, broadcast is over",
				zap.String("session_id", m.SessionID),
				zap.String("media_id", m.MediaID),
			)
		}
	}
}

// ensureWebhookSubscriptionOnce garante, uma vez por loja por processo, que a
// conta do Instagram está inscrita na lista ATUAL de campos de webhook.
//
// Best-effort e silencioso no sucesso: quem chama está no meio de um sweep de
// 20s e não pode ser derrubado por isto. A falha vira Warn porque tem
// consequência real — sem inscrição a Meta não entrega nada e a captura passa a
// depender só do polling, que salva o pedido mas não consegue avisar o
// comprador.
func (s *Service) ensureWebhookSubscriptionOnce(ctx context.Context, storeID string) {
	if v, ok := s.subscriptionEnsured.Load(storeID); ok {
		if last, _ := v.(time.Time); time.Since(last) < subscriptionRecheckEvery {
			return
		}
	}
	s.subscriptionEnsured.Store(storeID, time.Now())
	if err := s.SubscribeInstagramWebhooks(ctx, storeID); err != nil {
		// Solta o registro: uma falha transitória (token vencendo, Graph fora)
		// merece nova tentativa no próximo sweep, não esperar a janela inteira.
		s.subscriptionEnsured.Delete(storeID)
		logger.From(ctx, s.logger).Warn("failed to ensure instagram webhook subscription for a store with a live on air",
			zap.String("store_id", storeID), zap.Error(err))
	}
}

// subscriptionRecheckEvery é de quanto em quanto tempo a inscrição volta a ser
// garantida, para uma loja COM TRANSMISSÃO NO AR.
//
// Antes era uma vez por processo, e essa era a única autocorreção que o sistema
// tinha para uma inscrição morta. A Meta derruba a assinatura de um app cujas
// entregas falham seguidamente — que é exatamente o cenário que estivemos
// investigando, com o Bot Fight Mode do Cloudflare desafiando as entregas dela.
// Num processo que fica semanas no ar, "uma vez" significava nunca mais.
//
// Quinze minutos custa quatro chamadas por hora por loja com live acontecendo, e
// só enquanto ela acontece. É barato perto de uma transmissão inteira entregue
// só pelo polling.
const subscriptionRecheckEvery = 15 * time.Minute

// attachPublishedMediaToEvent liga uma publicação recém-criada no Instagram ao
// LiveCart. Nome distinto de bindPublishedMedia (publish_schedule.go), que é o
// caminho do agendamento e sempre cria evento próprio.
//
// Dois destinos, uma porta: com eventID, a publicação vira mais uma SESSÃO do
// evento que já existe; sem eventID, cria um evento próprio, que é o
// comportamento de sempre.
//
// A ligação por sessão é o que faz o guarda-chuva funcionar de ponta a ponta:
// o post de terça entra no MESMO evento da live de segunda, então o comprador
// que pediu nos dois continua com um carrinho só. Publicar sempre criando
// evento próprio produzia dois carrinhos para a mesma pessoa na mesma campanha.
//
// Os três caminhos de publicação (post, reel, story) passam por aqui de
// propósito: a regra de a qual evento a mídia pertence não pode divergir entre
// eles, e divergiria se cada um tivesse a sua cópia.
func (s *Service) attachPublishedMediaToEvent(
	ctx context.Context,
	input CreateInstagramPostInput,
	sessionType, mediaID, permalink, thumbnail string,
) (live.CreateLiveOutput, error) {
	if input.EventID == "" {
		return s.liveService.CreatePostEvent(ctx, live.CreatePostInput{
			StoreID:                input.StoreID,
			Title:                  input.Title,
			Type:                   sessionType,
			MediaID:                mediaID,
			MediaPermalink:         permalink,
			MediaThumbnailURL:      thumbnail,
			MediaCaption:           input.Caption,
			ProductIDs:             input.ProductIDs,
			StartsAt:               input.StartsAt,
			EndsAt:                 input.EndsAt,
			CartExpirationMinutes:  input.CartExpirationMinutes,
			CartMaxQuantityPerItem: input.CartMaxQuantityPerItem,
		})
	}

	// Janela, expiração e teto NÃO são reenviados: eles são do evento, e a
	// sessão que entra nele obedece o que já está lá. Aceitá-los aqui deixaria
	// uma publicação redefinir a regra da campanha inteira pelas costas.
	session, err := s.liveService.CreateSession(ctx, live.CreateSessionInput{
		EventID:           input.EventID,
		StoreID:           input.StoreID,
		Type:              sessionType,
		Platform:          "instagram",
		PlatformLiveID:    mediaID,
		MediaPermalink:    permalink,
		MediaThumbnailURL: thumbnail,
		MediaCaption:      input.Caption,
		ProductIDs:        input.ProductIDs,
	})
	if err != nil {
		return live.CreateLiveOutput{}, err
	}

	// O ID devolvido é o do EVENTO, não o da sessão: quem chamou quer navegar
	// para a campanha, que é onde a publicação agora vive.
	return live.CreateLiveOutput{
		ID:        session.EventID,
		Title:     input.Title,
		Platform:  "instagram",
		Status:    session.Status,
		CreatedAt: session.CreatedAt,
	}, nil
}

// instagramAccountID devolve o id da conta profissional do Instagram da loja —
// o mesmo que a Meta manda em entry.id nos webhooks. Vazio quando a integração
// não resolve; é uso de LOG, então falhar aqui não pode derrubar captura.
func (s *Service) instagramAccountID(ctx context.Context, storeID string) string {
	integration, err := s.repo.GetActiveByProvider(ctx, storeID, "social", "instagram")
	if err != nil || integration == nil {
		return ""
	}
	if v, ok := integration.Metadata["instagram_user_id"].(string); ok {
		return v
	}
	return ""
}

// TracePrefixIG marca as linhas da investigação da entrega de live_comments.
// TODO REMOVER junto com o restante do [IGTRACE].
const TracePrefixIG = "[IGTRACE] "

// instagramAltAccountID devolve o OUTRO id do Instagram da loja — a Meta tem
// dois (conta profissional e app-scoped) e, dependendo de quando a conta foi
// conectada, o metadata guarda um ou outro em `instagram_user_id`.
func (s *Service) instagramAltAccountID(ctx context.Context, storeID string) string {
	integration, err := s.repo.GetActiveByProvider(ctx, storeID, "social", "instagram")
	if err != nil || integration == nil {
		return ""
	}
	if v, ok := integration.Metadata["instagram_app_scoped_id"].(string); ok {
		return v
	}
	return ""
}

// instagramUsername é o @ da conta que a loja conectou. Usado para reconhecer
// que um comentário veio da PRÓPRIA loja — caso em que o id devolvido pelo
// polling é o da conta, e não um destinatário válido de DM.
func (s *Service) instagramUsername(ctx context.Context, storeID string) string {
	integration, err := s.repo.GetActiveByProvider(ctx, storeID, "social", "instagram")
	if err != nil || integration == nil {
		return ""
	}
	if v, ok := integration.Metadata["username"].(string); ok {
		return v
	}
	return ""
}
