package integration

// Persistência do rastreamento da situação do pedido no ERP.
//
// Satisfaz erp.ERPOrderStatusRepository. Fica aqui, e não no pacote erp, porque
// o SQL é dono deste pacote — mesma fronteira das outras portas.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"livecart/apps/api/db/sqlc"
	"livecart/apps/api/internal/erp"
)

// RecordOrderStatus grava a situação de um pedido vinculado a um carrinho.
//
// changed=false quando a situação já era essa — a query só escreve na mudança, e
// devolver zero linhas é como ela diz isso. É o caminho normal de uma
// redelivery, não um erro.
func (r *Repository) RecordOrderStatus(ctx context.Context, obs erp.ERPOrderStatusObservation) (erp.ERPOrderStatusTransition, bool, error) {
	store, err := parseUUID(obs.StoreID)
	if err != nil {
		return erp.ERPOrderStatusTransition{}, false, err
	}
	row, err := r.queries.RecordERPOrderStatus(ctx, sqlc.RecordERPOrderStatusParams{
		StoreID:         store,
		ExternalOrderID: obs.ExternalOrderID,
		OrderNumber:     obs.OrderNumber,
		Status:          string(obs.Status),
		Source:          obs.Source,
		Payload:         jsonOrNil(obs.Payload),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return erp.ERPOrderStatusTransition{}, false, nil
	}
	if err != nil {
		return erp.ERPOrderStatusTransition{}, false, err
	}
	return erp.ERPOrderStatusTransition{
		CartID:         uuidToString(row.CartID),
		PreviousStatus: row.PreviousStatus.String,
		Status:         row.Status,
		ObservedAt:     row.ObservedAt.Time,
	}, true, nil
}

// AdoptOrphanOrderStatusEvents vincula ao carrinho as passagens que chegaram
// antes de o pedido existir do lado de cá.
func (r *Repository) AdoptOrphanOrderStatusEvents(ctx context.Context, cartID, externalOrderID string) (int64, error) {
	cart, err := parseUUID(cartID)
	if err != nil {
		return 0, err
	}
	return r.queries.AdoptOrphanERPOrderStatusEvents(ctx, sqlc.AdoptOrphanERPOrderStatusEventsParams{
		CartID:          cart,
		ExternalOrderID: externalOrderID,
	})
}

// UpdateCartERPOrderNumber grava o número humano do pedido no carrinho.
func (r *Repository) UpdateCartERPOrderNumber(ctx context.Context, cartID, orderNumber string) error {
	if orderNumber == "" {
		return nil
	}
	cart, err := parseUUID(cartID)
	if err != nil {
		return err
	}
	return r.queries.UpdateCartERPOrderNumber(ctx, sqlc.UpdateCartERPOrderNumberParams{
		CartID:      cart,
		OrderNumber: pgtype.Text{String: orderNumber, Valid: true},
	})
}

// ListStaleOrderStatuses lista pedidos não terminais parados há mais que a
// janela.
func (r *Repository) ListStaleOrderStatuses(ctx context.Context, staleAfter time.Duration, limit int) ([]erp.StaleERPOrderStatus, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := r.queries.ListStaleERPOrderStatuses(ctx, sqlc.ListStaleERPOrderStatusesParams{
		StaleAfter: pgtype.Interval{Microseconds: staleAfter.Microseconds(), Valid: true},
		MaxRows:    int32(limit),
	})
	if err != nil {
		return nil, fmt.Errorf("listing stale ERP order statuses: %w", err)
	}
	out := make([]erp.StaleERPOrderStatus, 0, len(rows))
	for _, row := range rows {
		out = append(out, erp.StaleERPOrderStatus{
			CartID:          uuidToString(row.CartID),
			StoreID:         uuidToString(row.StoreID),
			ExternalOrderID: row.ExternalOrderID.String,
			Status:          row.ErpOrderStatus,
			StatusAt:        row.ErpOrderStatusAt.Time,
		})
	}
	return out, nil
}

// ListERPOrderStatusHistory devolve o trajeto do pedido de um carrinho, do mais
// recente para o mais antigo.
func (r *Repository) ListERPOrderStatusHistory(ctx context.Context, cartID string) ([]ERPOrderStatusHistoryRow, error) {
	cart, err := parseUUID(cartID)
	if err != nil {
		return nil, err
	}
	rows, err := r.queries.ListERPOrderStatusHistory(ctx, cart)
	if err != nil {
		return nil, fmt.Errorf("listing ERP order status history: %w", err)
	}
	out := make([]ERPOrderStatusHistoryRow, 0, len(rows))
	for _, row := range rows {
		out = append(out, ERPOrderStatusHistoryRow{
			ID:              uuidToString(row.ID),
			ExternalOrderID: row.ExternalOrderID,
			OrderNumber:     row.OrderNumber.String,
			Status:          row.Status,
			PreviousStatus:  row.PreviousStatus.String,
			Source:          row.Source,
			ObservedAt:      row.ObservedAt.Time,
		})
	}
	return out, nil
}

// ERPOrderStatusHistoryRow é uma passagem do pedido por um estágio.
type ERPOrderStatusHistoryRow struct {
	ID              string
	ExternalOrderID string
	OrderNumber     string
	Status          string
	PreviousStatus  string
	Source          string
	ObservedAt      time.Time
}

// jsonOrNil evita gravar `null` como literal JSON quando não há payload — a
// varredura não tem um, e uma coluna nula diz isso melhor.
func jsonOrNil(b []byte) []byte {
	if len(b) == 0 {
		return nil
	}
	return b
}

// GetCartERPOpAge devolve há quanto tempo a operação ERP em curso começou.
func (r *Repository) GetCartERPOpAge(ctx context.Context, cartID string) (time.Duration, error) {
	cart, err := parseUUID(cartID)
	if err != nil {
		return 0, err
	}
	segundos, err := r.queries.GetCartERPOpAge(ctx, cart)
	if err != nil {
		return 0, err
	}
	return time.Duration(segundos * float64(time.Second)), nil
}

// SumPromisedWithoutERPOrder devolve as unidades que a live já prometeu e que o
// ERP ainda não conhece — carrinho vivo, sem pedido criado.
func (r *Repository) SumPromisedWithoutERPOrder(ctx context.Context, externalProductID string) (int, error) {
	n, err := r.queries.SumPromisedWithoutERPOrder(ctx, pgtype.Text{String: externalProductID, Valid: true})
	return int(n), err
}
