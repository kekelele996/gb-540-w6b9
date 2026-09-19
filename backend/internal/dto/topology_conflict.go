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

// ApplyConflictBatchRequest disposes every conflict of one detection run at once.
type ApplyConflictBatchRequest struct {
	DetectionRunID uint   `json:"detection_run_id" validate:"required,gt=0"`
	Rationale      string `json:"rationale" validate:"max=2000"`
}

// ConflictBatchApplyResult returns the single derived draft and the batch
// states re-read after the atomic disposition.
type ConflictBatchApplyResult struct {
	Proposal  model.BoundaryProposal   `json:"proposal"`
	Conflicts []model.TopologyConflict `json:"conflicts"`
}
