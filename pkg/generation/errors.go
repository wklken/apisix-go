package generation

import "errors"

var (
	ErrIntegrity        = errors.New("generation publication integrity check failed")
	ErrCursorConflict   = errors.New("provider cursor was reused with different content")
	ErrProviderConflict = errors.New("desired provider changed without authoritative replacement")
	ErrNoLastGood       = errors.New("published predecessor required for last-good")
	ErrInvalidClosure   = errors.New("publication dependency closure is incomplete")
)
