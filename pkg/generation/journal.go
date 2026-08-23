package generation

import (
	"context"
	"errors"
)

var (
	ErrNotFound         = errors.New("generation state not found")
	ErrNewerSchema      = errors.New("generation journal schema is newer than this binary")
	ErrIntegrity        = errors.New("generation journal integrity check failed")
	ErrCursorConflict   = errors.New("provider cursor was reused with different content")
	ErrStaleCursor      = errors.New("provider cursor is stale relative to desired head")
	ErrProviderConflict = errors.New("desired provider changed without authoritative replacement")
	ErrNoLastGood       = errors.New("published predecessor required for last-good")
	ErrInvalidClosure   = errors.New("publication dependency closure is incomplete")
)

type Journal interface {
	ApplyDesired(context.Context, DesiredBatch) (ApplyTicket, error)
	LoadDesired(context.Context, uint64) (Snapshot, error)
	LoadPublished(context.Context, Domain) (PublishedGeneration, error)
	Stage(context.Context, ApplyTicket, PublicationSet) (PublicationToken, error)
	Commit(context.Context, PublicationToken) (Acknowledgement, error)
	Abort(context.Context, PublicationToken, string) error
	Revisions(context.Context) (RevisionSet, error)
	Recover(context.Context) (RecoveryState, error)
	Close() error
}
