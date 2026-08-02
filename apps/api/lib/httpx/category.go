package httpx

// Category classifies every backend error into one of four buckets. It drives
// server-side logging/analytics and, for DOMAIN, gates whether a stable Code is
// expected. It is NOT sent to the client as-is (the client sees Code + HTTP
// status); it is a server-side observability tag only — there is no field for it
// on Envelope.
type Category string

const (
	// CategoryDomain is a business-rule violation (cart expired, coupon not active).
	CategoryDomain Category = "DOMAIN"
	// CategoryValidation is a data-quality problem (missing/invalid field). It lives
	// on the ozzo/fields channel (errorhandler.go) and never becomes a ServiceError,
	// so it is used as a log tag only, never stored on ServiceError.Category.
	CategoryValidation Category = "VALIDATION"
	// CategoryRepository is a DB / persistence failure.
	CategoryRepository Category = "REPOSITORY"
	// CategoryInfrastructure is a network / external-service / infra failure.
	CategoryInfrastructure Category = "INFRASTRUCTURE"
)
