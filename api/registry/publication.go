package registry

import (
	"context"
	"errors"
)

var ErrHistoryOperationUnsupported = errors.New("operation requires the history publication API")
var ErrHistoryConflict = errors.New("history candidate has conflicts")
var ErrHistoryRejected = errors.New("history candidate failed validation")

type HistoryReceipt struct {
	RequestID         string
	Status            string
	Message           string
	Revision          uint64
	PublishedRevision uint64
}

type HistoryDot struct {
	Actor   string
	Counter uint64
}

type PublishedState struct {
	Version    Version
	Changes    ChangeSet
	Resolution *DependencyResolution
	Context    []HistoryDot
}

type PublishedHistory interface {
	History
	SubmitChanges(context.Context, ChangeSet, *DependencyResolution) (*HistoryReceipt, error)
	AwaitPublished(context.Context, *HistoryReceipt) (*PublishedState, error)
	ReadPublished(context.Context, uint64) (*PublishedState, error)
	RestoreChanges(context.Context, uint64) (*HistoryReceipt, error)
	FollowPublished(context.Context, uint64, func(*PublishedState) error) error
	ReportApplied(context.Context, *PublishedState, error) error
}

type ContextHistory interface {
	VersionsContext(context.Context) ([]Version, error)
	GetVersionContext(context.Context, uint) (Version, error)
	GetContext(context.Context, Version) (ChangeSet, error)
}

type StateSnapshotReader interface {
	SnapshotAt(context.Context, Version) (State, error)
}
