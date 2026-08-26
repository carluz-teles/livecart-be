package erp

// O pedido de venda É a reserva.
//
// Um comentário na live vira um item de carrinho; o primeiro item cria o pedido
// no ERP, e cada item seguinte reenvia a grade inteira por `PUT /itens`. Criar o
// pedido já segura a peça — o saldo físico não se mexe, `reservado` sobe e
// `disponivel` desce. Medido em 26/08/2026 na conta real:
//
//	criar pedido de 1 un.   →  saldo 5 (parado) · reservado 3→4 · disponivel 2→1
//	PUT /itens 1→2 un.      →  204, e reservado acompanha: 4→5
//	cancelar (situacao=2)   →  reserva devolvida sozinha, sem mais nada
//
// Máquina de estados (coluna carts.erp_order_state):
//
//	none → converting → open ⇄ mutating → confirmed
//	                      └──────────────→ cancelled
//
// Regra sagrada: 'converting' NUNCA volta para 'none' — a chamada em voo pode ter
// sucedido do lado do ERP, e resetar criaria um segundo pedido para o mesmo
// carrinho. Quem resolve um converting sem pedido é a adoção por marcador.
//
// ═══ As três coisas que este arquivo NÃO faz, e por quê ═══
//
//  1. Não lança estoque. Nunca. O lançamento é a baixa física, e quem a faz é o
//     faturamento do lojista no ERP. Lançar durante a live baixaria o físico
//     (justamente o que o modelo evita) e travaria toda edição seguinte com
//     `400 motivosBloqueio: "estoque lançado"`.
//
//  2. Não estorna por precaução. Num pedido que apenas reservou, `estornar-estoque`
//     devolve 204 e INFLA a reserva pela quantidade do pedido, a cada chamada, sem
//     teto — medido: 2 un. levaram `reservado` de 5 a 7 a 9, e `disponivel` a −4.
//     O único estorno legítimo é o de recuperação, e só depois de a própria API
//     recusar a edição com "estoque lançado".
//
//  3. Não movimenta estoque manual. Não existe mais `POST /estoque` no sistema.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.uber.org/zap"

	"livecart/apps/api/internal/integration/providers"
	"livecart/apps/api/lib/logger"
)

func erpOrderMarker(cartID string) string { return "lc-cart-" + cartID }

// PrepareCartForPayment é o gancho da iniciação de pagamento. Hoje ele quase
// nunca tem trabalho: o pedido nasceu no primeiro comentário, muito antes do
// checkout. Continua existindo para o carrinho que chegou ao pagamento sem
// pedido — loja que ligou o ERP no meio da live, produto vinculado tarde,
// conversão que morreu no caminho.
func (s *Service) PrepareCartForPayment(ctx context.Context, cartID, storeID string) {
	ctx = logger.WithStore(ctx, storeID, "")
	if err := s.EnsureERPOrderForCart(ctx, cartID, storeID); err != nil {
		logger.From(ctx, s.logger).Warn("cart order creation on payment initiation failed",
			zap.String("cart_id", cartID),
			zap.Error(err),
		)
	}
}

// PrewarmERPContact resolve/enriquece o contato no ERP em background quando um
// cliente RECORRENTE abre o checkout, para a criação do pedido achar o cache
// quente. Erros são irrelevantes: o caminho síncrono resolve o contato de
// qualquer forma.
func (s *Service) PrewarmERPContact(ctx context.Context, storeID, platformUserID, platformHandle, name, document, email, phone string) {
	ctx = logger.WithStore(ctx, storeID, "")
	erpIntegration, err := s.repo.GetActiveByProvider(ctx, storeID, "erp", "tiny")
	if err != nil {
		return
	}
	erpProvider, err := s.collab.ResolveProvider(ctx, erpIntegration)
	if err != nil {
		return
	}
	if _, err := s.collab.ResolveERPContact(ctx, erpProvider, erpIntegration, storeID, platformUserID, platformHandle, name, document, email, phone); err != nil {
		logger.From(ctx, s.logger).Debug("ERP contact prewarm failed",
			zap.String("platform_user_id", platformUserID),
			zap.Error(err),
		)
	}
}

// =============================================================================
// CRIAÇÃO — o primeiro comentário
// =============================================================================

// EnsureERPOrderForCart cria o pedido de venda do carrinho no ERP. Single-flight
// por CAS none→converting; idempotente (estados pós-criação retornam nil sem
// tocar o ERP). Erros deixam o estado 'converting' com o external_order_id como
// marcador de progresso — a retomada acontece na próxima chamada, no confirm
// (adoção por marcador) ou na varredura; NUNCA regride a 'none'.
func (s *Service) EnsureERPOrderForCart(ctx context.Context, cartID, storeID string) error {
	st, err := s.repo.GetCartERPOrderState(ctx, cartID)
	if err != nil {
		return fmt.Errorf("loading cart ERP order state: %w", err)
	}

	switch st.State {
	case OrderStateOpen, OrderStateMutating, OrderStateConfirmed:
		return nil // já existe
	case OrderStateCancelled:
		return nil // não ressuscita carrinho cancelado
	case OrderStateConverting:
		if st.ExternalOrderID != "" {
			return s.openCartOrder(ctx, storeID, cartID, st.ExternalOrderID)
		}
		// 'converting' sem pedido é ambíguo: ou há uma criação em voo agora, ou
		// uma anterior morreu antes do POST. A diferença é o TEMPO, e ignorá-la
		// custou caro numa medição de 12 compradores simultâneos: oito
		// carrinhos ficaram sem pedido nenhum porque a primeira tentativa
		// estourou o prazo na fila do limitador, e o único caminho de volta era
		// a varredura de dez em dez minutos. Numa live, dez minutos é nunca.
		//
		// Passado o prazo de carência, o próximo comentário do mesmo carrinho
		// retoma. Criar um pedido duplicado não é risco: a retomada procura
		// primeiro pela âncora no ERP.
		return s.retomarCriacaoPresa(ctx, cartID, storeID)
	}

	erpIntegration, err := s.repo.GetActiveByProvider(ctx, storeID, "erp", "tiny")
	if err != nil {
		return nil // loja sem ERP ligado
	}

	won, err := s.repo.TransitionCartERPOrderState(ctx, cartID, OrderStateNone, OrderStateConverting)
	if err != nil {
		return fmt.Errorf("claiming cart order creation: %w", err)
	}
	if !won {
		return nil // outro comentário do mesmo carrinho ganhou a corrida
	}

	return s.criarPedidoParaCarrinho(ctx, cartID, storeID, erpIntegration)
}

// criarPedidoParaCarrinho faz a criação propriamente dita, assumindo que quem
// chama JÁ garantiu o single-flight — pelo CAS none→converting, ou por segurar a
// trava do carrinho.
//
// Está separado porque há dois donos legítimos dessa garantia, e o confirm é o
// segundo: um carrinho que ficou preso em 'converting' sem pedido (o processo
// morreu entre o CAS e o POST) não pode ser criado por EnsureERPOrderForCart —
// ela lê o estado e conclui, corretamente, que outra criação está em voo. Sem
// esta porta o carrinho ficava sem pedido para sempre, e o pagamento com ele.
func (s *Service) criarPedidoParaCarrinho(ctx context.Context, cartID, storeID string, erpIntegration *Integration) error {
	erpProvider, err := s.collab.ResolveProvider(ctx, erpIntegration)
	if err != nil {
		return fmt.Errorf("creating ERP provider: %w", err)
	}

	// Pedido em situação Aberta, sem pagamento e sem nenhuma movimentação de
	// estoque. O colaborador resolve o contato — pelo @ do comprador quando é só
	// o que temos, com nome/CPF/e-mail/telefone quando já os conhecemos — monta
	// endereço e frete se existirem, e grava o external_order_id.
	var aplicada []providers.ERPOrderItem
	createErr := s.escreverNoERP(ctx, storeID, cartID, func(ctx context.Context) error {
		var err error
		aplicada, err = s.collab.CreateERPOrderForCart(ctx, erpProvider, erpIntegration, storeID, cartID)
		return err
	})
	if createErr != nil {
		// Estado permanece 'converting' de propósito (ver regra no topo).
		return fmt.Errorf("creating ERP order for cart: %w", createErr)
	}

	fresh, err := s.repo.GetCartERPOrderState(ctx, cartID)
	if err != nil {
		return fmt.Errorf("reloading cart after order create: %w", err)
	}
	if fresh.ExternalOrderID == "" {
		// Carrinho sem nenhum item vinculado ao ERP: não há pedido a criar.
		logger.From(ctx, s.logger).Info("cart has no ERP-linked items, order creation skipped",
			zap.String("cart_id", cartID),
		)
		return nil
	}

	// A âncora de idempotência é gravada dentro da criação, pelo provider. Ela
	// custa uma escrita a mais por pedido, e é o preço de poder reencontrá-lo: o
	// filtro por `numeroOrdemCompra` — que dispensaria o carimbo — é ignorado em
	// silêncio pela API e devolve a conta inteira.
	if err := s.openCartOrder(ctx, storeID, cartID, fresh.ExternalOrderID); err != nil {
		return err
	}

	// Comentários que chegaram DURANTE a criação entraram no carrinho depois de
	// a grade ter sido montada, e nada mais os aplicaria: quem chegou enquanto o
	// estado era 'converting' desiste de propósito, confiando nesta reconciliação.
	// A mutação converge sozinha e no-opa quando não há diferença — o custo é uma
	// leitura do banco, não uma chamada ao ERP.
	if mutErr := s.mutarGrade(ctx, cartID, storeID, aplicada); mutErr != nil {
		logger.From(ctx, s.logger).Warn("could not reconcile the grid right after creating the order",
			zap.String("cart_id", cartID),
			zap.Error(mutErr),
		)
	}
	return nil
}

// grahaCriacaoPresa é quanto se espera antes de considerar que uma criação em
// 'converting' morreu. Curto porque a alternativa é o carrinho não segurar
// estoque nenhum durante a live; longo o bastante para não atropelar uma criação
// que só está na fila do limitador.
const grahaCriacaoPresa = 45 * time.Second

// tempoDeOperacaoPresa é a partir de quando a varredura considera uma operação
// abandonada. Eram dez minutos, herdados de um mundo em que a live não dependia
// dela: hoje um carrinho preso é um carrinho que não segura estoque, e dez
// minutos de live é a live inteira.
const tempoDeOperacaoPresa = 2 * time.Minute

// retomarCriacaoPresa retoma um carrinho parado em 'converting' sem pedido.
//
// Single-flight pela trava do carrinho — a mesma do confirm —, para que uma
// rajada de comentários não dispare dez retomadas do mesmo carrinho. Quem não
// pega a trava sai em silêncio: a retomada de quem pegou serve para todos.
func (s *Service) retomarCriacaoPresa(ctx context.Context, cartID, storeID string) error {
	idade, err := s.repo.GetCartERPOpAge(ctx, cartID)
	if err != nil || idade < grahaCriacaoPresa {
		// Nova demais para desconfiar: provavelmente está mesmo em voo.
		logger.From(ctx, s.logger).Debug("ERP order creation in flight for cart",
			zap.String("cart_id", cartID),
			zap.Duration("age", idade),
		)
		return nil
	}

	release, acquired, lockErr := s.repo.AcquireCartFinalisationLock(ctx, cartID)
	if lockErr != nil || !acquired {
		return nil // outra retomada está rodando
	}
	defer release()

	// Reconfere sob a trava: a retomada que ganhou a corrida pode ter acabado.
	st, err := s.repo.GetCartERPOrderState(ctx, cartID)
	if err != nil {
		return fmt.Errorf("reloading cart before resuming creation: %w", err)
	}
	if st.State != OrderStateConverting {
		return nil
	}
	if st.ExternalOrderID != "" {
		return s.openCartOrder(ctx, storeID, cartID, st.ExternalOrderID)
	}

	logger.From(ctx, s.logger).Warn("resuming an ERP order creation that got stuck",
		zap.String("cart_id", cartID),
		zap.Duration("stuck_for", idade),
	)

	// A âncora primeiro: se o POST chegou a acontecer, o pedido está lá.
	adopted, adoptErr := s.adoptOrderByMarker(ctx, cartID, storeID)
	if adoptErr != nil {
		return adoptErr
	}
	if adopted != "" {
		return s.openCartOrder(ctx, storeID, cartID, adopted)
	}

	erpIntegration, err := s.repo.GetActiveByProvider(ctx, storeID, "erp", "tiny")
	if err != nil {
		return nil
	}
	return s.criarPedidoParaCarrinho(ctx, cartID, storeID, erpIntegration)
}

// openCartOrder fecha a criação: CAS converting→open, log e espelho. O pedido já
// está segurando a peça desde o instante em que o ERP respondeu ao POST; aqui só
// se registra isso do lado de cá.
func (s *Service) openCartOrder(ctx context.Context, storeID, cartID, orderID string) error {
	moved, err := s.repo.TransitionCartERPOrderState(ctx, cartID, OrderStateConverting, OrderStateOpen)
	if err != nil {
		return fmt.Errorf("transitioning cart to open: %w", err)
	}
	if !moved {
		logger.From(ctx, s.logger).Info("cart left converting state concurrently, skipping open transition",
			zap.String("cart_id", cartID),
		)
	}
	logger.From(ctx, s.logger).Info("sales order created for cart — the order holds the stock",
		zap.String("cart_id", cartID),
		zap.String("external_order_id", orderID),
	)
	s.SeedOrderStatusOnCreate(ctx, storeID, cartID, orderID)
	s.collab.MirrorToOrder(ctx, cartID)
	return nil
}

// =============================================================================
// MUTAÇÃO — do segundo comentário em diante
// =============================================================================

// MutateERPOrderItems aplica a grade ATUAL do carrinho ao pedido. Single-flight
// via CAS open→mutating. A grade é SEMPRE reconstruída do banco — chamadas
// concorrentes convergem para o estado final do carrinho, não para deltas
// individuais, então a ordem em que elas chegam não importa.
func (s *Service) MutateERPOrderItems(ctx context.Context, cartID, storeID string) error {
	return s.mutarGrade(ctx, cartID, storeID, nil)
}

// mutarGrade é a mutação com um ponto de partida.
//
// `jaAplicada` é a grade que o pedido comprovadamente já tem — a que a criação
// acabou de enviar. Passá-la evita o `PUT` redundante do caso comum, em que
// nada mudou entre criar e reconciliar: sem ela, toda venda pagaria uma escrita
// a mais contra o teto da conta. nil significa "não sei o que o pedido tem", e
// aí a primeira passada sempre envia.
func (s *Service) mutarGrade(ctx context.Context, cartID, storeID string, jaAplicada []providers.ERPOrderItem) error {
	// Reconfere DEPOIS de soltar 'mutating'.
	//
	// A passada interna já repete enquanto o banco muda, mas ela termina numa
	// leitura e só então o estado volta para 'open'. Um comentário que caia
	// exatamente nesse vão perde o CAS (ainda é 'mutating'), desiste, e ninguém
	// mais o aplica. É estreito e acontece: numa live simulada de 15
	// compradores, uma unidade de um carrinho ficou de fora assim.
	//
	// Soltar e reconferir fecha o vão, porque a releitura acontece com o estado
	// já em 'open' — quem chegar a partir dali ganha o CAS e cuida de si.
	ja := jaAplicada
	for tentativa := 1; tentativa <= 3; tentativa++ {
		aplicada, err := s.umaPassadaDeMutacao(ctx, cartID, storeID, ja)
		if err != nil || aplicada == nil {
			return err // erro, ou perdeu o CAS e outro está cuidando
		}
		grid, gridErr := s.cartGrid(ctx, cartID)
		if gridErr != nil || mesmaGrade(aplicada, grid) {
			return nil
		}
		ja = aplicada
	}
	logger.From(ctx, s.logger).Info("grid still moving after re-checking; the next comment continues",
		zap.String("cart_id", cartID),
	)
	return nil
}

// umaPassadaDeMutacao reivindica o pedido, aplica a grade até convergir e
// devolve o carrinho para 'open'. nil,nil significa que o CAS foi perdido.
func (s *Service) umaPassadaDeMutacao(ctx context.Context, cartID, storeID string, jaAplicada []providers.ERPOrderItem) ([]providers.ERPOrderItem, error) {
	st, err := s.repo.GetCartERPOrderState(ctx, cartID)
	if err != nil {
		return nil, fmt.Errorf("loading cart ERP order state: %w", err)
	}
	if st.State != OrderStateOpen || st.ExternalOrderID == "" {
		return nil, fmt.Errorf("cart %s não está em 'open' (estado %s): %w", cartID, st.State, ErrCartNotConverted)
	}

	won, err := s.repo.TransitionCartERPOrderState(ctx, cartID, OrderStateOpen, OrderStateMutating)
	if err != nil {
		return nil, fmt.Errorf("claiming order mutation: %w", err)
	}
	if !won {
		// Outra mutação em voo: ela reconstrói a grade do banco, que já contém a
		// mudança deste chamador, e reconfere depois de soltar o estado.
		logger.From(ctx, s.logger).Info("order mutation already in flight, latest grid will win",
			zap.String("cart_id", cartID),
		)
		return nil, nil
	}
	defer func() {
		// Contexto SEM cancelamento, e essa é a parte que importa.
		//
		// A compensação não pode morrer junto com o que ela compensa. Rodando no
		// ctx original, um prazo estourado no meio da mutação levava junto o
		// UPDATE que devolve o carrinho para 'open' — e o carrinho ficava preso
		// em 'mutating', onde nenhum comentário seguinte consegue entrar. Só a
		// varredura o alcançava, minutos depois.
		//
		// Medido numa live simulada de 15 compradores: seis carrinhos travados em
		// 'mutating' e parados ali, com 10 itens no banco que nunca chegaram ao
		// pedido.
		fim := context.WithoutCancel(ctx)
		if _, backErr := s.repo.TransitionCartERPOrderState(fim, cartID, OrderStateMutating, OrderStateOpen); backErr != nil {
			logger.From(fim, s.logger).Error("failed to return cart to open after mutation",
				zap.String("cart_id", cartID),
				zap.Error(backErr),
			)
		}
		s.collab.MirrorToOrder(fim, cartID)
	}()

	return s.applyCartGridToOrder(ctx, cartID, storeID, st.ExternalOrderID, jaAplicada)
}

// applyCartGridToOrder manda a grade do banco para o pedido. Uma chamada.
//
// A exceção é o pedido travado porque alguém lançou o estoque à mão no painel do
// ERP enquanto a live rodava. Aí, e SÓ aí, o estorno destrava — e o pedido volta
// a apenas reservar, que é onde ele deveria estar. Não relançamos depois: quem
// lança é o faturamento.
func (s *Service) applyCartGridToOrder(ctx context.Context, cartID, storeID, orderID string, jaAplicada []providers.ERPOrderItem) ([]providers.ERPOrderItem, error) {
	erpIntegration, err := s.repo.GetActiveByProvider(ctx, storeID, "erp", "tiny")
	if err != nil {
		return nil, fmt.Errorf("loading ERP integration: %w", err)
	}
	erpProvider, err := s.collab.ResolveProvider(ctx, erpIntegration)
	if err != nil {
		return nil, fmt.Errorf("creating ERP provider: %w", err)
	}

	// Repete até o banco parar de mudar.
	//
	// Uma mutação lê a grade, fala com o ERP por ~1s e termina. Todo comentário
	// que entrar nessa janela cai no CAS perdedor e desiste — o que só é seguro
	// se quem ganhou enxergar a mudança dele. Quando o item é gravado DEPOIS da
	// leitura, ninguém mais o aplica, e ele fica no carrinho sem nunca chegar ao
	// pedido: numa live simulada de 15 compradores foram 11 unidades assim.
	//
	// Enquanto seguramos 'mutating' somos o único escritor daquele pedido, então
	// basta reler no fim e repetir se mudou. O teto existe porque uma rajada
	// contínua poderia girar para sempre; se ele for atingido, a próxima mutação
	// (ou a varredura) continua de onde parou.
	const maxPassadas = 4
	enviada := jaAplicada
	for passada := 1; ; passada++ {
		grid, gridErr := s.cartGrid(ctx, cartID)
		if gridErr != nil {
			return nil, gridErr
		}
		if mesmaGrade(enviada, grid) {
			return enviada, nil // convergiu: o pedido já reflete o carrinho
		}
		final, mergeErr := s.preservarLinhasDoLojista(ctx, erpProvider, orderID, grid)
		if mergeErr != nil {
			return nil, mergeErr
		}
		if err := s.enviarGrade(ctx, erpProvider, cartID, storeID, orderID, final); err != nil {
			return nil, err
		}
		enviada = grid
		if passada >= maxPassadas {
			logger.From(ctx, s.logger).Info("grid still moving after the pass limit; the next mutation continues",
				zap.String("cart_id", cartID),
				zap.Int("passes", passada),
			)
			return enviada, nil
		}
	}
}

// mesmaGrade compara duas grades por produto e quantidade. A ordem não importa:
// o que interessa é se o pedido já reflete o carrinho.
func mesmaGrade(a, b []providers.ERPOrderItem) bool {
	if a == nil || len(a) != len(b) {
		return false
	}
	porProduto := make(map[string]int, len(a))
	for _, it := range a {
		porProduto[it.ProductID] += it.Quantity
	}
	for _, it := range b {
		porProduto[it.ProductID] -= it.Quantity
	}
	for _, resto := range porProduto {
		if resto != 0 {
			return false
		}
	}
	return true
}

// preservarLinhasDoLojista relê o pedido e devolve a grade a enviar: a nossa,
// mais o que o lojista acrescentou pelo painel.
//
// A escrita é SUBSTITUIÇÃO — `PUT /itens` troca a grade inteira. Sem reler antes,
// toda linha que ele tenha digitado desaparece na próxima mutação. Medido em
// 26/08/2026: o lojista somou 3 unidades de um produto ao pedido, o comentário
// seguinte da compradora fez o LiveCart reenviar a sua grade, e a linha sumiu com
// HTTP 204 e nenhum aviso — as 3 unidades voltaram à venda enquanto ele achava
// que estavam comprometidas.
//
// A partilha:
//
//   - linha COM o nosso marcador → nossa; a grade nova a substitui;
//   - linha SEM marcador, de produto que está no carrinho → tratada como nossa.
//     É o caso dos pedidos criados ANTES do marcador existir: preservá-la e
//     mandar a nossa junto dobraria a quantidade;
//   - linha SEM marcador, de produto fora do carrinho → do lojista, preservada.
//
// O caso que essa regra NÃO distingue é o lojista somar unidades de um produto
// que a compradora já pediu: a nossa quantidade vence, porque ela vem do
// carrinho. É a ambiguidade que sobra, e ela é irredutível sem um marcador nas
// linhas antigas.
//
// Falha de leitura NÃO vira escrita cega. Pular a mutação adia o ajuste da
// reserva, e a próxima tentativa a faz; escrever sem saber apaga o trabalho de
// alguém.
func (s *Service) preservarLinhasDoLojista(ctx context.Context, erpProvider providers.ERPProvider, orderID string, nossa []providers.ERPOrderItem) ([]providers.ERPOrderItem, error) {
	atuais, err := erpProvider.GetOrderItems(ctx, orderID)
	if err != nil {
		return nil, fmt.Errorf("relendo o pedido antes de escrever (mutação adiada para não apagar linha do lojista): %w", err)
	}

	noCarrinho := make(map[string]bool, len(nossa))
	for _, it := range nossa {
		noCarrinho[it.ProductID] = true
	}

	final := make([]providers.ERPOrderItem, 0, len(nossa)+len(atuais))
	final = append(final, nossa...)
	for _, it := range atuais {
		if providers.IsLiveCartItem(it.Note) || noCarrinho[it.ProductID] {
			continue // nossa, e a grade nova já a representa
		}
		final = append(final, it)
	}

	if len(final) > len(nossa) {
		logger.From(ctx, s.logger).Info("preserving lines the merchant added by hand",
			zap.String("external_order_id", orderID),
			zap.Int("ours", len(nossa)),
			zap.Int("theirs", len(final)-len(nossa)),
		)
	}
	return final, nil
}

// enviarGrade manda a grade e trata a única recusa que autoriza um estorno.
func (s *Service) enviarGrade(ctx context.Context, erpProvider providers.ERPProvider, cartID, storeID, orderID string, grid []providers.ERPOrderItem) error {
	err := s.escreverNoERP(ctx, storeID, cartID, func(ctx context.Context) error {
		return erpProvider.UpdateOrderItems(ctx, orderID, grid)
	})
	if errors.Is(err, providers.ErrOrderStockLaunched) {
		logger.From(ctx, s.logger).Warn("order locked by manually launched stock; reversing once to edit it",
			zap.String("cart_id", cartID),
			zap.String("external_order_id", orderID),
		)
		revErr := s.escreverNoERP(ctx, storeID, cartID, func(ctx context.Context) error {
			return erpProvider.ReverseOrderStock(ctx, orderID)
		})
		if revErr != nil {
			return fmt.Errorf("reversing manually launched stock to edit order: %w", revErr)
		}
		if err := s.repo.SetCartERPStockLaunched(ctx, cartID, false); err != nil {
			logger.From(ctx, s.logger).Warn("failed to clear erp_stock_launched",
				zap.String("cart_id", cartID),
				zap.Error(err),
			)
		}
		err = s.escreverNoERP(ctx, storeID, cartID, func(ctx context.Context) error {
			return erpProvider.UpdateOrderItems(ctx, orderID, grid)
		})
	}
	if err != nil {
		return fmt.Errorf("updating order items: %w", err)
	}

	logger.From(ctx, s.logger).Info("order grid updated — reservation follows, stock untouched",
		zap.String("cart_id", cartID),
		zap.String("external_order_id", orderID),
		zap.Int("items", len(grid)),
	)
	return nil
}

// cartGrid monta a grade do pedido a partir dos itens do carrinho vinculados ao
// ERP. Grade vazia é aceita pela API mas nunca é o que o comprador quer, então
// vira erro aqui: um pedido sem itens não segura nada.
func (s *Service) cartGrid(ctx context.Context, cartID string) ([]providers.ERPOrderItem, error) {
	items, err := s.repo.ListNonWaitlistedCartItems(ctx, cartID)
	if err != nil {
		return nil, fmt.Errorf("listing cart items for order grid: %w", err)
	}
	grid := make([]providers.ERPOrderItem, 0, len(items))
	for _, item := range items {
		if item.ProductExternalID == "" {
			continue
		}
		grid = append(grid, providers.ERPOrderItem{
			ProductID: item.ProductExternalID,
			Name:      item.ProductName,
			Quantity:  item.Quantity,
			UnitPrice: item.UnitPrice,
			// Marca a linha como nossa. É o que permite, na próxima leitura,
			// distinguir o que o LiveCart escreveu do que o lojista digitou —
			// e preservar o dele. A keyword vai junto porque é o que ele
			// reconhece na tela.
			Note: strings.TrimSpace(providers.LiveCartItemMarker + " " + item.ProductKeyword),
		})
	}
	if len(grid) == 0 {
		return nil, fmt.Errorf("cart %s sem itens vinculados ao ERP para aplicar no pedido", cartID)
	}
	return grid, nil
}

// =============================================================================
// PAGAMENTO
// =============================================================================

// ConfirmERPOrderPayment fecha a venda: grava as parcelas reais do gateway e
// aprova o pedido. Duas escritas, zero movimentação de estoque.
//
// A reserva feita no primeiro comentário segue de pé e vira baixa física quando
// o lojista fatura — nós não lançamos. Devolve ErrCartNotConverted quando o
// carrinho não tem pedido nenhum e não foi possível criar um.
func (s *Service) ConfirmERPOrderPayment(ctx context.Context, cartID, storeID string, status *providers.PaymentStatus) error {
	// Mesmo claim por carrinho: webhooks de gateway chegam duplicados em
	// goroutines concorrentes, e a adoção/retomada aqui dentro não pode correr em
	// dupla. O perdedor sai — a redelivery seguinte encontra 'confirmed' e no-opa.
	release, acquired, lockErr := s.repo.AcquireCartFinalisationLock(ctx, cartID)
	if lockErr != nil {
		return fmt.Errorf("acquiring confirm lock: %w", lockErr)
	}
	if !acquired {
		logger.From(ctx, s.logger).Info("ERP confirm already in flight for cart, skipping duplicate trigger",
			zap.String("cart_id", cartID),
		)
		return nil
	}
	defer release()

	// O retrato do gateway é gravado ANTES de qualquer chamada ao ERP.
	//
	// É o que o botão de reenviar relê meses depois: sem ele, um reenvio aprova
	// a venda no ERP sem as parcelas junto, e o financeiro do lojista fica com um
	// pedido aprovado e nenhum recebimento lançado. Gravar depois não serve —
	// exatamente as tentativas que falham no meio são as que vão precisar dele.
	//
	// A gravação é um COALESCE: preserva o retrato da primeira tentativa, que é a
	// visão canônica do que o gateway disse.
	if status != nil {
		snapshot, marshalErr := json.Marshal(status)
		if marshalErr != nil {
			logger.From(ctx, s.logger).Warn("could not serialise gateway snapshot",
				zap.String("cart_id", cartID), zap.Error(marshalErr))
		} else if err := s.repo.MarkCartERPFinalisationAttempt(ctx, cartID, snapshot); err != nil {
			logger.From(ctx, s.logger).Warn("could not stamp finalisation attempt",
				zap.String("cart_id", cartID), zap.Error(err))
		}
	}

	st, err := s.repo.GetCartERPOrderState(ctx, cartID)
	if err != nil {
		return fmt.Errorf("loading cart ERP order state: %w", err)
	}

	erpIntegration, err := s.repo.GetActiveByProvider(ctx, storeID, "erp", "tiny")
	if err != nil {
		return fmt.Errorf("loading ERP integration: %w", err)
	}

	switch st.State {
	case OrderStateNone:
		// Carrinho pago sem pedido: cria agora. Acontece quando a loja ligou o
		// ERP no meio da live, ou quando o produto foi vinculado depois do
		// comentário. É tarde para segurar estoque, mas a venda tem de existir.
		logger.From(ctx, s.logger).Info("paid cart had no ERP order; creating it now",
			zap.String("cart_id", cartID),
		)
		if err := s.EnsureERPOrderForCart(ctx, cartID, storeID); err != nil {
			return fmt.Errorf("creating ERP order for paid cart: %w", err)
		}
	case OrderStateConfirmed:
		return nil // redelivery de webhook — idempotente
	case OrderStateCancelled:
		return fmt.Errorf("cart %s pago após cancelamento do pedido ERP — reconciliação manual", cartID)
	case OrderStateConverting:
		orderID := st.ExternalOrderID
		if orderID == "" {
			// A criação anterior morreu em algum ponto entre reivindicar o
			// carrinho e gravar o id. Primeiro procura o pedido pelo marcador —
			// ele pode ter nascido do lado do ERP antes do processo cair.
			adopted, adoptErr := s.adoptOrderByMarker(ctx, cartID, storeID)
			if adoptErr != nil {
				return adoptErr
			}
			orderID = adopted
			if orderID == "" {
				// Nada rastreável: cria agora. A trava do carrinho, tomada no
				// início deste método, é o single-flight — por isso a criação
				// entra pela porta direta, e não por EnsureERPOrderForCart, que
				// veria 'converting' e concluiria que há outra em voo.
				if err := s.criarPedidoParaCarrinho(ctx, cartID, storeID, erpIntegration); err != nil {
					return fmt.Errorf("creating ERP order for stuck cart: %w", err)
				}
			}
		}
		if orderID != "" {
			if err := s.openCartOrder(ctx, storeID, cartID, orderID); err != nil {
				return fmt.Errorf("opening order before confirm: %w", err)
			}
		}
	case OrderStateMutating:
		// Mutação presa (processo morreu no meio): a grade pode estar velha.
		if _, err := s.repo.TransitionCartERPOrderState(ctx, cartID, OrderStateMutating, OrderStateOpen); err != nil {
			return fmt.Errorf("unsticking mutating cart: %w", err)
		}
		if _, err := s.applyCartGridToOrder(ctx, cartID, storeID, st.ExternalOrderID, nil); err != nil {
			return fmt.Errorf("reconciling order grid before confirm: %w", err)
		}
	case OrderStateOpen:
		// caminho normal
	default:
		return fmt.Errorf("estado ERP inesperado %q para cart %s", st.State, cartID)
	}

	fresh, err := s.repo.GetCartERPOrderState(ctx, cartID)
	if err != nil {
		return fmt.Errorf("reloading cart ERP order state: %w", err)
	}
	if fresh.ExternalOrderID == "" {
		return ErrCartNotConverted
	}

	// A GRADE É RECONCILIADA ANTES DE APROVAR. Sempre.
	//
	// É a rede que o resto do sistema não consegue ser: a mutação converge por
	// releitura, mas há um vão entre a última leitura e a liberação do estado, e
	// um comentário que caia nele fica só no carrinho. Numa live simulada de 15
	// compradores isso foi uma unidade em quinze.
	//
	// No pagamento, essa diferença deixa de ser aceitável: o pedido que o
	// comprador paga tem de ser o carrinho que ele montou. Custa UM PUT por
	// VENDA — não por comentário —, porque daqui não dá para saber o que o pedido
	// tem sem perguntar, e perguntar custaria o mesmo que escrever.
	if _, recErr := s.applyCartGridToOrder(ctx, cartID, storeID, fresh.ExternalOrderID, nil); recErr != nil {
		s.collab.MarkFinalisationFailed(ctx, cartID, "reconciliação da grade antes de aprovar falhou: "+recErr.Error())
		return fmt.Errorf("reconciling grid before approving: %w", recErr)
	}

	erpProvider, err := s.collab.ResolveProvider(ctx, erpIntegration)
	if err != nil {
		return fmt.Errorf("creating ERP provider: %w", err)
	}

	if status != nil {
		items, err := s.repo.ListNonWaitlistedCartItems(ctx, cartID)
		if err != nil {
			return fmt.Errorf("listing cart items for payment total: %w", err)
		}
		var totalAmount int64
		for _, item := range items {
			if item.ProductExternalID == "" {
				continue
			}
			totalAmount += item.UnitPrice * int64(item.Quantity)
		}
		paidAt := time.Now()
		if status.PaidAt != nil {
			paidAt = *status.PaidAt
		}
		payment := &providers.ERPOrderPayment{
			Method:           status.PaymentMethod,
			PaymentID:        status.PaymentID,
			Installments:     status.Installments,
			PaidAt:           paidAt,
			Amount:           totalAmount,
			MoneyReleaseDate: status.MoneyReleaseDate,
			FeeAmountCents:   status.FeeAmountCents,
			NetAmountCents:   status.NetAmountCents,
		}
		if err := s.escreverNoERP(ctx, storeID, cartID, func(ctx context.Context) error {
			return erpProvider.UpdateOrderPayment(ctx, fresh.ExternalOrderID, payment)
		}); err != nil {
			s.collab.MarkFinalisationFailed(ctx, cartID, "gravação das parcelas falhou: "+err.Error())
			return fmt.Errorf("updating order payment: %w", err)
		}
	}

	if err := s.escreverNoERP(ctx, storeID, cartID, func(ctx context.Context) error {
		return erpProvider.SetOrderSituacao(ctx, fresh.ExternalOrderID, providers.SituacaoAprovada)
	}); err != nil {
		s.collab.MarkFinalisationFailed(ctx, cartID, "aprovação do pedido falhou: "+err.Error())
		return fmt.Errorf("approving order: %w", err)
	}

	if _, err := s.repo.TransitionCartERPOrderState(ctx, cartID, fresh.State, OrderStateConfirmed); err != nil {
		logger.From(ctx, s.logger).Error("failed to transition cart to confirmed",
			zap.String("cart_id", cartID),
			zap.Error(err),
		)
	}
	if markErr := s.repo.MarkCartERPFinalisationDone(ctx, cartID); markErr != nil {
		logger.From(ctx, s.logger).Error("failed to mark cart ERP finalisation done after confirm",
			zap.String("cart_id", cartID),
			zap.Error(markErr),
		)
	}
	s.collab.EmitERPOrderFinalized(ctx, storeID, cartID)
	logger.From(ctx, s.logger).Info("ERP order payment confirmed — two PUTs, zero stock movement",
		zap.String("cart_id", cartID),
		zap.String("external_order_id", fresh.ExternalOrderID),
	)
	s.collab.MirrorToOrder(ctx, cartID)
	return nil
}

// =============================================================================
// CANCELAMENTO E ESTORNO DE PAGAMENTO
// =============================================================================

// CancelERPOrderForCart cancela o pedido, devolvendo a reserva. UMA chamada:
// `situacao=2`.
//
// Não acompanha estorno de estoque, e isso é a correção, não um esquecimento. O
// cancelamento já devolve a reserva sozinho — medido em 26/08/2026: um pedido
// com a reserva inflada a 9 unidades voltou a 3 (o valor de outros pedidos) no
// instante do cancelamento, sem nenhuma outra chamada. Estornar junto, num
// pedido que só reservou, INFLARIA a reserva em vez de devolvê-la.
func (s *Service) CancelERPOrderForCart(ctx context.Context, cartID, storeID string) error {
	// Mesma trava do confirm, e por um motivo concreto: os dois são operações
	// TERMINAIS sobre o mesmo pedido, e o confirm reconcilia a grade antes de
	// aprovar. Sem a exclusão, essa reconciliação aterrissa depois do
	// cancelamento e o pedido cancelado volta a segurar estoque — medido numa
	// bateria de 200 rodadas de "cancelar × pagar", rodada 39.
	release, acquired, lockErr := s.repo.AcquireCartFinalisationLock(ctx, cartID)
	if lockErr != nil {
		return fmt.Errorf("acquiring cancel lock: %w", lockErr)
	}
	if !acquired {
		return fmt.Errorf("cart %s com operação terminal em voo; cancelamento adiado: %w", cartID, ErrCartBusy)
	}
	defer release()

	st, err := s.repo.GetCartERPOrderState(ctx, cartID)
	if err != nil {
		return fmt.Errorf("loading cart ERP order state: %w", err)
	}
	switch st.State {
	case OrderStateNone, OrderStateCancelled:
		return nil
	case OrderStateConfirmed:
		return fmt.Errorf("cart %s confirmado — cancelamento pós-pago é fluxo de refund", cartID)
	case OrderStateMutating:
		// Há uma escrita EM VOO neste pedido. Cancelar por cima dela produz o
		// pior desfecho possível: o cancelamento devolve a reserva, o `PUT` que
		// estava no ar aterrissa logo depois e o pedido cancelado volta a segurar
		// estoque — e o CAS de volta para 'open' falha, então nada mais reconcilia
		// aquele carrinho.
		//
		// Foi medido: com o cancelamento reivindicando a partir de 'mutating',
		// uma bateria de 200 rodadas de "expiração × comentário" produziu um
		// pedido cancelado segurando 3 unidades.
		//
		// A mutação sempre devolve o carrinho para 'open', inclusive em erro, e
		// dura menos de um segundo. Devolver erro aqui faz a retentativa do asynq
		// encontrar o carrinho parado — que é o momento certo de cancelar.
		return fmt.Errorf("cart %s com mutação em voo; cancelamento adiado: %w", cartID, ErrCartBusy)
	}
	if st.ExternalOrderID == "" {
		// converting sem pedido: nada a cancelar no ERP.
		_, _ = s.repo.TransitionCartERPOrderState(ctx, cartID, st.State, OrderStateCancelled)
		return nil
	}

	erpProvider, err := s.providerFor(ctx, storeID)
	if err != nil {
		return err
	}

	// REIVINDICA ANTES DE CANCELAR. A ordem inversa tem uma janela: enquanto o
	// estado for 'open', um comentário que chega no mesmo instante ganha o CAS
	// open→mutating e manda um `PUT /itens` DEPOIS do cancelamento — e o pedido
	// cancelado volta a segurar estoque, sem que nada no sistema saiba.
	//
	// Foi medido: numa bateria de 40 rodadas de "expiração × comentário", a
	// rodada 24 terminou com um pedido cancelado segurando 3 unidades.
	//
	// Com o estado já em 'cancelled', o CAS do comentário concorrente falha e
	// ele desiste — que é o desfecho certo para um carrinho que venceu.
	won, err := s.repo.TransitionCartERPOrderState(ctx, cartID, st.State, OrderStateCancelled)
	if err != nil {
		return fmt.Errorf("claiming cart cancellation: %w", err)
	}
	if !won {
		// Outro caminho mexeu no carrinho entre a leitura e agora (uma mutação
		// em voo, um pagamento). A próxima tentativa relê e decide.
		logger.From(ctx, s.logger).Info("cart left its state concurrently, cancellation skipped",
			zap.String("cart_id", cartID),
		)
		return nil
	}

	if err := s.escreverNoERP(ctx, storeID, cartID, func(ctx context.Context) error {
		return erpProvider.SetOrderSituacao(ctx, st.ExternalOrderID, providers.SituacaoCancelada)
	}); err != nil {
		// Devolve o estado para a retentativa refazer o ciclo inteiro. Deixá-lo
		// em 'cancelled' com o pedido vivo no ERP seria pior: o carrinho pararia
		// de ser reconciliado e a reserva ficaria presa lá para sempre.
		fim := context.WithoutCancel(ctx) // a compensação sobrevive ao prazo
		if _, backErr := s.repo.TransitionCartERPOrderState(fim, cartID, OrderStateCancelled, st.State); backErr != nil {
			logger.From(fim, s.logger).Error("failed to return cart from cancelled after ERP refusal",
				zap.String("cart_id", cartID),
				zap.Error(backErr),
			)
		}
		return fmt.Errorf("cancelling order: %w", err)
	}
	s.collab.EmitERPOrderCancelled(ctx, storeID, cartID, st.ExternalOrderID, "cancel")
	logger.From(ctx, s.logger).Info("ERP order cancelled — reservation returned by the cancel itself",
		zap.String("cart_id", cartID),
		zap.String("external_order_id", st.ExternalOrderID),
	)
	s.collab.MirrorToOrder(ctx, cartID)
	return nil
}

// RefundConvertedCartOrder cancela um pedido CONFIRMADO cujo pagamento foi
// estornado no gateway. Mesma regra do cancelamento: `situacao=2` e nada mais.
//
// Se o lojista já tiver faturado (baixando o estoque de verdade), o cancelamento
// não desfaz a baixa — e não deve mesmo: a peça saiu, e é ele quem decide se
// volta. Estornar aqui, às cegas, é que estragaria a conta.
func (s *Service) RefundConvertedCartOrder(ctx context.Context, cartID, storeID string) error {
	release, acquired, lockErr := s.repo.AcquireCartFinalisationLock(ctx, cartID)
	if lockErr != nil {
		return fmt.Errorf("acquiring refund lock: %w", lockErr)
	}
	if !acquired {
		return fmt.Errorf("cart %s com operação terminal em voo; estorno adiado: %w", cartID, ErrCartBusy)
	}
	defer release()

	st, err := s.repo.GetCartERPOrderState(ctx, cartID)
	if err != nil {
		return fmt.Errorf("loading cart ERP order state: %w", err)
	}
	if st.State != OrderStateConfirmed || st.ExternalOrderID == "" {
		return nil
	}
	erpProvider, err := s.providerFor(ctx, storeID)
	if err != nil {
		return err
	}
	// Mesma reivindicação prévia do cancelamento comum, pelo mesmo motivo.
	won, err := s.repo.TransitionCartERPOrderState(ctx, cartID, OrderStateConfirmed, OrderStateCancelled)
	if err != nil {
		return fmt.Errorf("claiming refunded cart cancellation: %w", err)
	}
	if !won {
		return nil // já cancelado por outro caminho
	}
	if err := s.escreverNoERP(ctx, storeID, cartID, func(ctx context.Context) error {
		return erpProvider.SetOrderSituacao(ctx, st.ExternalOrderID, providers.SituacaoCancelada)
	}); err != nil {
		fim := context.WithoutCancel(ctx) // a compensação sobrevive ao prazo
		if _, backErr := s.repo.TransitionCartERPOrderState(fim, cartID, OrderStateCancelled, OrderStateConfirmed); backErr != nil {
			logger.From(fim, s.logger).Error("failed to return refunded cart from cancelled after ERP refusal",
				zap.String("cart_id", cartID),
				zap.Error(backErr),
			)
		}
		return fmt.Errorf("cancelling refunded order: %w", err)
	}
	s.collab.EmitERPOrderCancelled(ctx, storeID, cartID, st.ExternalOrderID, "refund")
	logger.From(ctx, s.logger).Info("refunded ERP order cancelled",
		zap.String("cart_id", cartID),
		zap.String("external_order_id", st.ExternalOrderID),
	)
	return nil
}

// =============================================================================
// RECONCILIAÇÃO
// =============================================================================

// RunERPOrderOpsSweep reconcilia criações/mutações presas (o processo morreu no
// meio): converting com pedido → abre; converting sem pedido → tenta adotar pelo
// marcador; mutating → re-aplica a grade do banco. NUNCA regride para 'none'.
func (s *Service) RunERPOrderOpsSweep(ctx context.Context) {
	stuck, err := s.repo.ListStuckERPOrderOps(ctx, tempoDeOperacaoPresa)
	if err != nil {
		logger.From(ctx, s.logger).Error("ERP order ops sweep failed to list", zap.Error(err))
		return
	}
	for _, op := range stuck {
		opCtx := logger.WithStore(ctx, op.StoreID, "")
		switch {
		case op.State == OrderStateConverting && op.ExternalOrderID != "":
			if err := s.openCartOrder(opCtx, op.StoreID, op.CartID, op.ExternalOrderID); err != nil {
				logger.From(opCtx, s.logger).Warn("sweep failed to open stuck order",
					zap.String("cart_id", op.CartID), zap.Error(err))
			}
		case op.State == OrderStateConverting:
			// Adota se o pedido existir; CRIA se não existir.
			//
			// Antes a varredura só tentava adotar e desistia — e um carrinho cuja
			// criação morreu antes do POST ficava sem pedido para sempre, sem
			// segurar estoque nenhum, esperando um pagamento que talvez nunca
			// viesse. Medido numa live simulada: três carrinhos parados assim por
			// mais de seis minutos, com a varredura rodando 31 vezes no meio.
			if err := s.retomarCriacaoPresa(opCtx, op.CartID, op.StoreID); err != nil {
				logger.From(opCtx, s.logger).Warn("sweep failed to resume a stuck creation",
					zap.String("cart_id", op.CartID), zap.Error(err))
			}
		case op.State == OrderStateMutating && op.ExternalOrderID != "":
			if _, err := s.applyCartGridToOrder(opCtx, op.CartID, op.StoreID, op.ExternalOrderID, nil); err != nil {
				logger.From(opCtx, s.logger).Warn("sweep failed to reconcile mutating cart",
					zap.String("cart_id", op.CartID), zap.Error(err))
				continue
			}
			if _, err := s.repo.TransitionCartERPOrderState(opCtx, op.CartID, OrderStateMutating, OrderStateOpen); err != nil {
				logger.From(opCtx, s.logger).Error("sweep failed to return cart to open",
					zap.String("cart_id", op.CartID), zap.Error(err))
			}
			s.collab.MirrorToOrder(opCtx, op.CartID)
		}
	}
}

// adoptOrderByMarker reencontra, pelo marcador lc-cart-<id>, o pedido que uma
// tentativa anterior criou antes de morrer, e o vincula ao carrinho. Devolve ""
// quando não existe — aí não há nada a adotar. É o que impede que uma retomada
// crie um segundo pedido para o mesmo carrinho.
func (s *Service) adoptOrderByMarker(ctx context.Context, cartID, storeID string) (string, error) {
	erpProvider, err := s.providerFor(ctx, storeID)
	if err != nil {
		return "", nil
	}
	foundID, findErr := erpProvider.FindOrderIDByMarker(ctx, erpOrderMarker(cartID))
	if findErr != nil {
		return "", fmt.Errorf("searching order by marker for adoption: %w", findErr)
	}
	if foundID == "" {
		return "", nil
	}
	if updErr := s.repo.UpdateCartExternalOrderID(ctx, cartID, foundID); updErr != nil {
		return "", fmt.Errorf("adopting ERP order %s: %w", foundID, updErr)
	}
	logger.From(ctx, s.logger).Info("adopted orphan ERP order via marker",
		zap.String("cart_id", cartID),
		zap.String("external_order_id", foundID),
	)
	return foundID, nil
}

// providerFor resolve o cliente do ERP ativo da loja.
func (s *Service) providerFor(ctx context.Context, storeID string) (providers.ERPProvider, error) {
	erpIntegration, err := s.repo.GetActiveByProvider(ctx, storeID, "erp", "tiny")
	if err != nil {
		return nil, fmt.Errorf("loading ERP integration: %w", err)
	}
	erpProvider, err := s.collab.ResolveProvider(ctx, erpIntegration)
	if err != nil {
		return nil, fmt.Errorf("creating ERP provider: %w", err)
	}
	return erpProvider, nil
}

// CheckTinyStockWebhookDelivery é o health-check de ENTREGA de webhook:
// integração ativa sem NENHUM evento de estoque na janela é quase certeza de URL
// removida pelo lado do ERP (eles apagam o cadastro após falhas consecutivas e
// param de entregar em silêncio). Loga em ERROR com dedupe de 24h por integração.
func (s *Service) CheckTinyStockWebhookDelivery(ctx context.Context, staleAfter time.Duration) {
	stale, err := s.repo.ListTinyIntegrationsWithStaleStockWebhook(ctx, staleAfter)
	if err != nil {
		logger.From(ctx, s.logger).Error("stock webhook delivery check failed to list", zap.Error(err))
		return
	}
	for _, integ := range stale {
		itemCtx := logger.WithStore(ctx, integ.StoreID, "")
		fields := []zap.Field{
			zap.String("integration_id", integ.IntegrationID),
			zap.Duration("stale_after", staleAfter),
		}
		if integ.LastStockEventAt != nil {
			fields = append(fields, zap.Time("last_stock_event_at", *integ.LastStockEventAt))
		} else {
			fields = append(fields, zap.String("last_stock_event_at", "nunca"))
		}
		logger.From(itemCtx, s.logger).Error("WEBHOOK DO ERP POSSIVELMENTE REMOVIDO: sem eventos de estoque na janela — recadastrar a URL no painel", fields...)
		if stampErr := s.repo.StampIntegrationStockWebhookAlert(itemCtx, integ.IntegrationID); stampErr != nil {
			logger.From(itemCtx, s.logger).Warn("failed to stamp stock webhook alert",
				zap.String("integration_id", integ.IntegrationID),
				zap.Error(stampErr),
			)
		}
	}
}
