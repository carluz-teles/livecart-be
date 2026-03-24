package user

import "time"

// Handler layer - Request/Response types

type GetMeResponse struct {
	ID        string    `json:"id"`
	StoreID   string    `json:"storeId"`
	Email     string    `json:"email"`
	Name      *string   `json:"name"`
	AvatarURL *string   `json:"avatarUrl"`
	Role      string    `json:"role"`
	Status    string    `json:"status"`
	StoreName string    `json:"storeName"`
	StoreSlug string    `json:"storeSlug"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

type SyncUserRequest struct {
	StoreName string `json:"storeName" validate:"required,min=2,max=100"`
	StoreSlug string `json:"storeSlug" validate:"required,min=2,max=50,slug"`
}

type GetUserStoresResponse struct {
	Stores []UserStoreResponse `json:"stores"`
}

type UserStoreResponse struct {
	ID        string    `json:"id"`
	StoreID   string    `json:"storeId"`
	Role      string    `json:"role"`
	Status    string    `json:"status"`
	StoreName string    `json:"storeName"`
	StoreSlug string    `json:"storeSlug"`
	CreatedAt time.Time `json:"createdAt"`
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
	Status    string
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
	Status    string
	StoreName string
	StoreSlug string
	CreatedAt time.Time
	UpdatedAt time.Time
}

type UpdateUserInput struct {
	ClerkUserID string
	StoreID     string
	Email       string
	Name        string
	AvatarURL   string
}

type UserStoreOutput struct {
	ID        string
	StoreID   string
	Role      string
	Status    string
	StoreName string
	StoreSlug string
	CreatedAt time.Time
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
	StoreID     string
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
	Status    string
	StoreName string
	StoreSlug string
	CreatedAt time.Time
	UpdatedAt time.Time
}
