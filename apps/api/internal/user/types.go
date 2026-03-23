package user

import "time"

// Handler layer - Request/Response types

type GetMeResponse struct {
	ID        string    `json:"id"`
	StoreID   string    `json:"store_id"`
	Email     string    `json:"email"`
	Name      *string   `json:"name"`
	AvatarURL *string   `json:"avatar_url"`
	Role      string    `json:"role"`
	StoreName string    `json:"store_name"`
	StoreSlug string    `json:"store_slug"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type SyncUserRequest struct {
	StoreName string `json:"store_name" validate:"required,min=2,max=100"`
	StoreSlug string `json:"store_slug" validate:"required,min=2,max=50,slug"`
}

// Service layer

type SyncUserInput struct {
	ClerkUserID string
	Email       string
	Name        string
	AvatarURL   string
	StoreName   string
	StoreSlug   string
}

type SyncUserOutput struct {
	ID        string
	StoreID   string
	Email     string
	Name      *string
	AvatarURL *string
	Role      string
	StoreName string
	StoreSlug string
	CreatedAt time.Time
	UpdatedAt time.Time
	IsNew     bool
}

type UserOutput struct {
	ID        string
	StoreID   string
	Email     string
	Name      *string
	AvatarURL *string
	Role      string
	StoreName string
	StoreSlug string
	CreatedAt time.Time
	UpdatedAt time.Time
}

type UpdateUserInput struct {
	ClerkUserID string
	Email       string
	Name        string
	AvatarURL   string
}

// Repository layer

type CreateUserWithStoreParams struct {
	ClerkUserID string
	Email       string
	Name        string
	AvatarURL   string
	StoreName   string
	StoreSlug   string
}

type UpdateUserParams struct {
	ClerkUserID string
	Email       string
	Name        string
	AvatarURL   string
}

type UserRow struct {
	ID        string
	StoreID   string
	Email     string
	Name      *string
	AvatarURL *string
	Role      string
	StoreName string
	StoreSlug string
	CreatedAt time.Time
	UpdatedAt time.Time
}
