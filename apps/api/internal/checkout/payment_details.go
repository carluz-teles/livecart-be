package checkout

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"livecart/apps/api/lib/httpx"
)

// CartCustomerInfo is the customer identity captured at checkout (paid carts only).
// All fields default to "" when missing — older paid carts may have nothing on file.
type CartCustomerInfo struct {
	Name     string
	Document string
	Phone    string
	Email    string
}

// CartShippingAddressInfo is the delivery address captured at checkout, decoded
// from carts.shipping_address (JSONB). Optional/older paid carts may return nil.
type CartShippingAddressInfo struct {
	ZipCode      string
	Street       string
	Number       string
	Complement   string
	Neighborhood string
	City         string
	State        string
}

// CartPaymentInfo is the payment confirmation snapshot for a paid cart.
// Method is the normalized public-facing value ("pix" or "card"); the
// card-specific fields are only populated for card payments through the
// transparent checkout flow (CardBrand/LastFourDigits/Installments/
// AuthorizationCode).
type CartPaymentInfo struct {
	RawMethod         string // raw value from carts.payment_method (pix, credit_card, ...)
	CardBrand         string
	LastFourDigits    string
	Installments      int
	AuthorizationCode string
}

// ReadCartPaymentDetails returns customer + shipping address + payment metadata
// for a paid cart. Returns nil for any sub-struct whose underlying columns are
// all empty/null — the caller decides whether to expose them.
//
// We deliberately read these via raw SQL (instead of widening the sqlc-managed
// GetCartByTokenWithDetails query) to keep the post-payment receipt fields
// isolated from the existing checkout flow.
func (r *Repository) ReadCartPaymentDetails(ctx context.Context, pool *pgxpool.Pool, cartID string) (*CartCustomerInfo, *CartShippingAddressInfo, *CartPaymentInfo, error) {
	uid, err := uuid.Parse(cartID)
	if err != nil {
		return nil, nil, nil, httpx.ErrBadRequest("invalid cart ID")
	}

	var (
		customerName      pgtype.Text
		customerDocument  pgtype.Text
		customerPhone     pgtype.Text
		customerEmail     pgtype.Text
		shippingAddress   []byte
		paymentMethod     pgtype.Text
		cardBrand         pgtype.Text
		cardLastFour      pgtype.Text
		cardInstallments  pgtype.Int4
		cardAuthorization pgtype.Text
	)

	err = pool.QueryRow(ctx, `
		SELECT customer_name,
		       customer_document,
		       customer_phone,
		       customer_email,
		       shipping_address,
		       payment_method,
		       card_brand,
		       card_last_four,
		       card_installments,
		       card_authorization_code
		FROM carts
		WHERE id = $1
	`, pgtype.UUID{Bytes: uid, Valid: true}).Scan(
		&customerName, &customerDocument, &customerPhone, &customerEmail,
		&shippingAddress,
		&paymentMethod, &cardBrand, &cardLastFour, &cardInstallments, &cardAuthorization,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil, nil, nil
		}
		return nil, nil, nil, fmt.Errorf("reading cart payment details: %w", err)
	}

	customer := buildCustomerInfo(customerName, customerDocument, customerPhone, customerEmail)
	address := buildShippingAddressInfo(shippingAddress)
	payment := buildPaymentInfo(paymentMethod, cardBrand, cardLastFour, cardInstallments, cardAuthorization)

	return customer, address, payment, nil
}

// GetLatestPaidCustomerSnapshot returns the customer + shipping address from
// the most recent paid cart of the same buyer (same store + platform_user_id),
// excluding the current cart. Used by GetCartForCheckout to prefill the
// transparent checkout form for returning buyers.
//
// Returns (nil, nil, nil) when there is no prior paid cart, when platformUserID
// is empty (anonymous), or when the prior cart had nothing useful recorded
// (e.g. older paid carts predating the customer-info columns).
func (r *Repository) GetLatestPaidCustomerSnapshot(ctx context.Context, pool *pgxpool.Pool, storeID, platformUserID, excludeCartID string) (*CartCustomerInfo, *CartShippingAddressInfo, error) {
	if platformUserID == "" {
		return nil, nil, nil
	}

	storeUID, err := uuid.Parse(storeID)
	if err != nil {
		return nil, nil, httpx.ErrBadRequest("invalid store ID")
	}
	excludeUID, err := uuid.Parse(excludeCartID)
	if err != nil {
		return nil, nil, httpx.ErrBadRequest("invalid cart ID")
	}

	var (
		customerName     pgtype.Text
		customerDocument pgtype.Text
		customerPhone    pgtype.Text
		customerEmail    pgtype.Text
		shippingAddress  []byte
	)

	err = pool.QueryRow(ctx, `
		SELECT c.customer_name,
		       c.customer_document,
		       c.customer_phone,
		       c.customer_email,
		       c.shipping_address
		FROM carts c
		JOIN live_events e ON e.id = c.event_id
		WHERE e.store_id = $1
		  AND c.platform_user_id = $2
		  AND c.payment_status = 'paid'
		  AND c.id <> $3
		ORDER BY c.paid_at DESC NULLS LAST
		LIMIT 1
	`,
		pgtype.UUID{Bytes: storeUID, Valid: true},
		platformUserID,
		pgtype.UUID{Bytes: excludeUID, Valid: true},
	).Scan(&customerName, &customerDocument, &customerPhone, &customerEmail, &shippingAddress)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil, nil
		}
		return nil, nil, fmt.Errorf("reading prior paid cart snapshot: %w", err)
	}

	customer := buildCustomerInfo(customerName, customerDocument, customerPhone, customerEmail)
	address := buildShippingAddressInfo(shippingAddress)
	return customer, address, nil
}

// WriteCartCardPayment persists the brand/last4/installments/auth-code
// returned by the payment provider after a successful card authorization,
// plus the payment_method itself when not already set. Called from the
// transparent card flow on approved status; PIX never reaches here.
//
// We seed payment_method = 'credit_card' via COALESCE so we don't depend
// on the gateway webhook to populate it — in dev / loca.lt the webhook
// often never arrives and the public response was omitting the entire
// `payment` block because normalizePaymentMethod("") returned "". The
// COALESCE protects against overwriting a more-specific method (e.g.
// "debit_card") that an earlier webhook might have already recorded.
//
// authorizationCode may be empty when the gateway omitted it — we just
// leave that column NULL.
func (r *Repository) WriteCartCardPayment(ctx context.Context, pool *pgxpool.Pool, cartID, cardBrand, lastFourDigits string, installments int, authorizationCode string) error {
	uid, err := uuid.Parse(cartID)
	if err != nil {
		return httpx.ErrBadRequest("invalid cart ID")
	}
	_, err = pool.Exec(ctx, `
		UPDATE carts
		SET card_brand              = $2,
		    card_last_four          = $3,
		    card_installments       = $4,
		    card_authorization_code = $5,
		    payment_method          = COALESCE(payment_method, 'credit_card')
		WHERE id = $1
	`,
		pgtype.UUID{Bytes: uid, Valid: true},
		pgtype.Text{String: cardBrand, Valid: cardBrand != ""},
		pgtype.Text{String: lastFourDigits, Valid: lastFourDigits != ""},
		pgtype.Int4{Int32: int32(installments), Valid: installments > 0},
		pgtype.Text{String: authorizationCode, Valid: authorizationCode != ""},
	)
	if err != nil {
		return fmt.Errorf("updating cart card payment: %w", err)
	}
	return nil
}

func buildCustomerInfo(name, doc, phone, email pgtype.Text) *CartCustomerInfo {
	if !name.Valid && !doc.Valid && !phone.Valid && !email.Valid {
		return nil
	}
	return &CartCustomerInfo{
		Name:     name.String,
		Document: doc.String,
		Phone:    phone.String,
		Email:    email.String,
	}
}

func buildShippingAddressInfo(raw []byte) *CartShippingAddressInfo {
	if len(raw) == 0 {
		return nil
	}
	var addr struct {
		ZipCode      string `json:"zipCode"`
		Street       string `json:"street"`
		Number       string `json:"number"`
		Complement   string `json:"complement"`
		Neighborhood string `json:"neighborhood"`
		City         string `json:"city"`
		State        string `json:"state"`
	}
	if err := json.Unmarshal(raw, &addr); err != nil {
		return nil
	}
	if addr.ZipCode == "" && addr.Street == "" && addr.City == "" && addr.State == "" {
		return nil
	}
	return &CartShippingAddressInfo{
		ZipCode:      addr.ZipCode,
		Street:       addr.Street,
		Number:       addr.Number,
		Complement:   addr.Complement,
		Neighborhood: addr.Neighborhood,
		City:         addr.City,
		State:        addr.State,
	}
}

func buildPaymentInfo(method, brand, lastFour pgtype.Text, installments pgtype.Int4, authorization pgtype.Text) *CartPaymentInfo {
	if !method.Valid && !brand.Valid && !lastFour.Valid && !installments.Valid && !authorization.Valid {
		return nil
	}
	return &CartPaymentInfo{
		RawMethod:         method.String,
		CardBrand:         brand.String,
		LastFourDigits:    lastFour.String,
		Installments:      int(installments.Int32),
		AuthorizationCode: authorization.String,
	}
}
