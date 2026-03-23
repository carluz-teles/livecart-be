package query

// PaginatedResult represents a paginated API response
type PaginatedResult[T any] struct {
	Data       []T                `json:"data"`
	Pagination PaginationResponse `json:"pagination"`
}

// NewPaginatedResult creates a new paginated result
func NewPaginatedResult[T any](data []T, pagination Pagination, total int) PaginatedResult[T] {
	return PaginatedResult[T]{
		Data:       data,
		Pagination: NewPaginationResponse(pagination, total),
	}
}
