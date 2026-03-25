package clerk

import (
	"context"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

var (
	ErrInvalidToken     = errors.New("invalid token")
	ErrTokenExpired     = errors.New("token expired")
	ErrMissingClaims    = errors.New("missing required claims")
	ErrJWKSFetch        = errors.New("failed to fetch JWKS")
	ErrKeyNotFound      = errors.New("signing key not found")
	ErrUserFetch        = errors.New("failed to fetch user from Clerk")
)

// Claims represents the JWT claims from Clerk
type Claims struct {
	jwt.RegisteredClaims
	UserID    string                 `json:"sub"`
	Email     string                 `json:"email,omitempty"`
	FirstName string                 `json:"first_name,omitempty"`
	LastName  string                 `json:"last_name,omitempty"`
	ImageURL  string                 `json:"image_url,omitempty"`
	Metadata  map[string]interface{} `json:"public_metadata,omitempty"`
	// Organization claims (when user has active org)
	OrgID   string `json:"org_id,omitempty"`
	OrgRole string `json:"org_role,omitempty"` // "org:admin" or "org:member"
	OrgSlug string `json:"org_slug,omitempty"`
}

// FullName returns the user's full name
func (c *Claims) FullName() string {
	if c.FirstName == "" && c.LastName == "" {
		return ""
	}
	if c.FirstName == "" {
		return c.LastName
	}
	if c.LastName == "" {
		return c.FirstName
	}
	return c.FirstName + " " + c.LastName
}

// JWK represents a JSON Web Key
type JWK struct {
	Kty string `json:"kty"`
	Kid string `json:"kid"`
	Use string `json:"use"`
	N   string `json:"n"`
	E   string `json:"e"`
	Alg string `json:"alg"`
}

// JWKS represents a JSON Web Key Set
type JWKS struct {
	Keys []JWK `json:"keys"`
}

// ClerkUser represents user data from Clerk API
type ClerkUser struct {
	ID             string `json:"id"`
	PrimaryEmailID string `json:"primary_email_address_id"`
	FirstName      string `json:"first_name"`
	LastName       string `json:"last_name"`
	ImageURL       string `json:"image_url"`
	EmailAddresses []struct {
		ID           string `json:"id"`
		EmailAddress string `json:"email_address"`
	} `json:"email_addresses"`
}

// Client handles Clerk JWT validation and API calls
type Client struct {
	jwksURL    string
	secretKey  string
	httpClient *http.Client
	jwks       *JWKS
	jwksMu     sync.RWMutex
	lastFetch  time.Time
	cacheTTL   time.Duration
}

// NewClient creates a new Clerk client
// frontendAPI should be your Clerk Frontend API domain (e.g., "clerk.your-domain.com")
// secretKey is optional - only needed if you want to use Clerk API (FetchUser)
func NewClient(frontendAPI string, secretKey ...string) *Client {
	// Remove protocol if provided
	frontendAPI = strings.TrimPrefix(frontendAPI, "https://")
	frontendAPI = strings.TrimPrefix(frontendAPI, "http://")

	c := &Client{
		jwksURL: fmt.Sprintf("https://%s/.well-known/jwks.json", frontendAPI),
		httpClient: &http.Client{
			Timeout: 10 * time.Second,
		},
		cacheTTL: 1 * time.Hour,
	}

	if len(secretKey) > 0 {
		c.secretKey = secretKey[0]
	}

	return c
}

// fetchJWKS fetches the JWKS from Clerk
func (c *Client) fetchJWKS(ctx context.Context) error {
	c.jwksMu.Lock()
	defer c.jwksMu.Unlock()

	// Check if cache is still valid
	if c.jwks != nil && time.Since(c.lastFetch) < c.cacheTTL {
		return nil
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.jwksURL, nil)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrJWKSFetch, err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrJWKSFetch, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%w: status %d", ErrJWKSFetch, resp.StatusCode)
	}

	var jwks JWKS
	if err := json.NewDecoder(resp.Body).Decode(&jwks); err != nil {
		return fmt.Errorf("%w: %v", ErrJWKSFetch, err)
	}

	c.jwks = &jwks
	c.lastFetch = time.Now()

	return nil
}

// getKey returns the signing key for the given key ID
func (c *Client) getKey(kid string) (*JWK, error) {
	c.jwksMu.RLock()
	defer c.jwksMu.RUnlock()

	if c.jwks == nil {
		return nil, ErrKeyNotFound
	}

	for _, key := range c.jwks.Keys {
		if key.Kid == kid {
			return &key, nil
		}
	}

	return nil, ErrKeyNotFound
}

// ValidateToken validates a Clerk JWT and returns the claims
func (c *Client) ValidateToken(ctx context.Context, tokenString string) (*Claims, error) {
	// Ensure we have JWKS
	if err := c.fetchJWKS(ctx); err != nil {
		return nil, err
	}

	// Parse the token
	token, err := jwt.ParseWithClaims(tokenString, &Claims{}, func(token *jwt.Token) (interface{}, error) {
		// Verify signing method
		if _, ok := token.Method.(*jwt.SigningMethodRSA); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", token.Header["alg"])
		}

		// Get the key ID
		kid, ok := token.Header["kid"].(string)
		if !ok {
			return nil, ErrKeyNotFound
		}

		// Get the key
		jwk, err := c.getKey(kid)
		if err != nil {
			// Try refreshing JWKS in case of key rotation
			if fetchErr := c.fetchJWKS(ctx); fetchErr != nil {
				return nil, fetchErr
			}
			jwk, err = c.getKey(kid)
			if err != nil {
				return nil, err
			}
		}

		// Convert JWK to RSA public key
		return jwkToRSAPublicKey(jwk)
	})

	if err != nil {
		if errors.Is(err, jwt.ErrTokenExpired) {
			return nil, ErrTokenExpired
		}
		return nil, fmt.Errorf("%w: %v", ErrInvalidToken, err)
	}

	claims, ok := token.Claims.(*Claims)
	if !ok || !token.Valid {
		return nil, ErrInvalidToken
	}

	// Validate required claims
	if claims.UserID == "" {
		return nil, ErrMissingClaims
	}

	return claims, nil
}

// jwkToRSAPublicKey converts a JWK to an RSA public key
func jwkToRSAPublicKey(jwk *JWK) (interface{}, error) {
	if jwk.Kty != "RSA" {
		return nil, fmt.Errorf("unsupported key type: %s", jwk.Kty)
	}

	// Decode N and E from base64url
	nBytes, err := jwt.NewParser().DecodeSegment(jwk.N)
	if err != nil {
		return nil, fmt.Errorf("failed to decode N: %v", err)
	}

	eBytes, err := jwt.NewParser().DecodeSegment(jwk.E)
	if err != nil {
		return nil, fmt.Errorf("failed to decode E: %v", err)
	}

	// Convert E bytes to int
	var e int
	for _, b := range eBytes {
		e = e<<8 + int(b)
	}

	// Create RSA public key
	return &rsa.PublicKey{
		N: new(big.Int).SetBytes(nBytes),
		E: e,
	}, nil
}

// FetchUser fetches user details from Clerk API
// Requires secretKey to be set in NewClient
func (c *Client) FetchUser(ctx context.Context, userID string) (*ClerkUser, error) {
	if c.secretKey == "" {
		return nil, fmt.Errorf("%w: secret key not configured", ErrUserFetch)
	}

	url := fmt.Sprintf("https://api.clerk.com/v1/users/%s", userID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUserFetch, err)
	}

	req.Header.Set("Authorization", "Bearer "+c.secretKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUserFetch, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w: status %d", ErrUserFetch, resp.StatusCode)
	}

	var user ClerkUser
	if err := json.NewDecoder(resp.Body).Decode(&user); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUserFetch, err)
	}

	return &user, nil
}

// GetPrimaryEmail extracts the primary email from ClerkUser
func (u *ClerkUser) GetPrimaryEmail() string {
	for _, email := range u.EmailAddresses {
		if email.ID == u.PrimaryEmailID {
			return email.EmailAddress
		}
	}
	// Fallback to first email if primary not found
	if len(u.EmailAddresses) > 0 {
		return u.EmailAddresses[0].EmailAddress
	}
	return ""
}

// FullName returns the user's full name
func (u *ClerkUser) FullName() string {
	if u.FirstName == "" && u.LastName == "" {
		return ""
	}
	if u.FirstName == "" {
		return u.LastName
	}
	if u.LastName == "" {
		return u.FirstName
	}
	return u.FirstName + " " + u.LastName
}
