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

	"github.com/gofiber/fiber/v2"
	"go.uber.org/zap"

	"livecart/apps/api/internal/erp"
	"livecart/apps/api/internal/integration/providers"
	"livecart/apps/api/lib/config"
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
// ⭐ Ele CARREGA O SALDO — `saldoFisicoTotal`, `saldoVirtualTotal` e o saldo por
// depósito. O espelho pode ser alimentado direto do evento, SEM gastar uma
// requisição da cota. Numa conta com teto de 3 req/s, cada webhook que não
// exige leitura de volta é cota que sobra para a venda.
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
// A chave é o client_secret do APLICATIVO — o mesmo para todas as lojas.
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

// HandleBling recebe os eventos do Bling.
//
// ⚠ RESPONDE 2xx SEMPRE que o evento é nosso e legível, ANTES de processar.
// O Bling exige 2xx em ATÉ 5 SEGUNDOS; passando disso ele re-entrega por 3 dias
// e, esgotado o retry, DESATIVA a configuração do webhook na UI — cegando TODAS
// as lojas do aplicativo de uma vez, porque a config é do aplicativo. Um
// processamento lento aqui não atrasa um evento: derruba a integração inteira.
//
// @Summary Bling webhook
// @Router /api/webhooks/bling [post]
func (h *WebhookHandler) HandleBling(c *fiber.Ctx) error {
	corpo := c.Body()
	ctx := c.UserContext()
	log := logger.From(ctx, h.logger)

	desfecho := verifyBlingSignature(corpo, c.Get(blingSignatureHeader), config.BlingClientSecret.String())

	var env BlingEnvelope
	if err := json.Unmarshal(corpo, &env); err != nil || env.Event == "" {
		// Corpo ilegível: 200 mesmo assim. Re-entregar um corpo que não sabemos
		// ler não melhora nada, e um 4xx aqui alimenta o contador de 300 erros
		// em 10 s que faz o Bling BLOQUEAR NOSSO IP por 10 minutos — para todas
		// as lojas, inclusive as que estão vendendo agora.
		log.Warn("bling webhook: corpo não é o envelope esperado",
			zap.String("assinatura", string(desfecho)))
		return c.JSON(fiber.Map{"ok": true})
	}

	log = log.With(
		zap.String("bling_event", env.Event),
		zap.String("bling_event_id", env.EventID),
		zap.String("bling_company_id", env.CompanyID),
		zap.String("assinatura", string(desfecho)),
	)

	if desfecho != signatureValid {
		// MODO OBSERVAÇÃO no primeiro deploy: registra e aceita.
		//
		// Recusar por engano um evento sem replay manual é perder o evento; o
		// Bling só re-entrega quando o erro é transitório do NOSSO lado, e um
		// 401 nosso ele trata como definitivo. Vira estrito depois de a
		// observação provar que a assinatura bate sempre.
		log.Warn("bling webhook: assinatura não confere (modo observação — aceitando)")
	}

	// POSSE. É aqui que a URL única deixa de ser um risco.
	//
	// Qualquer conta que instale o aplicativo entrega evento nesta URL,
	// inclusive contas que não são lojas do LiveCart. Sem esta checagem, um
	// evento de estoque de terceiro mexeria no espelho de uma loja nossa — a
	// mesma classe do pagamento da Shopify que caiu no LiveCart pelo Pagar.me.
	integracao, err := h.service.repo.GetActiveERPByAccount(ctx, string(providers.ProviderBling), env.CompanyID)
	if err != nil || integracao == nil {
		// 200: não é nosso, e devolver erro alimentaria o bloqueio de IP.
		log.Debug("bling webhook: evento de uma conta que não é loja do LiveCart — ignorado")
		return c.JSON(fiber.Map{"ok": true})
	}

	log = log.With(zap.String("store_id", integracao.StoreID))

	// Responder ANTES de processar. O trabalho vai para a fila; o orçamento de
	// 5 s do Bling não pode depender do que o nosso pipeline leva.
	//
	// ⚠ A entrega é FORA DE ORDEM e pode ser DUPLICADA — a doc do Bling diz as
	// duas coisas explicitamente. O consumidor precisa ser idempotente pelo
	// `eventId` (que casa com o UNIQUE(integration_id, event_id) que a tabela
	// webhook_events já tem) e descartar evento antigo pela marca d'água `date`.
	h.enfileirarEventoBling(ctx, integracao, env, log)

	return c.JSON(fiber.Map{"ok": true})
}

// cortarRecurso separa o recurso da ação num nome de evento ("stock.created"
// → "stock"). Nomeada para o teste poder afirmar o roteamento sem subir HTTP —
// e porque `virtual_stock` tem underscore, então um split ingênuo por qualquer
// separador quebraria nele.
func cortarRecurso(evento string) (recurso, acao string, ok bool) {
	return strings.Cut(evento, ".")
}

// enfileirarEventoBling despacha o evento para processamento assíncrono.
func (h *WebhookHandler) enfileirarEventoBling(
	ctx context.Context, integracao *IntegrationRow, env BlingEnvelope, log *zap.Logger,
) {
	recurso, _, _ := cortarRecurso(env.Event)

	switch recurso {
	case "stock", "virtual_stock":
		var d BlingStockData
		if err := json.Unmarshal(env.Data, &d); err != nil || d.Produto.ID == 0 {
			log.Warn("bling webhook: evento de estoque sem produto legível", zap.Error(err))
			return
		}
		// O saldo VEM NO PAYLOAD — não precisa de GET de volta. É a diferença
		// que mais alivia a cota numa live.
		log.Info("bling webhook: estoque",
			zap.Int64("produto", d.Produto.ID),
			zap.Float64("saldo_fisico", d.SaldoFisicoTotal),
			zap.Float64("saldo_virtual", d.SaldoVirtualTotal),
			zap.String("operacao", d.Operacao),
		)
		// A prova chega de graça, aqui: disponível < físico significa que o ERP
		// está segurando peça, logo a conta TEM a Reserva de estoque ligada.
		// Guardar isso é o que permite a tela responder sem sondar e o modo
		// nativo ser oferecido só a quem pode usá-lo.
		h.service.observarCapacidadeDeReserva(ctx, integracao,
			int(d.SaldoFisicoTotal), int(d.SaldoVirtualTotal))

		h.service.despacharEstoqueBling(ctx, integracao, d, env)

	case "product":
		log.Info("bling webhook: produto")
		h.service.despacharProdutoBling(ctx, integracao, env)

	case "order", "invoice":
		// ⚠ NÃO parsear `data` além do id: a forma dela não está verificada em
		// fonte nenhuma. Um GET do pedido devolve situação, notaFiscal e grade
		// de uma vez, e é imune à forma do payload.
		h.service.despacharPedidoBling(ctx, integracao, env)

	default:
		log.Debug("bling webhook: recurso sem tratamento")
	}
}

// despacharEstoqueBling processa um evento de estoque em background.
//
// Reusa ProcessProductWebhook, o MESMO caminho do Tiny: ele já coalesce por
// produto (quinze compradores no mesmo item viravam quinze releituras onde uma
// basta) e já dispara o backstop da fila de espera depois. Duplicar esse fluxo
// para o Bling seria duplicar as duas correções junto.
func (s *Service) despacharEstoqueBling(
	ctx context.Context, integracao *IntegrationRow, d BlingStockData, env BlingEnvelope,
) {
	produtoID := strconv.FormatInt(d.Produto.ID, 10)
	storeID := integracao.StoreID

	// Background com contexto próprio: quem chamou já respondeu 200 ao Bling.
	go func() {
		bg := logger.WithStore(context.WithoutCancel(ctx), storeID, "")
		log := logger.From(bg, s.logger).With(
			zap.String("bling_event_id", env.EventID),
			zap.String("external_product_id", produtoID),
		)

		aplicado, err := s.ProcessProductWebhook(bg, storeID, string(providers.ProviderBling), produtoID)
		if err != nil {
			log.Error("bling: falha ao processar webhook de estoque", zap.Error(err))
			return
		}
		if !aplicado {
			// Absorvido por uma releitura em curso. Não é erro, e o backstop da
			// fila só roda sobre estoque efetivamente escrito.
			return
		}
		if err := s.ProcessWaitlistAfterStockWebhook(bg, storeID, string(providers.ProviderBling), produtoID); err != nil {
			log.Error("bling: falha no backstop da fila de espera", zap.Error(err))
		}
	}()
}

// despacharProdutoBling reprocessa o cadastro do produto.
func (s *Service) despacharProdutoBling(ctx context.Context, integracao *IntegrationRow, env BlingEnvelope) {
	var d struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(env.Data, &d); err != nil || d.ID == 0 {
		return
	}
	produtoID := strconv.FormatInt(d.ID, 10)
	storeID := integracao.StoreID

	go func() {
		bg := logger.WithStore(context.WithoutCancel(ctx), storeID, "")
		if _, err := s.ProcessProductWebhook(bg, storeID, string(providers.ProviderBling), produtoID); err != nil {
			logger.From(bg, s.logger).Error("bling: falha ao processar webhook de produto",
				zap.String("external_product_id", produtoID), zap.Error(err))
		}
	}()
}

// despacharPedidoBling reage a uma mudança no pedido de venda.
//
// Era o buraco que fechava o ciclo pela metade: o evento chegava, era logado
// como "fase 3 pendente" e descartado. O lojista cancelou dois pedidos no
// Bling em 31/08/2026, o estoque de lá voltou corretamente, e o LiveCart
// continuou com os dois carrinhos vivos segurando 5 unidades — 10 promessas
// contra 5 peças, com o portão aberto para vender de novo o que já tinha dono.
//
// A reação em si NÃO é nova nem específica do Bling: ObserveOrderStatus já
// cancela o carrinho, marca pagamento lançado por fora e ressuscita carrinho
// quando o pedido volta a viver. Só faltava o Bling chegar até ela.
func (s *Service) despacharPedidoBling(
	ctx context.Context, integracao *IntegrationRow, env BlingEnvelope,
) {
	var d struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(env.Data, &d); err != nil || d.ID == 0 {
		logger.From(ctx, s.logger).Warn("bling webhook: evento de pedido sem id legível",
			zap.String("bling_event", env.Event), zap.Error(err))
		return
	}
	pedidoID := strconv.FormatInt(d.ID, 10)
	storeID := integracao.StoreID

	go func() {
		bg := logger.WithStore(context.WithoutCancel(ctx), storeID, "")
		log := logger.From(bg, s.logger).With(
			zap.String("bling_event", env.Event),
			zap.String("external_order_id", pedidoID),
		)

		// Coalesce por pedido. O Bling emite `order.updated` a cada mutação da
		// grade, e numa live isso é um evento por comentário do mesmo carrinho.
		// Sem isto, cada um custaria um GET do teto de 3 req/s que a live já usa
		// para criar os pedidos — a mesma correção que o espelho de estoque
		// precisou, pelo mesmo motivo.
		rodou, err := s.coalescedorDeSituacao().Fazer(storeID+"|"+pedidoID, func() error {
			return s.observarSituacaoDoPedidoBling(bg, integracao, pedidoID, env.Event)
		})
		if !rodou {
			log.Debug("bling: leitura de situação absorvida por outra em curso")
			return
		}
		if err != nil {
			log.Error("bling: falha ao observar a situação do pedido", zap.Error(err))
		}
	}()
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
func (s *Service) observarCapacidadeDeReserva(
	ctx context.Context, integracao *IntegrationRow, fisico, disponivel int,
) {
	if erp.ObservarCapacidade(fisico, disponivel) != erp.ContaReserva {
		return
	}
	if erp.CapacidadeConfirmada(integracao.Provider, integracao.Metadata) {
		return // já sabíamos
	}

	metadata := integracao.Metadata
	if metadata == nil {
		metadata = map[string]any{}
	}
	metadata[erp.ChaveCapacidadeDeReserva] = true
	if err := s.repo.UpdateMetadata(ctx, integracao.ID, metadata); err != nil {
		logger.From(ctx, s.logger).Warn("não consegui guardar a capacidade de reserva observada",
			zap.String("integration_id", integracao.ID), zap.Error(err))
		return
	}
	integracao.Metadata = metadata

	logger.From(ctx, s.logger).Info("capacidade de reserva CONFIRMADA por observação",
		zap.String("integration_id", integracao.ID),
		zap.String("provider", integracao.Provider),
		zap.Int("saldo_fisico", fisico),
		zap.Int("saldo_disponivel", disponivel),
	)
}
