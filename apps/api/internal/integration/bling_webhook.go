package integration

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	"go.uber.org/zap"

	"livecart/apps/api/internal/erp"
	"livecart/apps/api/internal/integration/providers"
	"livecart/apps/api/lib/config"
	"livecart/apps/api/lib/httpx"
	"livecart/apps/api/lib/logger"
)

// Borda do webhook do Bling.
//
// Diferença estrutural em relação a todos os outros webhooks do LiveCart: a URL
// é ÚNICA para todas as lojas. O Bling não tem API para registrar webhook — a
// URL é cadastrada na UI do APLICATIVO, e o aplicativo é um só. Não há
// `:storeId` no caminho para ancorar nada.
//
// Quem identifica a origem é o `companyId` do envelope. MEDIDO em 29/08/2026:
// ele é BYTE-IDÊNTICO ao `data.id` de GET /empresas/me/dados-basicos, que
// gravamos em integrations.erp_account_id no fim do OAuth. É por ele que se
// resolve a loja, com índice.
//
// Isso é a mesma classe de problema do webhook do Pagar.me numa conta
// compartilhada, onde pagamento de outra plataforma caiu no LiveCart: a defesa
// é a mesma — checar POSSE antes de agir, nunca confiar em quem entregou.

// BlingEnvelope é o corpo que o Bling entrega. Forma MEDIDA de um evento real
// (o spec não a descreve: webhook não é endpoint).
type BlingEnvelope struct {
	EventID   string          `json:"eventId"`
	Date      string          `json:"date"`
	Version   string          `json:"version"` // medido: "v1"
	Event     string          `json:"event"`   // recurso.acao — ex. "stock.created"
	CompanyID string          `json:"companyId"`
	Data      json.RawMessage `json:"data"`
}

// BlingStockData é o payload dos eventos de estoque.
//
// Carrega os saldos físico/virtual e por depósito. Servem de evidência de
// capacidade de reserva. O espelho relê o saldo atual, pois a entrega pode ser
// fora de ordem e aplicar diretamente um saldo antigo reabriria vendas.
type BlingStockData struct {
	Produto struct {
		ID int64 `json:"id"`
	} `json:"produto"`
	SaldoFisicoTotal  float64 `json:"saldoFisicoTotal"`
	SaldoVirtualTotal float64 `json:"saldoVirtualTotal"`
	Deposito          struct {
		ID           int64   `json:"id"`
		SaldoFisico  float64 `json:"saldoFisico"`
		SaldoVirtual float64 `json:"saldoVirtual"`
	} `json:"deposito"`
	Operacao   string  `json:"operacao"`
	Quantidade float64 `json:"quantidade"`
}

const blingSignatureHeader = "X-Bling-Signature-256"

// verifyBlingSignature confere o HMAC-SHA256 do corpo CRU.
//
// A chave é o client_secret do APLICATIVO usado pela integração. O aplicativo
// compartilhado usa o ambiente; aplicativos privados usam Credentials.Extra.
// Consequência que precisa estar escrita: rotacionar o secret invalida o Basic
// do token endpoint E a assinatura de todos os webhooks ao mesmo tempo, então a
// troca exige janela com dois segredos aceitos.
func verifyBlingSignature(body []byte, header, secret string) signatureOutcome {
	if secret == "" {
		return signatureUnconfigured
	}
	header = strings.TrimSpace(header)
	if header == "" {
		return signatureMissing
	}
	valor, ok := strings.CutPrefix(header, signaturePrefix)
	if !ok {
		return signatureMalformed
	}
	esperado, err := hex.DecodeString(strings.TrimSpace(valor))
	if err != nil || len(esperado) != sha256.Size {
		return signatureMalformed
	}

	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	// Comparação em tempo constante: byte a byte vazaria o digest por timing.
	if !hmac.Equal(esperado, mac.Sum(nil)) {
		return signatureMismatch
	}
	return signatureValid
}

// HandleBling authenticates and persists the event before acknowledging it.
// Processing runs from the durable outbox, outside Bling's five-second budget.
// @Summary Bling webhook
// @Router /api/webhooks/bling [post]
func (h *WebhookHandler) HandleBling(c *fiber.Ctx) error {
	return handleBlingWebhook(c, config.BlingClientSecret.String(), h.service.repo.GetActiveERPByAccount,
		h.service.blingWebhookSecret, h.service.enqueueBlingWebhook, h.logger)
}

type blingIntegrationLookup func(context.Context, string, string) (*IntegrationRow, error)
type blingWebhookSecretResolver func(*IntegrationRow) (string, error)
type blingWebhookEnqueue func(context.Context, *IntegrationRow, BlingEnvelope) error

func handleBlingWebhook(c *fiber.Ctx, secret string, lookup blingIntegrationLookup, resolveSecret blingWebhookSecretResolver, enqueue blingWebhookEnqueue, baseLog *zap.Logger) error {
	ctx, cancel := context.WithTimeout(c.UserContext(), 4*time.Second)
	defer cancel()
	log := logger.From(ctx, baseLog)
	outcome := verifyBlingSignature(c.Body(), c.Get(blingSignatureHeader), secret)
	// Structural signature failures cannot be repaired by a private app secret.
	// A mismatch can: CompanyID is used only as a lookup hint until authenticated.
	shape := outcome
	if shape == signatureUnconfigured {
		shape = verifyBlingSignature(c.Body(), c.Get(blingSignatureHeader), "signature-shape-check")
	}
	if shape == signatureMissing || shape == signatureMalformed {
		log.Warn("bling webhook: assinatura recusada", zap.String("assinatura", string(outcome)))
		return c.SendStatus(fiber.StatusUnauthorized)
	}
	var env BlingEnvelope
	if err := json.Unmarshal(c.Body(), &env); err != nil || env.Event == "" || env.EventID == "" || env.CompanyID == "" || !json.Valid(env.Data) {
		log.Warn("bling webhook: envelope inválido")
		return c.SendStatus(fiber.StatusBadRequest)
	}
	log = log.With(zap.String("bling_event", env.Event), zap.String("bling_event_id", env.EventID), zap.String("bling_company_id", env.CompanyID))
	integration, err := lookup(ctx, string(providers.ProviderBling), env.CompanyID)
	if httpx.IsNotFound(err) || (err == nil && integration == nil) {
		if outcome != signatureValid {
			return c.SendStatus(fiber.StatusUnauthorized)
		}
		log.Debug("bling webhook: conta sem integração ativa; ignorado")
		return c.JSON(fiber.Map{"ok": true})
	}
	if err != nil {
		log.Error("bling webhook: falha ao resolver integração", zap.Error(err))
		return c.SendStatus(fiber.StatusServiceUnavailable)
	}
	boundSecret, err := resolveSecret(integration)
	if err != nil {
		log.Error("bling webhook: falha ao resolver credencial de assinatura", zap.String("integration_id", integration.ID), zap.Error(err))
		return c.SendStatus(fiber.StatusServiceUnavailable)
	}
	// Check the selected integration's app even if the global signature passed:
	// a shared-app signature must not authenticate an account using a private app.
	outcome = verifyBlingSignature(c.Body(), c.Get(blingSignatureHeader), boundSecret)
	if outcome != signatureValid {
		status := fiber.StatusUnauthorized
		if outcome == signatureUnconfigured {
			status = fiber.StatusServiceUnavailable
		}
		log.Warn("bling webhook: assinatura recusada", zap.String("assinatura", string(outcome)), zap.String("integration_id", integration.ID))
		return c.SendStatus(status)
	}
	if err := enqueue(ctx, integration, env); err != nil {
		log.Error("bling webhook: falha ao persistir evento", zap.String("store_id", integration.StoreID), zap.Error(err))
		return c.SendStatus(fiber.StatusServiceUnavailable)
	}
	log.Info("bling webhook: evento persistido", zap.String("store_id", integration.StoreID), zap.String("integration_id", integration.ID))
	return c.JSON(fiber.Map{"ok": true})
}

func cortarRecurso(evento string) (recurso, acao string, ok bool) {
	return strings.Cut(evento, ".")
}

// despacharPedidoBling returns processing failures to the queue for retry.
func (s *Service) despacharPedidoBling(ctx context.Context, integration *IntegrationRow, env BlingEnvelope) error {
	id, err := blingResourceID(env.Data)
	if err != nil {
		return err
	}
	_, err = s.coalescedorDeSituacao().Fazer(integration.StoreID+"|"+id, func() error {
		return s.observarSituacaoDoPedidoBling(ctx, integration, id, env.Event)
	})
	return err
}

func blingResourceID(data json.RawMessage) (string, error) {
	var resource struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(data, &resource); err != nil {
		return "", fmt.Errorf("decoding bling resource id: %w", err)
	}
	if resource.ID <= 0 {
		return "", fmt.Errorf("bling resource id must be positive")
	}
	return strconv.FormatInt(resource.ID, 10), nil
}

// observarSituacaoDoPedidoBling lê a situação atual e entrega ao rastreamento.
//
// Um GET, e não o payload: a forma de `data` não está verificada em fonte
// nenhuma, e a leitura devolve a situação normalizada (`situacao.valor`), que
// vale em qualquer conta — ao contrário do `id`, que é do lojista.
func (s *Service) observarSituacaoDoPedidoBling(
	ctx context.Context, integracao *IntegrationRow, pedidoID, evento string,
) error {
	log := logger.From(ctx, s.logger).With(
		zap.String("external_order_id", pedidoID),
		zap.String("bling_event", evento),
	)

	// ═══ A PERGUNTA DE GRAÇA VEM ANTES DA CARA ═══
	//
	// O webhook do Bling é POR CONTA, não por pedido nosso: chega evento de
	// TODO pedido do lojista — os que ele digita à mão, os do site dele, os de
	// marketplace. Ler a situação de um pedido que não é nosso gasta uma
	// requisição do teto de 3 req/s da conta e não conclui nada.
	//
	// Durante uma live esse teto é o mesmo que cria os pedidos da venda. Cada
	// leitura desperdiçada aqui é uma escrita que falta lá.
	//
	// O vínculo está no nosso banco, indexado. Perguntar a ele é grátis.
	cartID, err := s.CartIDByExternalOrder(ctx, integracao.StoreID, pedidoID)
	if err != nil {
		return fmt.Errorf("resolvendo o carrinho do pedido %s: %w", pedidoID, err)
	}
	if cartID == "" {
		log.Debug("bling: pedido não é de nenhum carrinho desta loja; nada a observar")
		return nil
	}

	prov, err := s.createProviderFromRow(ctx, integracao)
	if err != nil {
		return fmt.Errorf("criando o provider: %w", err)
	}
	leitor, ok := prov.(interface {
		GetOrderSituacao(ctx context.Context, orderID string) (int, error)
	})
	if !ok {
		return nil // provider sem leitura de situação: nada a observar
	}

	canonico, err := leitor.GetOrderSituacao(ctx, pedidoID)
	if errors.Is(err, providers.ErrOrderNotFound) {
		// O pedido ERA nosso e sumiu do ERP: o lojista apagou lá. Não é falha
		// nossa e não há situação para observar — mas é fato operacional, e o
		// carrinho continua apontando para um pedido que não existe mais.
		//
		// NÃO desfazemos o vínculo aqui de propósito: limpá-lo faria o próximo
		// evento do carrinho criar um pedido NOVO no ERP, e um apagão acidental
		// no Bling viraria duplicata silenciosa. Quem decide refazer é o
		// lojista, pela tela.
		log.Warn("bling: o pedido deste carrinho não existe mais no ERP (apagado lá)",
			zap.String("cart_id", cartID))
		return nil
	}
	if err != nil {
		return fmt.Errorf("lendo a situação do pedido %s: %w", pedidoID, err)
	}

	status, conhecido := providers.ERPOrderStatusFromSituacao(canonico)
	if !conhecido {
		// "Não sei" explícito. O Bling tem situações sem análogo no nosso enum
		// (parciais, "em digitação"), e inventar uma faria o rastreamento
		// concluir coisa que ninguém afirmou.
		logger.From(ctx, s.logger).Debug("bling: situação sem análogo no núcleo; nada a observar",
			zap.String("external_order_id", pedidoID), zap.Int("canonico", canonico))
		return nil
	}

	logger.From(ctx, s.logger).Info("bling: situação do pedido observada",
		zap.String("external_order_id", pedidoID),
		zap.String("bling_event", evento),
		zap.String("status", string(status)),
	)
	return s.ERP().ObserveOrderStatus(
		ctx, integracao.StoreID, pedidoID, "", status, erp.StatusSourceWebhook, nil,
	)
}

// observarCapacidadeDeReserva guarda a prova de que a conta reserva.
//
// Só grava quando a prova é POSITIVA, e só uma vez: a ausência de reserva num
// instante não prova nada (pode não haver pedido aberto, e o saldo demora de 9
// a 22 s para refletir um pedido novo), então ela nunca derruba o veredito.
func (s *Service) observarCapacidadeDeReserva(ctx context.Context, integration *IntegrationRow, fisico, disponivel int) error {
	if erp.ObservarCapacidade(fisico, disponivel) != erp.ContaReserva || erp.CapacidadeConfirmada(integration.Provider, integration.Metadata) {
		return nil
	}
	if err := s.repo.recordBlingReservationCapability(ctx, integration.ID); err != nil {
		return err
	}
	logger.From(ctx, s.logger).Info("capacidade de reserva confirmada por observação", zap.String("integration_id", integration.ID), zap.String("provider", integration.Provider))
	return nil
}
