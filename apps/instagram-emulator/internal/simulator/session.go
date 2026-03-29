package simulator

import (
	"sync"

	"github.com/google/uuid"
)

// Session represents the current state of the simulated Instagram live
type Session struct {
	mu sync.RWMutex

	// Account info
	accountID string
	username  string

	// Live state
	liveActive bool
	mediaID    string

	// Generator for creating fake data
	generator *Generator
}

// NewSession creates a new simulator session
func NewSession(accountID, username string) *Session {
	return &Session{
		accountID: accountID,
		username:  username,
		generator: NewGenerator(),
	}
}

// StartLive starts a new live session
func (s *Session) StartLive() string {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.liveActive = true
	s.mediaID = uuid.New().String()[:20] // Simulate Instagram media ID format

	return s.mediaID
}

// EndLive ends the current live session
func (s *Session) EndLive() {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.liveActive = false
	s.mediaID = ""
}

// IsLiveActive returns whether there's an active live
func (s *Session) IsLiveActive() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.liveActive
}

// GetMediaID returns the current media ID
func (s *Session) GetMediaID() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.mediaID
}

// GetAccountID returns the account ID
func (s *Session) GetAccountID() string {
	return s.accountID
}

// GetUsername returns the username
func (s *Session) GetUsername() string {
	return s.username
}

// GetGenerator returns the data generator
func (s *Session) GetGenerator() *Generator {
	return s.generator
}
