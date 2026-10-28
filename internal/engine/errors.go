package engine

import (
	"errors"
	"fmt"
)

// MuseAuthError means muse.ai explicitly rejected the session (HTTP 401, or the
// page showed a login screen). Only this error should mark an account unhealthy.
type MuseAuthError struct{ Message string }

func (e *MuseAuthError) Error() string { return e.Message }

// MuseGenerationError covers transient or structural failures. It must not
// change an account's previously confirmed health state.
type MuseGenerationError struct{ Message string }

func (e *MuseGenerationError) Error() string { return e.Message }

// MuseModelRefusalError means Muse completed the turn with text instead of the
// requested media. It is terminal: the caller must not rotate accounts.
type MuseModelRefusalError struct{ Message string }

func (e *MuseModelRefusalError) Error() string { return e.Message }

// RefImageSafetyError means a reference image source is forbidden by policy.
type RefImageSafetyError struct{ Message string }

func (e *RefImageSafetyError) Error() string { return e.Message }

func authError(format string, args ...any) error {
	return &MuseAuthError{Message: fmt.Sprintf(format, args...)}
}

func genError(format string, args ...any) error {
	return &MuseGenerationError{Message: fmt.Sprintf(format, args...)}
}

func refusalError(format string, args ...any) error {
	return &MuseModelRefusalError{Message: fmt.Sprintf(format, args...)}
}

func safetyError(format string, args ...any) error {
	return &RefImageSafetyError{Message: fmt.Sprintf(format, args...)}
}

func IsAuthError(err error) bool {
	var target *MuseAuthError
	return errors.As(err, &target)
}

func IsRefusalError(err error) bool {
	var target *MuseModelRefusalError
	return errors.As(err, &target)
}

func IsSafetyError(err error) bool {
	var target *RefImageSafetyError
	return errors.As(err, &target)
}
