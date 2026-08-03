package live

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"livecart/apps/api/db/sqlc"
	"livecart/apps/api/lib/httpx"
)

// =============================================================================
// SESSION PRODUCTS — a lista de produtos vendáveis é da TRANSMISSÃO
//
// A lista pertence à SESSÃO e só a ela: uma live vende qualquer coisa, um post
// vende só o produto X e um story só o produto Y, e os três podem ser
// transmissões da MESMA campanha. Não há leitura nem escrita por evento aqui —
// as funções que traduziam "evento" para "todas as sessões" saíram junto com as
// rotas que as chamavam.
//
// Lista vazia = todos os produtos ativos da loja liberados naquela transmissão.
// =============================================================================

func toSessionProductParams(in SessionProductInput) (pgtype.Int4, pgtype.Int4) {
	var specialPrice pgtype.Int4
	if in.SpecialPrice != nil {
		specialPrice = pgtype.Int4{Int32: int32(*in.SpecialPrice), Valid: true}
	}
	var maxQuantity pgtype.Int4
	if in.MaxQuantity != nil {
		maxQuantity = pgtype.Int4{Int32: *in.MaxQuantity, Valid: true}
	}
	return specialPrice, maxQuantity
}

// UpsertSessionProduct grava (ou regrava) o produto na whitelist da sessão.
// A chave natural é (sessão, produto): POST e PUT são a mesma operação.
func (r *Repository) UpsertSessionProduct(ctx context.Context, in SessionProductInput) (SessionProductOutput, error) {
	sessionUID, err := parseUUID(in.SessionID)
	if err != nil {
		return SessionProductOutput{}, err
	}
	productUID, err := parseUUID(in.ProductID)
	if err != nil {
		return SessionProductOutput{}, err
	}
	specialPrice, maxQuantity := toSessionProductParams(in)

	if _, err := r.q.UpsertSessionProduct(ctx, sqlc.UpsertSessionProductParams{
		SessionID:    sessionUID,
		ProductID:    productUID,
		SpecialPrice: specialPrice,
		MaxQuantity:  maxQuantity,
		DisplayOrder: in.DisplayOrder,
		Featured:     in.Featured,
	}); err != nil {
		return SessionProductOutput{}, fmt.Errorf("upserting session product: %w", err)
	}

	row, err := r.q.GetSessionProductByProductID(ctx, sqlc.GetSessionProductByProductIDParams{
		SessionID: sessionUID,
		ProductID: productUID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return SessionProductOutput{}, httpx.ErrNotFound("session product not found")
		}
		return SessionProductOutput{}, fmt.Errorf("getting session product: %w", err)
	}
	return toSessionProductOutput(row), nil
}

// ListSessionProducts devolve a whitelist DAQUELA sessão. Lista vazia significa
// "vende tudo" — a ausência de linha é a regra, não um erro.
func (r *Repository) ListSessionProducts(ctx context.Context, sessionID string) ([]SessionProductOutput, error) {
	uid, err := parseUUID(sessionID)
	if err != nil {
		return nil, err
	}
	rows, err := r.q.ListSessionProducts(ctx, uid)
	if err != nil {
		return nil, fmt.Errorf("listing session products: %w", err)
	}
	out := make([]SessionProductOutput, len(rows))
	for i, row := range rows {
		out[i] = toSessionProductOutputFromList(row)
	}
	return out, nil
}

// DeleteSessionProduct remove o produto da whitelist da sessão.
func (r *Repository) DeleteSessionProduct(ctx context.Context, sessionID, productID string) error {
	sessionUID, err := parseUUID(sessionID)
	if err != nil {
		return err
	}
	productUID, err := parseUUID(productID)
	if err != nil {
		return err
	}
	return r.q.DeleteSessionProduct(ctx, sqlc.DeleteSessionProductParams{
		SessionID: sessionUID,
		ProductID: productUID,
	})
}

// CountSessionProducts conta a whitelist da sessão.
func (r *Repository) CountSessionProducts(ctx context.Context, sessionID string) (int, error) {
	uid, err := parseUUID(sessionID)
	if err != nil {
		return 0, err
	}
	n, err := r.q.CountSessionProducts(ctx, uid)
	if err != nil {
		return 0, fmt.Errorf("counting session products: %w", err)
	}
	return int(n), nil
}

// CountSessionProductsByEvent devolve, numa leitura só, quantos produtos cada
// transmissão da campanha libera. Sessão ausente do mapa = zero = vende tudo.
func (r *Repository) CountSessionProductsByEvent(ctx context.Context, eventID string) (map[string]int, error) {
	uid, err := parseUUID(eventID)
	if err != nil {
		return nil, err
	}
	rows, err := r.q.CountSessionProductsByEvent(ctx, uid)
	if err != nil {
		return nil, fmt.Errorf("counting session products by event: %w", err)
	}
	out := make(map[string]int, len(rows))
	for _, row := range rows {
		out[row.SessionID.String()] = int(row.ProductCount)
	}
	return out, nil
}

func toSessionProductOutput(row sqlc.GetSessionProductByProductIDRow) SessionProductOutput {
	return buildSessionProductOutput(
		row.ID, row.ProductID, row.ProductName, row.ProductKeyword,
		row.ProductImageUrl, row.OriginalPrice, row.SpecialPrice, row.MaxQuantity,
		row.DisplayOrder, row.Featured, row.ProductStock, row.ProductActive,
		row.CreatedAt, row.UpdatedAt,
	)
}

func toSessionProductOutputFromList(row sqlc.ListSessionProductsRow) SessionProductOutput {
	return buildSessionProductOutput(
		row.ID, row.ProductID, row.ProductName, row.ProductKeyword,
		row.ProductImageUrl, row.OriginalPrice, row.SpecialPrice, row.MaxQuantity,
		row.DisplayOrder, row.Featured, row.ProductStock, row.ProductActive,
		row.CreatedAt, row.UpdatedAt,
	)
}

// buildSessionProductOutput centraliza a conversão pgtype→DTO das duas leituras
// (a lista da sessão e a linha de um produto) — elas devolvem exatamente os
// mesmos campos e duplicar a conversão é como o cálculo de preço efetivo já se
// espalhou por três funções no lado do evento.
func buildSessionProductOutput(
	id, productID pgtype.UUID,
	name, keyword string,
	imageURL pgtype.Text,
	originalPrice pgtype.Int8,
	specialPriceCol, maxQuantityCol pgtype.Int4,
	displayOrder int32,
	featured bool,
	stock pgtype.Int4,
	active pgtype.Bool,
	createdAt, updatedAt pgtype.Timestamptz,
) SessionProductOutput {
	var specialPrice *int64
	if specialPriceCol.Valid {
		v := int64(specialPriceCol.Int32)
		specialPrice = &v
	}
	var maxQuantity *int32
	if maxQuantityCol.Valid {
		maxQuantity = &maxQuantityCol.Int32
	}
	var image *string
	if imageURL.Valid {
		image = &imageURL.String
	}
	effectivePrice := originalPrice.Int64
	if specialPrice != nil {
		effectivePrice = *specialPrice
	}
	return SessionProductOutput{
		ID:             id.String(),
		ProductID:      productID.String(),
		Name:           name,
		Keyword:        keyword,
		ImageURL:       image,
		OriginalPrice:  originalPrice.Int64,
		SpecialPrice:   specialPrice,
		EffectivePrice: effectivePrice,
		MaxQuantity:    maxQuantity,
		DisplayOrder:   displayOrder,
		Featured:       featured,
		Stock:          stock.Int32,
		ProductActive:  active.Bool,
		CreatedAt:      createdAt.Time,
		UpdatedAt:      updatedAt.Time,
	}
}
