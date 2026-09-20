// Package store owns durable raw evidence and rebuildable query projections.
package store

import (
	"encoding/json"
	"errors"
	"time"

	"github.com/evanchakrin/agent-mission-control/internal/protocol"
)

var (
	ErrConflict = errors.New("conflict")
	ErrNotFound = errors.New("not found")
	ErrInvalid  = errors.New("invalid input")
	ErrCapacity = errors.New("storage reserve reached")
)

// Options limits work, never historical record count. AvailableBytes may be
// supplied by the host's volume policy; zero ReserveBytes disables that policy.
type Options struct {
	// Managed parser children share their supervising hub's checkpoint owner.
	// Standalone stores leave this false and own their checkpoint lifecycle.
	ExternalCheckpointOwner bool
	ReserveBytes            int64
	AvailableBytes          func(string) (int64, error)
	// BeforeCommit is a fault-injection seam for storage integration tests.
	BeforeCommit func() error
	// AfterAttributionVerification injects failures/races after the read proof,
	// before the writer is acquired. Production callers leave it nil.
	AfterAttributionVerification func() error
}

type SourceState struct {
	Source             protocol.Source `json:"source"`
	DurableOffset      int64           `json:"durableOffset"`
	IndexedOffset      int64           `json:"indexedOffset"`
	ParserState        json.RawMessage `json:"parserState,omitempty"`
	ExternalIndex      bool            `json:"externalIndex"`
	ProjectionRevision string          `json:"projectionRevision"`
}

type Session struct {
	ID                 string          `json:"id"`
	MachineID          string          `json:"machineId"`
	MachineName        string          `json:"machineName,omitempty"`
	SourceID           string          `json:"sourceId"`
	Generation         string          `json:"generation"`
	ProjectionRevision string          `json:"projectionRevision"`
	Provider           string          `json:"provider"`
	NativeID           string          `json:"nativeId"`
	NativeAgentID      string          `json:"nativeAgentId,omitempty"`
	ParentThreadID     string          `json:"parentThreadId,omitempty"`
	ForkedFromID       string          `json:"forkedFromId,omitempty"`
	Title              string          `json:"title"`
	Project            string          `json:"project"`
	LastActivity       time.Time       `json:"lastActivity"`
	EventCount         int64           `json:"eventCount"`
	TokensIn           int64           `json:"tokensIn"`
	TokensCache        int64           `json:"tokensCache"`
	TokensCacheWrite   int64           `json:"tokensCacheWrite"`
	TokensOut          int64           `json:"tokensOut"`
	Models             json.RawMessage `json:"models,omitempty"`
	Completeness       string          `json:"completeness"`
	CostEstimate       *float64        `json:"costEstimate"`
	Pricing            *SessionPricing `json:"pricing,omitempty"`
	Metadata           Metadata        `json:"metadata"`
}

// SessionPricing references an immutable historical estimate at the session's
// current indexing checkpoint. Coverage is over recorded usage, not invoices.
type SessionPricing struct {
	Components         *CostComponents `json:"components,omitempty"`
	SnapshotID         string          `json:"snapshotId"`
	CatalogID          string          `json:"catalogId"`
	Context            string          `json:"context"`
	Cost               float64         `json:"cost"`
	RecordedTokens     int64           `json:"recordedTokens"`
	PricedTokens       int64           `json:"pricedTokens"`
	UnpricedTokens     int64           `json:"unpricedTokens"`
	UnattributedTokens int64           `json:"unattributedTokens"`
	Coverage           float64         `json:"coverage"`
}

type PricingTotals struct {
	RecordedTokens              int64    `json:"recordedTokens"`
	PricedTokens                int64    `json:"pricedTokens"`
	UnpricedTokens              int64    `json:"unpricedTokens"`
	TokensAwaitingPricing       int64    `json:"tokensAwaitingPricing"`
	KnownUnattributedTokens     int64    `json:"knownUnattributedTokens"`
	SessionsWithPricingCoverage int64    `json:"sessionsWithPricingCoverage"`
	TokenPricingCoverage        *float64 `json:"tokenPricingCoverage"`
}

type Event struct {
	ID           string    `json:"id"`
	SessionID    string    `json:"sessionId"`
	AgentID      string    `json:"agentId"`
	Kind         string    `json:"kind"`
	Timestamp    time.Time `json:"timestamp"`
	SourceOffset int64     `json:"sourceOffset"`
	SourceLength int64     `json:"sourceLength"`
	Text         string    `json:"text"`
	// SearchText is transient parser output used only by the search index.
	SearchText         string          `json:"-"`
	Data               json.RawMessage `json:"data,omitempty"`
	DedupeKey          string          `json:"dedupeKey,omitempty"`
	Sequence           int64           `json:"sequence"`
	ProjectionRevision string          `json:"projectionRevision"`
	// HistorySnapshot is a read-time navigation binding, never parser evidence.
	HistorySnapshot string `json:"historySnapshot,omitempty"`
	// AgentDisplayName is current organization data, not transcript evidence.
	AgentDisplayName string `json:"agentDisplayName,omitempty"`
	// SearchContext is current read-time organization, not source evidence.
	SearchContext *EventSearchContext `json:"searchContext,omitempty"`
	ToolResult    *ToolResultEvidence `json:"toolResult,omitempty"`
}

// ToolResultEvidence describes correlation, not whether the delegated goal was met.
type ToolResultEvidence struct {
	State          string `json:"state"`
	ResultSequence *int64 `json:"resultSequence,omitempty"`
	Error          *bool  `json:"error,omitempty"`
}

type EventSearchContext struct {
	SessionTitle string `json:"sessionTitle"`
	MachineID    string `json:"machineId"`
	MachineName  string `json:"machineName"`
	Provider     string `json:"provider"`
	Archived     bool   `json:"archived"`
}

// UsageObservation preserves provider evidence; prices are derived separately.
// CounterScope must describe the verified provider scope, not an inferred sum.
type UsageObservation struct {
	ID               string          `json:"id"`
	SessionID        string          `json:"sessionId"`
	AgentID          string          `json:"agentId"`
	Model            string          `json:"model"`
	Timestamp        time.Time       `json:"timestamp"`
	CounterScope     string          `json:"counterScope"`
	Kind             string          `json:"kind"`
	TokensIn         int64           `json:"tokensIn"`
	TokensCache      int64           `json:"tokensCache"`
	TokensCacheWrite int64           `json:"tokensCacheWrite"`
	TokensOut        int64           `json:"tokensOut"`
	Evidence         json.RawMessage `json:"evidence,omitempty"`
	// Present names the fields actually reported by a partial provider message.
	// Nil retains full-replacement semantics for non-message observations. The
	// stored union distinguishes unknown buckets from provider-reported zeroes.
	Present []string `json:"present,omitempty"`
}

type IndexBatch struct {
	SourceID           string
	Generation         string
	ProjectionRevision string
	FromOffset         int64
	ToOffset           int64
	Session            Session
	Events             []Event
	Usage              []UsageObservation
	ParserState        json.RawMessage
}

// Cursor is an opaque continuation token. Limits default to 100 and cap at 500.
type SessionQuery struct {
	Cursor                    string
	Limit                     int
	MachineID                 string
	Provider                  string
	Project                   string
	ProjectAssignment         *string // nil: any; empty: no owner assignment, independent of source path
	Archived                  *bool
	Text                      string
	Sort                      string
	Direction                 string
	PinnedFirst               bool
	UnassignedProject         bool
	MissingJavaScriptEvidence bool // current projection only; diagnostics are not missing evidence
	MissingUndoEvidence       bool // absent current undo checkpoint; unsupported syntax is not missing evidence
	From                      *time.Time
	To                        *time.Time
}
type SessionPage struct {
	Sessions   []Session `json:"sessions"`
	NextCursor string    `json:"nextCursor,omitempty"`
}
type EventPage struct {
	Snapshot     string  `json:"snapshot,omitempty"`
	Events       []Event `json:"events"`
	NextSequence int64   `json:"nextSequence,omitempty"`
}
type SearchQuery struct {
	Text          string
	DelegatedOnly bool
	Related       bool // top five lexical matches; no sequence continuation
	SessionID     string
	AfterSequence int64
	Limit         int
}

type Metadata struct {
	Archived bool     `json:"archived"`
	Pinned   bool     `json:"pinned"`
	Name     string   `json:"name,omitempty"`
	Note     string   `json:"note,omitempty"`
	Tags     []string `json:"tags,omitempty"`
	Project  string   `json:"project,omitempty"`
	// ProjectOverride distinguishes explicit unassignment from source inheritance.
	// Older nonempty Project values remain overrides without this marker.
	ProjectOverride bool      `json:"projectOverride,omitempty"`
	Revision        int64     `json:"revision"`
	UpdatedAt       time.Time `json:"updatedAt"`
}
type MetadataPatch struct {
	AgentName   *AgentNamePatch `json:"agentName,omitempty"`
	Archived    *bool           `json:"archived,omitempty"`
	Pinned      *bool           `json:"pinned,omitempty"`
	Name        *string         `json:"name,omitempty"`
	Note        *string         `json:"note,omitempty"`
	Tags        *[]string       `json:"tags,omitempty"`
	Project     *string         `json:"project,omitempty"`
	Revision    int64           `json:"revision"`
	OperationID string          `json:"operationId"`
}

type AgentNamePatch struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type Machine struct {
	Heartbeat protocol.Heartbeat `json:"heartbeat"`
	LastSeen  time.Time          `json:"lastSeen"`
	Label     MachineLabel       `json:"label"`
}

type BackupManifest struct {
	LegacyAssetsSHA256 string    `json:"legacyAssetsSHA256,omitempty"`
	Version            int       `json:"version"`
	CreatedAt          time.Time `json:"createdAt"`
	RecoveryEpoch      string    `json:"recoveryEpoch"`
	DatabaseSHA256     string    `json:"databaseSHA256"`
	BlobCount          int64     `json:"blobCount"`
	RawBytes           int64     `json:"rawBytes"`
}

type Totals struct {
	PricingTotals
	CurrentComparisons      *ComparisonTotals `json:"currentComparisons,omitempty"`
	Sessions                int64             `json:"sessions"`
	Events                  int64             `json:"events"`
	TokensIn                int64             `json:"tokensIn"`
	TokensCache             int64             `json:"tokensCache"`
	TokensCacheWrite        int64             `json:"tokensCacheWrite"`
	TokensOut               int64             `json:"tokensOut"`
	CostEstimate            float64           `json:"costEstimate"`
	SessionsWithEstimate    int64             `json:"sessionsWithEstimate"`
	SessionsWithoutEstimate int64             `json:"sessionsWithoutEstimate"`
	PricingCoverage         string            `json:"pricingCoverage"`
}
type Change struct {
	Sequence  int64     `json:"sequence"`
	Kind      string    `json:"kind"`
	SessionID string    `json:"sessionId"`
	At        time.Time `json:"at"`
}
