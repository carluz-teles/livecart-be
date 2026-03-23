package query

import "strings"

const (
	SortOrderAsc  = "asc"
	SortOrderDesc = "desc"
)

// Sorting represents sorting parameters for list queries
type Sorting struct {
	SortBy    string `query:"sortBy"`
	SortOrder string `query:"sortOrder"`
}

// Normalize ensures sorting values are valid
func (s *Sorting) Normalize(defaultSortBy string) {
	if s.SortBy == "" {
		s.SortBy = defaultSortBy
	}

	s.SortOrder = strings.ToLower(s.SortOrder)
	if s.SortOrder != SortOrderAsc && s.SortOrder != SortOrderDesc {
		s.SortOrder = SortOrderDesc
	}
}

// IsAsc returns true if sort order is ascending
func (s Sorting) IsAsc() bool {
	return s.SortOrder == SortOrderAsc
}

// OrderSQL returns the SQL ORDER BY clause fragment (e.g., "ASC" or "DESC")
func (s Sorting) OrderSQL() string {
	if s.IsAsc() {
		return "ASC"
	}
	return "DESC"
}
