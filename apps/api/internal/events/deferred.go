package events

import "errors"

// DeferredError marks expected contention. It remains an error so the queue
// retries normally and still archives/alerts if attempts are exhausted.
type DeferredError struct{ message string }

func NewDeferredError(message string) error { return &DeferredError{message: message} }
func (e *DeferredError) Error() string      { return e.message }

// IsDeferred reports whether every cause is an expected postponement. A joined
// persistence/network failure must still be logged at the normal failure level.
func IsDeferred(err error) bool {
	if _, ok := err.(*DeferredError); ok {
		return true
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		causes := joined.Unwrap()
		if len(causes) == 0 {
			return false
		}
		for _, cause := range causes {
			if !IsDeferred(cause) {
				return false
			}
		}
		return true
	}
	if cause := errors.Unwrap(err); cause != nil {
		return IsDeferred(cause)
	}
	return false
}
