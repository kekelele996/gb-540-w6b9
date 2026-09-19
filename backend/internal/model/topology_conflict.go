package model

import (
	"time"

	"cadastral-boundary-topology-resolution/backend/internal/constants"
)

// TopologyConflict is an immutable detection result for a proposal snapshot.
type TopologyConflict struct {
	ID                      uint                   `gorm:"primaryKey" json:"id"`
	ProposalID              uint                   `gorm:"not null;index" json:"proposal_id"`
	ParcelIDs               string                 `gorm:"type:text;not null" json:"parcel_ids"`
	ConflictType            constants.ConflictType `gorm:"size:32;not null;index" json:"conflict_type"`
	GeometryGeoJSON         string                 `gorm:"column:geometry_geojson;type:text" json:"geometry_geojson"`
	MagnitudeSquareM        float64                `gorm:"not null" json:"magnitude_square_m"`
	Severity                string                 `gorm:"size:16;not null" json:"severity"`
	AlgorithmVersion        string                 `gorm:"size:40;not null" json:"algorithm_version"`
	InputHash               string                 `gorm:"size:128;not null;index" json:"input_hash"`
	ConflictState           string                 `gorm:"size:32;not null;index" json:"conflict_state"`
	SuggestedResolutionJSON string                 `gorm:"type:text" json:"suggested_resolution_json"`
	Explanation             string                 `gorm:"size:2000" json:"explanation"`
	DetectedAt              time.Time              `gorm:"not null" json:"detected_at"`
	ResolvedBy              *uint                  `json:"resolved_by"`
}

// TopologyDetectionRun binds an actor and Idempotency-Key to immutable
// detection results so an interrupted client can replay the same operation.
type TopologyDetectionRun struct {
	ID             uint      `gorm:"primaryKey" json:"id"`
	ProposalID     uint      `gorm:"not null;index" json:"proposal_id"`
	ActorID        uint      `gorm:"not null;uniqueIndex:idx_detection_actor_key" json:"actor_id"`
	IdempotencyKey string    `gorm:"size:128;not null;uniqueIndex:idx_detection_actor_key" json:"idempotency_key"`
	RequestHash    string    `gorm:"size:128;not null" json:"request_hash"`
	InputHash      string    `gorm:"size:128;not null;index" json:"input_hash"`
	ResultIDs      string    `gorm:"type:text;not null" json:"result_ids"`
	CreatedAt      time.Time `json:"created_at"`
}

// ConflictReviewBatch binds the conflicts produced by one detection run to a
// single reviewer decision set. All conflicts must carry a conclusion before
// the batch can be applied; applying creates exactly one new draft proposal.
type ConflictReviewBatch struct {
	ID               uint       `gorm:"primaryKey" json:"id"`
	DetectionRunID   uint       `gorm:"not null;uniqueIndex" json:"detection_run_id"`
	ProposalID       uint       `gorm:"not null;index" json:"proposal_id"`
	ReviewerID       *uint      `gorm:"index" json:"reviewer_id"`
	BatchState       string     `gorm:"size:24;not null;index" json:"batch_state"`
	ConflictIDs      string     `gorm:"type:text;not null" json:"conflict_ids"`
	ResultProposalID *uint      `gorm:"column:result_proposal_id;index" json:"result_proposal_id"`
	AppliedAt        *time.Time `json:"applied_at"`
	CreatedAt        time.Time  `json:"created_at"`
	UpdatedAt        time.Time  `json:"updated_at"`
}

// ConflictDisposition is one reviewer conclusion inside a batch. Each conflict
// has at most one disposition; re-posting a disposition replaces the review
// note while the batch remains open.
type ConflictDisposition struct {
	ID         uint      `gorm:"primaryKey" json:"id"`
	BatchID    uint      `gorm:"not null;uniqueIndex:idx_disposition_batch_conflict" json:"batch_id"`
	ConflictID uint      `gorm:"not null;uniqueIndex:idx_disposition_batch_conflict;index" json:"conflict_id"`
	Decision   string    `gorm:"size:32;not null;index" json:"decision"`
	ReviewNote string    `gorm:"size:2000" json:"review_note"`
	ReviewerID uint      `gorm:"not null" json:"reviewer_id"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// ConflictBatchApplication makes batch application idempotent: replaying the
// same actor and Idempotency-Key returns the stored outcome instead of
// creating a second draft, even under concurrent requests.
type ConflictBatchApplication struct {
	ID               uint      `gorm:"primaryKey" json:"id"`
	BatchID          uint      `gorm:"not null;index" json:"batch_id"`
	ActorID          uint      `gorm:"not null;uniqueIndex:idx_batch_apply_actor_key" json:"actor_id"`
	IdempotencyKey   string    `gorm:"size:128;not null;uniqueIndex:idx_batch_apply_actor_key" json:"idempotency_key"`
	ResultProposalID uint      `gorm:"not null" json:"result_proposal_id"`
	CreatedAt        time.Time `json:"created_at"`
}
