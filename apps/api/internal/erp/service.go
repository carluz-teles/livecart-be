package erp

import (
	"context"
	"sync"
	"time"

	"go.uber.org/zap"

	"livecart/apps/api/internal/erp/erpwrite"
)

// ERPRepository is the persistence port consumed by the ERP service. It is
// declared here (the consumer side, per Go idiom) and satisfied by
// internal/integration.Repository via a boot-wired adapter — erp MUST NOT import
// integration (cycle). It starts with the two methods the ERP order flow already
// needs and grows one method per slice as logic migrates (Bloco B2b+).
type ERPRepository interface {
	// ListCartGridItems devolve a grade do GRUPO — este carrinho mais os que
	// foram juntados a ele. É o que sobe para o pedido no ERP.
	ListCartGridItems(ctx context.Context, cartID string) ([]NonWaitlistedCartItem, error)
	// CartIsPaid diz se o carrinho já tem pagamento registrado deste lado.
	CartIsPaid(ctx context.Context, cartID string) (bool, error)
	// CartIsTerminated diz se o carrinho chegou a um fim de onde não sai
	// sozinho (cancelado ou vencido).
	CartIsTerminated(ctx context.Context, cartID string) (bool, error)
	// ListERPLinkedProductsSample devolve uma amostra de produtos ligados ao
	// ERP e com estoque — a matéria-prima da checagem do módulo de Reserva.
	ListERPLinkedProductsSample(ctx context.Context, storeID, externalSource string, limite int) ([]ERPLinkedProduct, error)
	// GetActiveByProvider resolves the active ERP integration for a store.
	// Signature mirrors integration.Repository.GetActiveByProvider, but returns
	// the neutral erp.Integration instead of the integration-owned IntegrationRow
	// so this package stays free of the integration import.
	GetActiveByProvider(ctx context.Context, storeID, integrationType, provider string) (*Integration, error)

	// GetActiveERP resolve o ERP ATIVO da loja sem perguntar por provider — a
	// pergunta certa, agora que existe mais de um ERP possível. O banco garante
	// no máximo um por loja (uniq_integrations_store_one_erp, migration 000061),
	// então não há ambiguidade a resolver aqui.
	//
	// Devolve o MESMO httpx.ErrNotFound de GetActiveByProvider quando a loja não
	// tem ERP, para que os call sites que já tratam "sem ERP" não mudem.
	GetActiveERP(ctx context.Context, storeID string) (*Integration, error)

	// GetByProvider resolves ANY integration for a store (active or not) so the
	// legacy finalisation can disambiguate "merchant never set up Tiny" (info)
	// from "Tiny exists but is in error state" (warn). Returns the neutral
	// erp.Integration, mapped by the boot-wired adapter (same cycle guard as
	// GetActiveByProvider).
	GetByProvider(ctx context.Context, storeID, integrationType, provider string) (*Integration, error)

	// AcquireCartFinalisationLock takes a session-scoped Postgres advisory lock
	// keyed on the cart id. The lock lives on integration.Repository (which holds
	// the raw pgxpool — advisory locks are per-connection); erp only consumes it
	// through this port. acquired=false means another finalisation of the SAME
	// cart is running right now, so the loser bails. The caller MUST call
	// release() when acquired.
	AcquireCartFinalisationLock(ctx context.Context, cartID string) (release func(), acquired bool, err error)

	// GetCartERPOrderState reads the cart's ERP order lifecycle state.
	GetCartERPOrderState(ctx context.Context, cartID string) (*CartERPOrderState, error)
	// GetCartERPOpAge diz há quanto tempo a operação ERP em curso começou. É o
	// que separa "criação em voo agora" de "criação que morreu" — as duas se
	// parecem no estado, e só o relógio as distingue. Zero quando não há marca.
	GetCartERPOpAge(ctx context.Context, cartID string) (time.Duration, error)
	// GetCartERPFinalisationStatus reads the Order payment row's ERP finalisation
	// lifecycle (status/attempts/snapshot) plus the cart's external_order_id. É o
	// que o reenvio manual relê para não aprovar a venda sem o financeiro.
	GetCartERPFinalisationStatus(ctx context.Context, cartID string) (*CartFinalisationStatus, error)
	// MarkCartERPFinalisationAttempt stamps the attempt (bumps count, COALESCEs
	// the gateway snapshot) BEFORE the ERP is touched, so an admin retry replays.
	MarkCartERPFinalisationAttempt(ctx context.Context, cartID string, paymentSnapshot []byte) error

	// --- Contador de estoque local ---
	// O contador local é o portão da venda: atômico, é o que a fila de espera
	// consulta e o que responde ao comprador na hora. Ele existe
	// independentemente do ERP — loja sem integração vende por ele igual.

	// GetCartShortID returns the cart's human-facing sequential number (the
	// #1189 the merchant sees in LiveCart).
	GetCartShortID(ctx context.Context, cartID string) (int32, error)
	// DecrementProductStock atomically lowers local stock; ErrNoRows means the
	// decrement would go negative (insufficient stock).
	DecrementProductStock(ctx context.Context, productID string, quantity int) error
	// IncrementProductStock raises local stock (also satisfies stockReservationRepo).
	IncrementProductStock(ctx context.Context, productID string, quantity int) error
	// EmitStockReserved / EmitStockReleased publish the stock events best-effort
	// (also satisfy stockReservationRepo, so NewStockReservations can take repo).
	EmitStockReserved(ctx context.Context, p StockEventParams) error
	EmitStockReleased(ctx context.Context, p StockEventParams) error

	// --- Order-as-reservation lifecycle persistence (Bloco B2c, Design C) ---
	// Each method below is already implemented verbatim on integration.Repository
	// and promoted by the erpRepoAdapter (the DTO types are aliases of the erp
	// ones), so no new adapter code is needed.

	// TransitionCartERPOrderState is the state-machine CAS: it advances the cart
	// from `from` to `to` and returns false when the current state differs from
	// `from` (the single-flight primitive of conversion/mutation/confirm).
	TransitionCartERPOrderState(ctx context.Context, cartID, from, to string) (bool, error)
	// UpdateCartExternalOrderID persists the ERP order id on the cart (marker
	// adoption in confirm/sweep).
	UpdateCartExternalOrderID(ctx context.Context, cartID, externalOrderID string) error
	// SetCartERPStockLaunched flips the durable "order stock launched" marker.
	SetCartERPStockLaunched(ctx context.Context, cartID string, launched bool) error
	// MarkCartERPFinalisationDone stamps the terminal finalisation marker so the
	// existing telemetry/guards stay coherent (confirmed = done).
	MarkCartERPFinalisationDone(ctx context.Context, cartID string) error
	// ListNonWaitlistedCartItems returns the ERP-linked cart grid — input for the
	// order mutation cycle and the confirm payment total.
	ListNonWaitlistedCartItems(ctx context.Context, cartID string) ([]NonWaitlistedCartItem, error)
	// ListStuckERPOrderOps lists converting/mutating carts older than the
	// threshold — input for the reconciliation sweep.
	ListStuckERPOrderOps(ctx context.Context, olderThan time.Duration) ([]StuckERPOrderOp, error)
	// ListTinyIntegrationsWithStaleStockWebhook lists active Tiny integrations
	// with no stock webhook events in the window (URL likely removed by Tiny).
	ListTinyIntegrationsWithStaleStockWebhook(ctx context.Context, staleAfter time.Duration) ([]StaleStockWebhookIntegration, error)
	// StampIntegrationStockWebhookAlert dedupes the stale-webhook alert (24h).
	StampIntegrationStockWebhookAlert(ctx context.Context, integrationID string) error

	// --- Cart NFe sync persistence (Bloco B2d) ---
	// UpsertCartERPInvoice, FindCartByExternalOrderID and UpdateShipmentInvoice are
	// already implemented verbatim on integration.Repository (DTO types aliased),
	// so they satisfy this port directly. GetShipmentByOrderID is bridged to the
	// slim erp.ShipmentInvoiceRef by the erpRepoAdapter (same cycle guard as
	// GetActiveByProvider), and GetCartInvoiceAnchor is the new enxuto cart reader.

	// GetCartInvoiceAnchor returns just the two cart fields the NFe sync needs —
	// the owning store and the ERP order id — without dragging the full CartRow
	// (which stays integration-owned to avoid the import cycle).
	GetCartInvoiceAnchor(ctx context.Context, cartID string) (storeID, externalOrderID string, err error)
	// UpsertCartERPInvoice persists the NFe onto the Order's payment row (resolved
	// from cart_id). Idempotent; returns 0 rows when no Order exists yet (benign
	// skip — the NF is always post-confirmation).
	UpsertCartERPInvoice(ctx context.Context, params UpsertCartERPInvoiceParams) (int64, error)
	// FindCartByExternalOrderID locates the cart linked to an ERP pedido id for a
	// store — the bridge the Tiny nota_fiscal webhook needs (Tiny only sends the
	// pedido id).
	FindCartByExternalOrderID(ctx context.Context, externalOrderID, storeID string) (string, error)
	// GetShipmentByOrderID returns the slim invoice ref of the (at most one)
	// shipment attached to the cart, or nil when none exists yet.
	GetShipmentByOrderID(ctx context.Context, cartID string) (*ShipmentInvoiceRef, error)
	// UpdateShipmentInvoice mirrors the NFe chave/kind onto an existing shipment.
	UpdateShipmentInvoice(ctx context.Context, shipmentID, invoiceKey, invoiceKind string) error
}

// Service handles ERP-domain business logic.
type Service struct {
	repo   ERPRepository
	collab StockCollaborators
	stock  *StockReservations
	status ERPOrderStatusRepository
	// reopener segue o pedido que o lojista reabriu no ERP. Opcional.
	reopener CartReopener
	// cartSync é o caminho de volta: o pedido do ERP refletido no carrinho.
	// Ver reflexo.go.
	cartSync CartSyncCollaborators
	logger   *zap.Logger

	// escrita serializa as escritas no ERP e as mantém dentro do teto real da
	// conta. Não é opcional: o teto é um fato da API, não uma preferência.
	escrita *filaDeEscrita
}

// filaDeEscrita é o par limitador + fila serial por chave.
//
// Existe por dois números medidos. O primeiro é o teto: 4 escritas por segundo
// em rajada e 30 por minuto sustentadas, POR CONTA — uma live de 150 comentários
// passa disso com folga, e o Tiny não manda Retry-After, então quem não se
// contém sozinho descobre o limite por 429. O segundo é a corrida: duas escritas
// em voo no MESMO pedido corrompem a grade, então elas entram em fila pela chave
// do carrinho.
//
// Os baldes são POR LOJA porque o teto é por conta do ERP: um processo que
// atende dez lojas não pode fazer uma esperar a cota da outra.
type filaDeEscrita struct {
	mu      sync.Mutex
	limites erpwrite.Limits
	// sobreposto marca que SetWriteLimits mandou um teto explícito. Nesse caso
	// ele vale para todo mundo — é o que os testes de fluxo usam para abrir os
	// portões sem esperar a janela de 60 segundos.
	sobreposto bool
	fila       *erpwrite.Queue
	baldes     map[string]*erpwrite.Limiter
	// providerDoBalde lembra com que ERP cada balde foi semeado. Uma loja que
	// troca de ERP precisa de balde novo: sem isto ela ficaria com o teto do
	// ERP antigo até o próximo deploy.
	providerDoBalde map[string]string
}

func novaFilaDeEscrita(limites erpwrite.Limits) *filaDeEscrita {
	return &filaDeEscrita{
		limites: limites,
		// ⚠ NewQueue(BurstN) NÃO é balde de taxa: é o semáforo de concorrência
		// do PROCESSO, compartilhado por todas as lojas e todos os providers.
		// Ele fica com o número do padrão de propósito — mexer nele é outra
		// decisão, sobre quantas escritas simultâneas o processo aguenta, e não
		// sobre quanto cada ERP aceita por minuto.
		fila:            erpwrite.NewQueue(limites.BurstN),
		baldes:          map[string]*erpwrite.Limiter{},
		providerDoBalde: map[string]string{},
	}
}

// balde devolve o limitador da loja, semeado com o teto do ERP DELA.
func (f *filaDeEscrita) balde(storeID, provider string) *erpwrite.Limiter {
	f.mu.Lock()
	defer f.mu.Unlock()

	// Teto explícito (SetWriteLimits) vale para todo mundo — os testes contam
	// com isso para abrir os portões.
	if f.sobreposto {
		lim, ok := f.baldes[storeID]
		if !ok {
			lim = erpwrite.NewLimiter(f.limites)
			f.baldes[storeID] = lim
		}
		return lim
	}

	lim, ok := f.baldes[storeID]
	if ok && f.providerDoBalde[storeID] == provider {
		return lim
	}
	// Loja nova, ou loja que trocou de ERP: balde novo com o teto certo.
	lim = erpwrite.NewLimiter(erpwrite.LimitesDoProvider(provider))
	f.baldes[storeID] = lim
	f.providerDoBalde[storeID] = provider
	return lim
}

// SetWriteLimits troca os tetos de escrita da conta.
//
// O padrão é o medido contra a API real, e é o que vale em produção. Existe
// porque o teto é uma propriedade da CONTA no ERP — planos diferentes podem ter
// números diferentes —, e porque os testes de fluxo não têm por que esperar uma
// janela de 60 segundos para provar uma regra de negócio. O limitador tem os
// seus próprios testes.
func (s *Service) SetWriteLimits(limites erpwrite.Limits) {
	s.escrita = novaFilaDeEscrita(limites)
	s.escrita.sobreposto = true
}

// escreverNoERP roda fn com a vez do carrinho e dentro da cota da loja.
//
// A ordem importa: primeiro a fila (garante que ninguém mais está escrevendo
// neste pedido), depois o limitador (garante que a conta tem cota). Invertida,
// duas escritas do mesmo pedido poderiam passar pelo limitador juntas e só então
// disputar a fila, o que não corrompe nada mas gasta cota fora de hora.
//
// erpwrite.ErrNotDispatched sobe intacto quando o prazo não comporta a espera:
// é a diferença entre "não saiu daqui" e "não sei se chegou", e ela decide se
// repetir é seguro.
func (s *Service) escreverNoERP(ctx context.Context, storeID, chave string, fn func(context.Context) error) error {
	if s.escrita == nil {
		return fn(ctx)
	}
	// O teto é do ERP da loja, e por isso ele é resolvido aqui. É a mesma
	// consulta indexada que o resto do caminho quente já faz; falhar nela cai no
	// padrão mais APERTADO (Tiny), porque o desconhecido não pode ganhar folga.
	provider := ""
	if integracao, err := s.repo.GetActiveERP(ctx, storeID); err == nil && integracao != nil {
		provider = integracao.Provider
	}

	return s.escrita.fila.Do(ctx, chave, func(ctx context.Context) error {
		if err := s.escrita.balde(storeID, provider).Wait(ctx); err != nil {
			return err
		}
		return fn(ctx)
	})
}

// NewService creates a new ERP service. collab supplies the integration-Service
// helpers the ERP flow calls back into (provider resolution, product linking,
// order creation, mirroring).
func NewService(repo ERPRepository, collab StockCollaborators, logger *zap.Logger) *Service {
	return &Service{
		repo:    repo,
		collab:  collab,
		stock:   NewStockReservations(repo, logger),
		logger:  logger,
		escrita: novaFilaDeEscrita(erpwrite.DefaultLimits()),
	}
}
