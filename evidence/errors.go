package evidence

import (
	"fmt"
)

// ErrInvalidEvidence returns when evidence failed to validate
type ErrInvalidEvidence struct {
	Reason error
}

func (e ErrInvalidEvidence) Error() string {
	return fmt.Sprintf("evidence is not valid: %v ", e.Reason)
}

// ErrDatabase passes on an error caused by the db
type ErrDatabase struct {
	Cause error
}

func (e ErrDatabase) Error() string {
	return fmt.Sprintf("database error: %v", e.Cause)
}
