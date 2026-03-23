package query

const (
	DefaultPage  = 1
	DefaultLimit = 20
	MaxLimit     = 100
)

// Pagination represents pagination parameters for list queries
type Pagination struct {
	Page  int `query:"page"`
	Limit int `query:"limit"`
}

// Normalize ensures pagination values are within valid ranges
func (p *Pagination) Normalize() {
	if p.Page < 1 {
		p.Page = DefaultPage
	}
	if p.Limit < 1 {
		p.Limit = DefaultLimit
	}
	if p.Limit > MaxLimit {
		p.Limit = MaxLimit
	}
}

// Offset calculates the offset for database queries
func (p Pagination) Offset() int {
	return (p.Page - 1) * p.Limit
}

// PaginationResponse represents pagination metadata in API responses
type PaginationResponse struct {
	Page       int `json:"page"`
	Limit      int `json:"limit"`
	Total      int `json:"total"`
	TotalPages int `json:"totalPages"`
}

// NewPaginationResponse creates a pagination response from query params and total count
func NewPaginationResponse(p Pagination, total int) PaginationResponse {
	totalPages := total / p.Limit
	if total%p.Limit > 0 {
		totalPages++
	}

	return PaginationResponse{
		Page:       p.Page,
		Limit:      p.Limit,
		Total:      total,
		TotalPages: totalPages,
	}
}
