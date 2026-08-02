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
// SESSION PRODUCTS (Whitelist da SESSÃO — D15/N2)
//
// A whitelist passou a ser da sessão. As rotas legadas por EVENTO continuam
// existindo (o frontend ainda as usa) e são traduzidas aqui: escrever no evento
// aplica em todas as sessões dele; ler do evento devolve a UNIÃO das sessões.
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

// ListEventWhitelist devolve a UNIÃO das whitelists das sessões do evento, uma
// linha por produto. É o que sustenta a listagem e o badge por evento.
func (r *Repository) ListEventWhitelist(ctx context.Context, eventID string) ([]SessionProductOutput, error) {
	uid, err := parseUUID(eventID)
	if err != nil {
		return nil, err
	}
	rows, err := r.q.ListEventWhitelistFromSessions(ctx, uid)
	if err != nil {
		return nil, fmt.Errorf("listing event whitelist: %w", err)
	}
	out := make([]SessionProductOutput, len(rows))
	for i, row := range rows {
		out[i] = toSessionProductOutputFromEvent(row)
	}
	return out, nil
}

// UpsertProductInAllEventSessions aplica a whitelist do EVENTO em todas as
// sessões existentes dele. Sessão criada DEPOIS nasce vazia (N2): não há
// herança, e vazia vende tudo.
func (r *Repository) UpsertProductInAllEventSessions(ctx context.Context, in SessionProductInput) error {
	eventUID, err := parseUUID(in.EventID)
	if err != nil {
		return err
	}
	productUID, err := parseUUID(in.ProductID)
	if err != nil {
		return err
	}
	specialPrice, maxQuantity := toSessionProductParams(in)
	return r.q.UpsertSessionProductForEvent(ctx, sqlc.UpsertSessionProductForEventParams{
		EventID:      eventUID,
		ProductID:    productUID,
		SpecialPrice: specialPrice,
		MaxQuantity:  maxQuantity,
		DisplayOrder: in.DisplayOrder,
		Featured:     in.Featured,
	})
}

// DeleteProductFromAllEventSessions remove o produto da whitelist de todas as
// sessões do evento.
func (r *Repository) DeleteProductFromAllEventSessions(ctx context.Context, eventID, productID string) error {
	eventUID, err := parseUUID(eventID)
	if err != nil {
		return err
	}
	productUID, err := parseUUID(productID)
	if err != nil {
		return err
	}
	return r.q.DeleteSessionProductsByEventAndProduct(ctx, sqlc.DeleteSessionProductsByEventAndProductParams{
		EventID:   eventUID,
		ProductID: productUID,
	})
}

// GetEventWhitelistProduct devolve uma linha da união por evento.
func (r *Repository) GetEventWhitelistProduct(ctx context.Context, eventID, productID string) (SessionProductOutput, error) {
	eventUID, err := parseUUID(eventID)
	if err != nil {
		return SessionProductOutput{}, err
	}
	productUID, err := parseUUID(productID)
	if err != nil {
		return SessionProductOutput{}, err
	}
	row, err := r.q.GetEventWhitelistProduct(ctx, sqlc.GetEventWhitelistProductParams{
		EventID:   eventUID,
		ProductID: productUID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return SessionProductOutput{}, httpx.ErrNotFound("produto nao esta na whitelist do evento")
		}
		return SessionProductOutput{}, fmt.Errorf("getting event whitelist product: %w", err)
	}
	return buildSessionProductOutput(
		row.ID, row.ProductID, row.ProductName, row.ProductKeyword,
		row.ProductImageUrl, row.OriginalPrice, row.SpecialPrice, row.MaxQuantity,
		row.DisplayOrder, row.Featured, row.ProductStock, row.ProductActive,
		row.CreatedAt, row.UpdatedAt,
	), nil
}

// CountEventWhitelist conta produtos DISTINTOS barrados no evento inteiro.
func (r *Repository) CountEventWhitelist(ctx context.Context, eventID string) (int, error) {
	uid, err := parseUUID(eventID)
	if err != nil {
		return 0, err
	}
	n, err := r.q.CountEventWhitelistFromSessions(ctx, uid)
	if err != nil {
		return 0, fmt.Errorf("counting event whitelist: %w", err)
	}
	return int(n), nil
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

func toSessionProductOutputFromEvent(row sqlc.ListEventWhitelistFromSessionsRow) SessionProductOutput {
	return buildSessionProductOutput(
		row.ID, row.ProductID, row.ProductName, row.ProductKeyword,
		row.ProductImageUrl, row.OriginalPrice, row.SpecialPrice, row.MaxQuantity,
		row.DisplayOrder, row.Featured, row.ProductStock, row.ProductActive,
		row.CreatedAt, row.UpdatedAt,
	)
}

// buildSessionProductOutput centraliza a conversão pgtype→DTO das três leituras
// (por sessão, por produto e a união por evento) — elas devolvem exatamente os
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
