package dto

import "cadastral-boundary-topology-resolution/backend/internal/model"

type ConflictQuery struct {
	ProposalID *uint
	State      string
	Type       string
	Page       int
	PageSize   int
}

type DetectConflictRequest struct {
	ProposalID     uint    `json:"proposal_id" validate:"required,gt=0"`
	SnapToleranceM float64 `json:"snap_tolerance_m" validate:"omitempty,gt=0,lte=1000"`
}

type ConflictTransitionRequest struct {
	To string `json:"to" validate:"required"`
}

type ApplySuggestionRequest struct {
	Rationale string `json:"rationale" validate:"max=2000"`
}

// ConflictDispositionInput is one reviewer conclusion posted as part of a
// unified batch disposition request.
type ConflictDispositionInput struct {
	ConflictID uint   `json:"conflict_id" validate:"required,gt=0"`
	Decision   string `json:"decision" validate:"required,oneof=confirmed false_positive resolution_proposed"`
	ReviewNote string `json:"review_note" validate:"max=2000"`
}

// BatchDispositionRequest replaces or adds reviewer conclusions for a batch.
// Every conflict of the detection run must end up with a conclusion, and at
// least one conclusion must propose the snap suggestion before applying.
type BatchDispositionRequest struct {
	Dispositions []ConflictDispositionInput `json:"dispositions" validate:"required,min=1,dive"`
}

// BatchApplyRequest atomically creates one new draft proposal from the
// proposed snap suggestion and resolves every confirmed batch conflict. The
// Idempotency-Key header guards duplicate and concurrent replay.
type BatchApplyRequest struct {
	Rationale string `json:"rationale" validate:"max=2000"`
}

// BatchDispositionView is the read-back projection of one conclusion.
type BatchDispositionView struct {
	ConflictID uint   `json:"conflict_id"`
	Decision   string `json:"decision"`
	ReviewNote string `json:"review_note"`
	ReviewerID uint   `json:"reviewer_id"`
}

// ConflictSummaryView projects the live conflict state inside a batch so the
// client can tell a stored conclusion apart from a conflict resolved elsewhere.
type ConflictSummaryView struct {
	ID            uint   `json:"id"`
	ProposalID    uint   `json:"proposal_id"`
	ConflictType  string `json:"conflict_type"`
	ConflictState string `json:"conflict_state"`
	Severity      string `json:"severity"`
}

// ReviewBatchView projects a batch, its current conflict states and the
// reviewer conclusions so a refreshed page reads the same decision set back.
type ReviewBatchView struct {
	Batch             model.ConflictReviewBatch `json:"batch"`
	Dispositions      []BatchDispositionView    `json:"dispositions"`
	Conflicts         []ConflictSummaryView     `json:"conflicts"`
	ReadyToApply      bool                      `json:"ready_to_apply"`
	MissingConclusion int                       `json:"missing_conclusion"`
	HasSuggestion     bool                      `json:"has_suggestion"`
}

// BatchApplyResult is returned once for an apply request and for every
// idempotent replay of the same actor/key pair.
type BatchApplyResult struct {
	Batch     model.ConflictReviewBatch `json:"batch"`
	Proposal  model.BoundaryProposal    `json:"proposal"`
	Conflicts []model.TopologyConflict  `json:"conflicts"`
	Replayed  bool                      `json:"replayed"`
}
