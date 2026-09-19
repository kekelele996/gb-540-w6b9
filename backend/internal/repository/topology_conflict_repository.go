package repository

import (
	"errors"
	"fmt"

	"cadastral-boundary-topology-resolution/backend/internal/dto"
	"cadastral-boundary-topology-resolution/backend/internal/model"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// TopologyConflictRepository stores immutable findings and their lifecycle.
type TopologyConflictRepository struct{ db *gorm.DB }

func (r *TopologyConflictRepository) Create(item *model.TopologyConflict) error {
	if err := r.db.Create(item).Error; err != nil {
		return fmt.Errorf("create topology conflict: %w", err)
	}
	return nil
}

func (r *TopologyConflictRepository) Get(id uint) (model.TopologyConflict, error) {
	var item model.TopologyConflict
	if err := r.db.First(&item, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return item, ErrNotFound
		}
		return item, fmt.Errorf("get conflict: %w", err)
	}
	return item, nil
}

func (r *TopologyConflictRepository) ListByIDs(ids []uint) ([]model.TopologyConflict, error) {
	if len(ids) == 0 {
		return []model.TopologyConflict{}, nil
	}
	var items []model.TopologyConflict
	if err := retryOnSQLiteBusy(r.db, func() error {
		return r.db.Where("id IN ?", ids).Find(&items).Error
	}); err != nil {
		return nil, fmt.Errorf("find topology conflicts by ids: %w", err)
	}
	byID := make(map[uint]model.TopologyConflict, len(items))
	for _, item := range items {
		byID[item.ID] = item
	}
	ordered := make([]model.TopologyConflict, 0, len(ids))
	for _, id := range ids {
		item, ok := byID[id]
		if !ok {
			return nil, fmt.Errorf("topology conflict %d missing from detection run: %w", id, ErrNotFound)
		}
		ordered = append(ordered, item)
	}
	return ordered, nil
}

func (r *TopologyConflictRepository) List(q dto.ConflictQuery) ([]model.TopologyConflict, int64, error) {
	db := r.db.Model(&model.TopologyConflict{})
	if q.ProposalID != nil {
		db = db.Where("proposal_id = ?", *q.ProposalID)
	}
	if q.State != "" {
		db = db.Where("conflict_state = ?", q.State)
	}
	if q.Type != "" {
		db = db.Where("conflict_type = ?", q.Type)
	}
	var total int64
	if err := db.Count(&total).Error; err != nil {
		return nil, 0, fmt.Errorf("count conflicts: %w", err)
	}
	var items []model.TopologyConflict
	if err := db.Order("detected_at DESC").Offset((q.Page - 1) * q.PageSize).Limit(q.PageSize).Find(&items).Error; err != nil {
		return nil, 0, fmt.Errorf("list conflicts: %w", err)
	}
	return items, total, nil
}

func (r *TopologyConflictRepository) Transition(id uint, from, to string, resolvedBy *uint) error {
	updates := map[string]any{"conflict_state": to}
	if resolvedBy != nil {
		updates["resolved_by"] = resolvedBy
	}
	result := r.db.Model(&model.TopologyConflict{}).Where("id = ? AND conflict_state = ?", id, from).Updates(updates)
	if result.Error != nil {
		return fmt.Errorf("transition conflict: %w", result.Error)
	}
	if result.RowsAffected == 0 {
		return fmt.Errorf("conflict state changed: %w", gorm.ErrInvalidTransaction)
	}
	return nil
}

// TopologyDetectionRunRepository owns idempotency records for conflict detection.
type TopologyDetectionRunRepository struct{ db *gorm.DB }

func (r *TopologyDetectionRunRepository) GetByActorKey(actorID uint, key string) (model.TopologyDetectionRun, error) {
	var item model.TopologyDetectionRun
	if err := r.db.Where("actor_id = ? AND idempotency_key = ?", actorID, key).First(&item).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return item, ErrNotFound
		}
		return item, fmt.Errorf("find detection idempotency key: %w", err)
	}
	return item, nil
}

func (r *TopologyDetectionRunRepository) Create(item *model.TopologyDetectionRun) error {
	if err := r.db.Create(item).Error; err != nil {
		return fmt.Errorf("create topology detection run: %w", err)
	}
	return nil
}

func (r *TopologyDetectionRunRepository) Get(id uint) (model.TopologyDetectionRun, error) {
	var item model.TopologyDetectionRun
	if err := r.db.First(&item, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return item, ErrNotFound
		}
		return item, fmt.Errorf("get detection run: %w", err)
	}
	return item, nil
}

// GetLatestByProposal returns the most recent detection run for a proposal.
func (r *TopologyDetectionRunRepository) GetLatestByProposal(proposalID uint) (model.TopologyDetectionRun, error) {
	var item model.TopologyDetectionRun
	if err := r.db.Where("proposal_id = ?", proposalID).Order("id DESC").First(&item).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return item, ErrNotFound
		}
		return item, fmt.Errorf("get latest detection run: %w", err)
	}
	return item, nil
}

// ConflictReviewBatchRepository persists unified review batches, their
// per-conflict conclusions and idempotent application records.
type ConflictReviewBatchRepository struct{ db *gorm.DB }

func (r *ConflictReviewBatchRepository) Create(item *model.ConflictReviewBatch) error {
	if err := r.db.Create(item).Error; err != nil {
		return fmt.Errorf("create conflict review batch: %w", err)
	}
	return nil
}

func (r *ConflictReviewBatchRepository) Get(id uint) (model.ConflictReviewBatch, error) {
	var item model.ConflictReviewBatch
	if err := retryOnSQLiteBusy(r.db, func() error {
		return r.db.First(&item, id).Error
	}); err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return item, ErrNotFound
		}
		return item, fmt.Errorf("get conflict review batch: %w", err)
	}
	return item, nil
}

func (r *ConflictReviewBatchRepository) GetByDetectionRun(runID uint) (model.ConflictReviewBatch, error) {
	var item model.ConflictReviewBatch
	if err := r.db.Where("detection_run_id = ?", runID).First(&item).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return item, ErrNotFound
		}
		return item, fmt.Errorf("get batch by detection run: %w", err)
	}
	return item, nil
}

// Transition performs a conditional state update so concurrent applications
// cannot both move an open batch to applied.
func (r *ConflictReviewBatchRepository) Transition(id uint, from, to string, updates map[string]any) error {
	updates["batch_state"] = to
	result := r.db.Model(&model.ConflictReviewBatch{}).Where("id = ? AND batch_state = ?", id, from).Updates(updates)
	if result.Error != nil {
		return fmt.Errorf("transition conflict review batch: %w", result.Error)
	}
	if result.RowsAffected == 0 {
		return fmt.Errorf("review batch state changed: %w", gorm.ErrInvalidTransaction)
	}
	return nil
}

func (r *ConflictReviewBatchRepository) ListOpenByProposal(proposalID uint) ([]model.ConflictReviewBatch, error) {
	var items []model.ConflictReviewBatch
	if err := r.db.Where("proposal_id = ? AND batch_state = ?", proposalID, "open").Order("id DESC").Find(&items).Error; err != nil {
		return nil, fmt.Errorf("list open review batches: %w", err)
	}
	return items, nil
}

// UpsertDisposition stores exactly one conclusion per (batch, conflict). The
// unique index and the database-level upsert enforce the invariant even under
// concurrent requests.
func (r *ConflictReviewBatchRepository) UpsertDisposition(item *model.ConflictDisposition) error {
	if err := r.db.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "batch_id"}, {Name: "conflict_id"}},
		DoUpdates: clause.AssignmentColumns([]string{"decision", "review_note", "reviewer_id", "updated_at"}),
	}).Create(item).Error; err != nil {
		return fmt.Errorf("upsert conflict disposition: %w", err)
	}
	return nil
}

func (r *ConflictReviewBatchRepository) ListDispositions(batchID uint) ([]model.ConflictDisposition, error) {
	var items []model.ConflictDisposition
	if err := retryOnSQLiteBusy(r.db, func() error {
		return r.db.Where("batch_id = ?", batchID).Order("conflict_id ASC").Find(&items).Error
	}); err != nil {
		return nil, fmt.Errorf("list conflict dispositions: %w", err)
	}
	return items, nil
}

func (r *ConflictReviewBatchRepository) CreateApplication(item *model.ConflictBatchApplication) error {
	if err := r.db.Create(item).Error; err != nil {
		return fmt.Errorf("create batch application: %w", err)
	}
	return nil
}

func (r *ConflictReviewBatchRepository) GetApplication(actorID uint, key string) (model.ConflictBatchApplication, error) {
	var item model.ConflictBatchApplication
	if err := retryOnSQLiteBusy(r.db, func() error {
		return r.db.Where("actor_id = ? AND idempotency_key = ?", actorID, key).First(&item).Error
	}); err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return item, ErrNotFound
		}
		return item, fmt.Errorf("find batch application: %w", err)
	}
	return item, nil
}
