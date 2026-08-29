package erp

// A drenagem: passar o que já está segurado por reserva MANUAL para o pedido de
// venda, sem que a peça fique livre por um segundo.
//
// No instante do corte a cantodaart tinha 690 unidades seguradas por 462 saídas
// manuais em 126 carrinhos — e 508 delas eram de uma live EM ANDAMENTO. Não é
// limpeza de lixo: é trocar o mecanismo de guarda debaixo de gente comprando.
//
// A ordem é a coisa toda. Para cada carrinho:
//
//	1. cria o pedido de venda   → o ERP passa a segurar pelo `reservado`
//	2. estorna as saídas manuais → o saldo físico volta ao lugar
//
// Nessa ordem o `disponivel` não se move: o estorno devolve N ao físico e o
// pedido já tinha tirado N do disponível. Invertida, existe uma janela em que as
// N unidades estão livres — e no meio de uma live alguém as compra.
//
// Cada linha é marcada como revertida SÓ depois de o ERP confirmar a entrada, uma
// a uma. Interromper no meio é seguro: a próxima passada continua de onde parou,
// e o que já foi estornado não é estornado de novo.
//
// Isto tem prazo de validade. Quando a tabela stock_reservations estiver vazia,
// este arquivo, o ReverseLegacyStockExit do provider e as queries de reserva saem
// juntos.

import (
	"context"
	"fmt"
	"time"

	"go.uber.org/zap"

	"livecart/apps/api/internal/integration/providers"
	"livecart/apps/api/lib/logger"
)

// CartWithLegacyReservations é um carrinho que ainda segura peça pelo modelo
// antigo.
type CartWithLegacyReservations struct {
	CartID          string
	StoreID         string
	Status          string
	PaymentStatus   string
	ExternalOrderID string
	EventStatus     string
	Rows            int
	Units           int
}

// LegacyReservationRow é uma saída manual a devolver.
type LegacyReservationRow struct {
	ID                string
	CartID            string
	ProductID         string
	ExternalProductID string
	Quantity          int
	ERPMovementID     string
}

// DrainRepository é a persistência que a drenagem precisa. Vive separada porque
// tem prazo: sai inteira quando a migração terminar.
type DrainRepository interface {
	ListCartsWithActiveReservations(ctx context.Context, storeID string) ([]CartWithLegacyReservations, error)
	ListLegacyReservationsByCart(ctx context.Context, cartID string) ([]LegacyReservationRow, error)
	// ClaimReservationForReversal reivindica a linha ANTES de falar com o ERP e
	// devolve true só para quem ganhou. É o que torna o estorno duplo impossível
	// quando a drenagem é reexecutada.
	ClaimReservationForReversal(ctx context.Context, reservationID string) (bool, error)
	// ReverseReservationByID marca a linha revertida, só depois de o ERP
	// confirmar a entrada.
	ReverseReservationByID(ctx context.Context, reservationID string) error
	// RestoreReservationToActive desfaz a reivindicação quando o ERP recusa, para
	// a próxima passada voltar a enxergar a linha.
	RestoreReservationToActive(ctx context.Context, reservationID string) error
}

// SetDrainRepository liga a persistência da drenagem.
func (s *Service) SetDrainRepository(r DrainRepository) { s.drain = r }

// DrainCartOutcome é o que aconteceu com um carrinho.
type DrainCartOutcome struct {
	CartID    string
	Units     int
	OrderID   string
	Reversed  int
	Remaining int
	Skipped   string // motivo, quando nada foi feito
	Err       string
}

// DrainReport é o resultado de uma passada.
type DrainReport struct {
	StoreID       string
	DryRun        bool
	Carts         int
	Units         int
	OrdersCreated int
	RowsReversed  int
	UnitsReversed int
	Failed        int
	Outcomes      []DrainCartOutcome
	Duration      time.Duration
	// PorTempo diz que a passada parou pelo orçamento de tempo, não por ter
	// acabado o trabalho. A tela usa para saber que há mais e seguir.
	PorTempo bool
	// JaRodando diz que outra passada tinha a trava. Nada foi feito aqui.
	JaRodando bool
}

// DrainLegacyReservations passa a guarda do estoque das saídas manuais para os
// pedidos de venda, carrinho a carrinho.
//
// dryRun percorre a mesma lista e não escreve nada — é como se confere o tamanho
// do trabalho antes de mexer numa loja com live no ar.
//
// limite corta a passada depois de N carrinhos (0 = todos). Serve para drenar em
// lotes: o teto da conta é de 30 escritas por minuto, e 126 carrinhos são cerca
// de 850 chamadas.
func (s *Service) DrainLegacyReservations(ctx context.Context, storeID string, dryRun bool, limite, maxSegundos int) (*DrainReport, error) {
	if s.drain == nil {
		return nil, fmt.Errorf("drenagem não está ligada neste processo")
	}
	inicio := time.Now()
	ctx = logger.WithStore(ctx, storeID, "")

	// UMA passada por loja de cada vez.
	//
	// A requisição estoura o prazo do navegador muito antes de a passada
	// terminar, e o servidor NÃO para junto: ele segue drenando. O cliente
	// então repete, e as duas passadas caminham sobre a mesma lista. O CAS da
	// reivindicação impede estorno duplo — foi o que salvou a migração da
	// cantodaart em 29/08 —, mas as duas competem pela mesma cota de 30
	// escritas por minuto e o resultado é metade das chamadas desperdiçada em
	// corridas perdidas, com o Tiny devolvendo 429 para todo mundo.
	//
	// Reusa a trava de finalização, que é um advisory lock por string. A chave
	// é da LOJA, não do carrinho: o que não pode acontecer duas vezes é a
	// passada inteira.
	if !dryRun {
		liberar, obteve, err := s.repo.AcquireCartFinalisationLock(ctx, "drenagem:"+storeID)
		if err != nil {
			return nil, fmt.Errorf("tomando a trava da drenagem: %w", err)
		}
		if !obteve {
			logger.From(ctx, s.logger).Info("drain skipped: another pass holds the lock")
			return &DrainReport{StoreID: storeID, JaRodando: true}, nil
		}
		defer liberar()
	}

	carrinhos, err := s.drain.ListCartsWithActiveReservations(ctx, storeID)
	if err != nil {
		return nil, fmt.Errorf("listing carts with legacy reservations: %w", err)
	}

	rel := &DrainReport{StoreID: storeID, DryRun: dryRun}
	for _, c := range carrinhos {
		rel.Carts++
		rel.Units += c.Units
	}

	logger.From(ctx, s.logger).Info("legacy reservation drain starting",
		zap.Bool("dry_run", dryRun),
		zap.Int("carts", rel.Carts),
		zap.Int("units", rel.Units),
	)
	if dryRun {
		for _, c := range carrinhos {
			rel.Outcomes = append(rel.Outcomes, DrainCartOutcome{
				CartID: c.CartID, Units: c.Units, OrderID: c.ExternalOrderID,
				Remaining: c.Rows,
			})
		}
		rel.Duration = time.Since(inicio)
		return rel, nil
	}

	erpProvider, err := s.providerFor(ctx, storeID)
	if err != nil {
		return nil, err
	}
	legado, ok := erpProvider.(interface {
		ReverseLegacyStockExit(ctx context.Context, productID string, qty int, obs string) (string, error)
	})
	if !ok {
		return nil, fmt.Errorf("o provedor deste ERP não sabe estornar saída manual")
	}

	prazo := time.Duration(maxSegundos) * time.Second
	for i, c := range carrinhos {
		if limite > 0 && i >= limite {
			break
		}
		// O orçamento de tempo é conferido ANTES de começar mais um carrinho, e
		// não durante: interromper um pela metade deixaria o pedido criado e os
		// estornos por fazer, que é o estado que a ordem das etapas existe para
		// evitar. Assim a passada devolve sempre um número inteiro de carrinhos
		// prontos, e a próxima continua do começo do seguinte.
		if prazo > 0 && i > 0 && time.Since(inicio) >= prazo {
			rel.PorTempo = true
			break
		}
		res := s.drenarCarrinho(ctx, legado, c)
		rel.Outcomes = append(rel.Outcomes, res)
		if res.OrderID != "" && c.ExternalOrderID == "" {
			rel.OrdersCreated++
		}
		rel.RowsReversed += res.Reversed
		if res.Err != "" {
			rel.Failed++
		}
	}
	rel.Duration = time.Since(inicio)

	logger.From(ctx, s.logger).Info("legacy reservation drain finished",
		zap.Int("carts", len(rel.Outcomes)),
		zap.Int("orders_created", rel.OrdersCreated),
		zap.Int("rows_reversed", rel.RowsReversed),
		zap.Int("failed", rel.Failed),
		zap.Duration("took", rel.Duration),
	)
	return rel, nil
}

// drenarCarrinho faz a troca de guarda de UM carrinho: primeiro o pedido assume,
// depois as saídas manuais são devolvidas.
func (s *Service) drenarCarrinho(ctx context.Context, legado interface {
	ReverseLegacyStockExit(ctx context.Context, productID string, qty int, obs string) (string, error)
}, c CartWithLegacyReservations) DrainCartOutcome {
	out := DrainCartOutcome{CartID: c.CartID, Units: c.Units}

	// PASSO 1 — o pedido assume a guarda.
	//
	// Antes de qualquer estorno, sempre. É este passo que faz o `disponivel` não
	// se mexer quando o físico voltar: o pedido já tirou dele o que o estorno vai
	// devolver ao saldo.
	//
	// Carrinho que já tem pedido pula direto para o estorno — a guarda já trocou
	// numa passada anterior que foi interrompida no meio.
	if c.ExternalOrderID == "" {
		if err := s.EnsureERPOrderForCart(ctx, c.CartID, c.StoreID); err != nil {
			out.Err = "criando o pedido antes de estornar: " + err.Error()
			out.Remaining = c.Rows
			logger.From(ctx, s.logger).Error("drain could not create the order; reversal NOT attempted",
				zap.String("cart_id", c.CartID), zap.Error(err))
			return out
		}
	}
	st, err := s.repo.GetCartERPOrderState(ctx, c.CartID)
	if err != nil {
		out.Err = "relendo o estado do carrinho: " + err.Error()
		out.Remaining = c.Rows
		return out
	}
	out.OrderID = st.ExternalOrderID
	if out.OrderID == "" {
		// Sem pedido não há guarda nova, e estornar aqui soltaria a peça no meio
		// da live. Este carrinho fica para a próxima passada.
		out.Skipped = "carrinho sem pedido no ERP (nenhum item vinculado?)"
		out.Remaining = c.Rows
		return out
	}

	// PASSO 2 — devolver as saídas manuais, uma a uma.
	linhas, err := s.drain.ListLegacyReservationsByCart(ctx, c.CartID)
	if err != nil {
		out.Err = "listando as reservas do carrinho: " + err.Error()
		return out
	}
	for _, l := range linhas {
		ganhou, claimErr := s.drain.ClaimReservationForReversal(ctx, l.ID)
		if claimErr != nil {
			out.Err = "reivindicando a reserva: " + claimErr.Error()
			out.Remaining++
			continue
		}
		if !ganhou {
			// Outra passada já cuidou desta linha.
			continue
		}

		obs := fmt.Sprintf("Migracao pedido-como-reserva - Cart %s", s.cartRef(ctx, c.CartID))
		erroERP := s.escreverNoERP(ctx, c.StoreID, c.CartID, func(ctx context.Context) error {
			_, err := legado.ReverseLegacyStockExit(ctx, l.ExternalProductID, l.Quantity, obs)
			return err
		})
		if erroERP != nil {
			// Devolve a linha para 'active' para a próxima passada tentar de novo.
			// Errar para o lado de "estornar de menos" é a escolha certa: falta de
			// estorno é visível no saldo; estorno a mais inventa estoque.
			if restErr := s.drain.RestoreReservationToActive(ctx, l.ID); restErr != nil {
				logger.From(ctx, s.logger).Error("drain could not release the claim after an ERP refusal — this row will not be retried",
					zap.String("reservation_id", l.ID), zap.Error(restErr))
			}
			out.Err = "estornando a saída manual: " + erroERP.Error()
			out.Remaining++
			continue
		}
		if markErr := s.drain.ReverseReservationByID(ctx, l.ID); markErr != nil {
			// O ERP JÁ devolveu a peça. Não marcar aqui faria a próxima passada
			// devolver de novo — estoque inventado. Erro alto e a linha fica.
			logger.From(ctx, s.logger).Error("CRÍTICO: o ERP devolveu a peça mas a linha não foi marcada; a próxima passada estornaria de novo",
				zap.String("reservation_id", l.ID),
				zap.String("external_product_id", l.ExternalProductID),
				zap.Int("quantity", l.Quantity),
				zap.Error(markErr))
			out.Err = "marcando a reserva revertida: " + markErr.Error()
			out.Remaining++
			continue
		}
		out.Reversed++
	}

	logger.From(ctx, s.logger).Info("cart drained",
		zap.String("cart_id", c.CartID),
		zap.String("external_order_id", out.OrderID),
		zap.Int("rows_reversed", out.Reversed),
		zap.Int("rows_remaining", out.Remaining),
	)
	return out
}

// cartRef devolve o número humano do carrinho para a observação do estorno, de
// modo que o lojista consiga casar a linha do extrato com o pedido.
func (s *Service) cartRef(ctx context.Context, cartID string) string {
	if n, err := s.repo.GetCartShortID(ctx, cartID); err == nil && n > 0 {
		return fmt.Sprintf("#%d", n)
	}
	return cartID
}

var _ = providers.ERPProvider(nil)
