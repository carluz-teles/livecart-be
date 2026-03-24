package store

import "time"

// Handler layer
type CreateStoreRequest struct {
	Name string `json:"name" validate:"required,min=2,max=100"`
	Slug string `json:"slug" validate:"required,min=2,max=50,alphanum"`
}

type CreateStoreResponse struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Slug      string    `json:"slug"`
	CreatedAt time.Time `json:"createdAt"`
}

type UpdateStoreRequest struct {
	Name           string `json:"name" validate:"required,min=2,max=100"`
	WhatsappNumber string `json:"whatsappNumber"`
	EmailAddress   string `json:"emailAddress"`
	SMSNumber      string `json:"smsNumber"`
}

type StoreResponse struct {
	ID             string    `json:"id"`
	Name           string    `json:"name"`
	Slug           string    `json:"slug"`
	Active         bool      `json:"active"`
	WhatsappNumber *string   `json:"whatsappNumber"`
	EmailAddress   *string   `json:"emailAddress"`
	SMSNumber      *string   `json:"smsNumber"`
	CreatedAt      time.Time `json:"createdAt"`
}

// Service layer
type CreateStoreInput struct {
	Name string
	Slug string
}

type CreateStoreOutput struct {
	ID        string
	Name      string
	Slug      string
	CreatedAt time.Time
}

type UpdateStoreInput struct {
	StoreID        string
	Name           string
	WhatsappNumber string
	EmailAddress   string
	SMSNumber      string
}

type StoreOutput struct {
	ID             string
	Name           string
	Slug           string
	Active         bool
	WhatsappNumber *string
	EmailAddress   *string
	SMSNumber      *string
	CreatedAt      time.Time
}

// Repository layer
type CreateStoreParams struct {
	Name string
	Slug string
}

type UpdateStoreParams struct {
	ID             string
	Name           string
	WhatsappNumber string
	EmailAddress   string
	SMSNumber      string
}

type StoreRow struct {
	ID             string
	Name           string
	Slug           string
	Active         bool
	WhatsappNumber *string
	EmailAddress   *string
	SMSNumber      *string
	CreatedAt      time.Time
}
