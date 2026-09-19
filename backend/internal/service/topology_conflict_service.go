package service

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"cadastral-boundary-topology-resolution/backend/internal/constants"
	"cadastral-boundary-topology-resolution/backend/internal/dto"
	"cadastral-boundary-topology-resolution/backend/internal/geometry"
	"cadastral-boundary-topology-resolution/backend/internal/model"
	"cadastral-boundary-topology-resolution/backend/internal/repository"
)

func (s *CadastralService) DetectConflicts(req dto.DetectConflictRequest, idempotencyKey string, actor Actor) ([]model.TopologyConflict, error) {
	key := strings.TrimSpace(idempotencyKey)
	if key == "" || len(key) > 128 {
		return nil, invalid("Idempotency-Key must contain between 1 and 128 characters", nil)
	}
	proposal, err := s.store.Proposals.Get(req.ProposalID)
	if errors.Is(err, repository.ErrNotFound) {
		return nil, notFound("proposal")
	}
	if err != nil {
		return nil, internal("load proposal failed", err)
	}
	tolerance := req.SnapToleranceM
	if tolerance == 0 {
		tolerance = proposal.SnapToleranceM
	}
	requestHash := geometry.Hash(strconv.FormatUint(uint64(req.ProposalID), 10), fmt.Sprintf("%.6f", tolerance))
	if existing, findErr := s.store.DetectionRuns.GetByActorKey(actor.ID, key); findErr == nil {
		return s.replayDetectionRun(existing, requestHash)
	} else if !errors.Is(findErr, repository.ErrNotFound) {
		return nil, internal("check conflict idempotency failed", findErr)
	}

	parcel, err := s.store.Parcels.Get(proposal.ParcelID)
	if errors.Is(err, repository.ErrNotFound) {
		return nil, notFound("parcel")
	}
	if err != nil {
		return nil, internal("load parcel failed", err)
	}
	base, baseErr := geometry.ParsePolygon(parcel.BoundaryGeoJSON)
	if baseErr != nil {
		return nil, geoInvalid(baseErr)
	}
	proposed, proposedErr := geometry.ParsePolygon(proposal.ProposedGeoJSON)
	if proposedErr != nil {
		return nil, geoInvalid(proposedErr)
	}
	neighbourModels, neighbourErr := s.store.Parcels.ListActiveByCoordinateSystem(parcel.CoordinateSystem, parcel.ID)
	if neighbourErr != nil {
		return nil, internal("load neighbouring parcels failed", neighbourErr)
	}
	neighbours := make([]geometry.ParcelReference, 0, len(neighbourModels))
	references := []geometry.Polygon{base}
	hashParts := []string{parcel.BoundaryGeoJSON, proposal.ProposedGeoJSON, parcel.CoordinateSystem, fmt.Sprintf("%.6f", tolerance), geometry.AlgorithmVersion}
	for _, neighbour := range neighbourModels {
		polygon, parseErr := geometry.ParsePolygon(neighbour.BoundaryGeoJSON)
		if parseErr != nil {
			return nil, internal(fmt.Sprintf("stored geometry for neighbouring parcel %d is invalid", neighbour.ID), parseErr)
		}
		neighbours = append(neighbours, geometry.ParcelReference{ID: neighbour.ID, Polygon: polygon})
		references = append(references, polygon)
		hashParts = append(hashParts, strconv.FormatUint(uint64(neighbour.ID), 10), neighbour.BoundaryGeoJSON)
	}
	sort.Slice(neighbours, func(i, j int) bool { return neighbours[i].ID < neighbours[j].ID })
	snapped, snapErr := geometry.SnapToReferences(proposed, references, tolerance)
	if snapErr != nil {
		return nil, geoInvalid(snapErr)
	}
	findings, detectErr := geometry.DetectTopology(base, snapped.Polygon, neighbours, tolerance)
	if detectErr != nil {
		return nil, internal("detect topology conflicts failed", detectErr)
	}
	inputHash := geometry.Hash(hashParts...)
	suggestedJSON, marshalErr := json.Marshal(map[string]any{
		"action": "review_snapped_boundary", "snapped_geojson": json.RawMessage(snapped.SuggestedGeoJSON), "snap_changes": snapped.Changes,
		"tolerance_m": tolerance, "coordinate_system": parcel.CoordinateSystem, "algorithm_version": geometry.AlgorithmVersion, "topology_input_hash": inputHash,
	})
	if marshalErr != nil {
		return nil, internal("encode topology suggestion failed", marshalErr)
	}
	items := make([]model.TopologyConflict, 0, len(findings))
	now := time.Now().UTC()
	for _, finding := range findings {
		participantIDs := uniqueSortedIDs(append([]uint{parcel.ID}, finding.ParcelIDs...))
		parcelIDs, encodeErr := json.Marshal(participantIDs)
		if encodeErr != nil {
			return nil, internal("encode topology participants failed", encodeErr)
		}
		items = append(items, model.TopologyConflict{
			ProposalID: proposal.ID, ParcelIDs: string(parcelIDs), ConflictType: constants.ConflictType(finding.ConflictType), GeometryGeoJSON: finding.Geometry,
			MagnitudeSquareM: finding.Magnitude, Severity: conflictSeverity(finding.ConflictType, finding.Magnitude, tolerance), AlgorithmVersion: geometry.AlgorithmVersion,
			InputHash: inputHash, ConflictState: constants.ConflictDetected, SuggestedResolutionJSON: string(suggestedJSON), Explanation: finding.Explanation, DetectedAt: now,
		})
	}
	err = s.store.Transaction(func(tx *repository.Store) error {
		for index := range items {
			if createErr := tx.Conflicts.Create(&items[index]); createErr != nil {
				return createErr
			}
			if auditErr := tx.Audits.Create(audit(actor, "conflict.detected", "TopologyConflict", items[index].ID, &parcel.ID, "{}", snapshot(items[index]))); auditErr != nil {
				return auditErr
			}
		}
		resultIDs := make([]uint, 0, len(items))
		for _, item := range items {
			resultIDs = append(resultIDs, item.ID)
		}
		encodedIDs, encodeErr := json.Marshal(resultIDs)
		if encodeErr != nil {
			return encodeErr
		}
		run := model.TopologyDetectionRun{ProposalID: proposal.ID, ActorID: actor.ID, IdempotencyKey: key, RequestHash: requestHash, InputHash: inputHash, ResultIDs: string(encodedIDs)}
		if createErr := tx.DetectionRuns.Create(&run); createErr != nil {
			return createErr
		}
		return tx.Audits.Create(audit(actor, "conflict.detection_completed", "TopologyDetectionRun", run.ID, &proposal.ID, "{}", snapshot(run)))
	})
	if err != nil {
		if existing, findErr := s.store.DetectionRuns.GetByActorKey(actor.ID, key); findErr == nil {
			return s.replayDetectionRun(existing, requestHash)
		}
		return nil, wrapCadastral(err, "detect conflicts failed")
	}
	return items, nil
}

func (s *CadastralService) ListConflicts(q dto.ConflictQuery) ([]model.TopologyConflict, dto.Pagination, error) {
	normalizePage(&q.Page, &q.PageSize)
	items, total, err := s.store.Conflicts.List(q)
	if err != nil {
		return nil, dto.Pagination{}, internal("list conflicts failed", err)
	}
	return items, dto.Pagination{Page: q.Page, PageSize: q.PageSize, Total: total}, nil
}

func (s *CadastralService) GetConflict(id uint) (model.TopologyConflict, error) {
	item, err := s.store.Conflicts.Get(id)
	if errors.Is(err, repository.ErrNotFound) {
		return item, notFound("conflict")
	}
	if err != nil {
		return item, internal("get conflict failed", err)
	}
	return item, nil
}

func (s *CadastralService) TransitionConflict(id uint, req dto.ConflictTransitionRequest, actor Actor) (model.TopologyConflict, error) {
	item, err := s.store.Conflicts.Get(id)
	if errors.Is(err, repository.ErrNotFound) {
		return item, notFound("conflict")
	}
	if err != nil {
		return item, internal("get conflict failed", err)
	}
	if !constants.CanConflictTransition(item.ConflictState, req.To) {
		return item, conflict("conflict state transition is not allowed", nil)
	}
	var resolved *uint
	if req.To == constants.ConflictResolved || req.To == constants.ConflictClosed {
		resolved = &actor.ID
	}
	err = s.store.Transaction(func(tx *repository.Store) error {
		if transitionErr := tx.Conflicts.Transition(id, item.ConflictState, req.To, resolved); transitionErr != nil {
			return transitionErr
		}
		return tx.Audits.Create(audit(actor, "conflict.state_changed", "TopologyConflict", id, nil, snapshot(item), snapshot(map[string]any{"state": req.To})))
	})
	if err != nil {
		return item, conflict("conflict changed while transitioning", err)
	}
	item.ConflictState = req.To
	item.ResolvedBy = resolved
	return item, nil
}

func (s *CadastralService) ApplyConflictSuggestion(id uint, req dto.ApplySuggestionRequest, actor Actor) (model.BoundaryProposal, error) {
	if err := requireAnyRole(actor, constants.RoleReviewer, constants.RoleAdmin); err != nil {
		return model.BoundaryProposal{}, err
	}
	item, err := s.store.Conflicts.Get(id)
	if errors.Is(err, repository.ErrNotFound) {
		return model.BoundaryProposal{}, notFound("conflict")
	}
	if err != nil {
		return model.BoundaryProposal{}, internal("get conflict failed", err)
	}
	if item.ConflictState != constants.ConflictResolutionProposed {
		return model.BoundaryProposal{}, conflict("a suggestion can only be applied from resolution_proposed", nil)
	}
	proposal, err := s.store.Proposals.Get(item.ProposalID)
	if errors.Is(err, repository.ErrNotFound) {
		return model.BoundaryProposal{}, notFound("proposal")
	}
	if err != nil {
		return model.BoundaryProposal{}, internal("load proposal failed", err)
	}
	var suggestion struct {
		SnappedGeoJSON json.RawMessage `json:"snapped_geojson"`
	}
	if err := json.Unmarshal([]byte(item.SuggestedResolutionJSON), &suggestion); err != nil || len(suggestion.SnappedGeoJSON) == 0 {
		return model.BoundaryProposal{}, conflict("the conflict has no usable snapped-boundary suggestion", err)
	}
	polygon, parseErr := geometry.ParsePolygon(string(suggestion.SnappedGeoJSON))
	if parseErr != nil {
		return model.BoundaryProposal{}, internal("stored suggested geometry is invalid", parseErr)
	}
	parcel, parcelErr := s.store.Parcels.Get(proposal.ParcelID)
	if errors.Is(parcelErr, repository.ErrNotFound) {
		return model.BoundaryProposal{}, notFound("parcel")
	}
	if parcelErr != nil {
		return model.BoundaryProposal{}, internal("load parcel failed", parcelErr)
	}
	rationale := strings.TrimSpace(req.Rationale)
	if rationale == "" {
		rationale = fmt.Sprintf("Applied deterministic suggestion from topology conflict %d.", item.ID)
	}
	derived := model.BoundaryProposal{
		ParcelID: proposal.ParcelID, BaseVersion: parcel.BoundaryVersion, ProposedGeoJSON: string(suggestion.SnappedGeoJSON), ObservationIDs: proposal.ObservationIDs,
		SnapToleranceM: proposal.SnapToleranceM, AreaDeltaSquareM: polygon.Area - parcel.AreaSquareM, ProposalState: constants.ProposalDraft,
		Rationale: rationale, Version: proposal.Version + 1, CreatedBy: actor.ID,
	}
	resolvedBy := actor.ID
	err = s.store.Transaction(func(tx *repository.Store) error {
		if createErr := tx.Proposals.Create(&derived); createErr != nil {
			return createErr
		}
		if transitionErr := tx.Conflicts.Transition(item.ID, item.ConflictState, constants.ConflictResolved, &resolvedBy); transitionErr != nil {
			return transitionErr
		}
		if auditErr := tx.Audits.Create(audit(actor, "proposal.created_from_conflict", "BoundaryProposal", derived.ID, &derived.ParcelID, "{}", snapshot(derived))); auditErr != nil {
			return auditErr
		}
		return tx.Audits.Create(audit(actor, "conflict.suggestion_applied", "TopologyConflict", item.ID, &derived.ID, snapshot(item), snapshot(map[string]any{"conflict_state": constants.ConflictResolved, "proposal_id": derived.ID})))
	})
	if err != nil {
		return model.BoundaryProposal{}, wrapCadastral(err, "apply conflict suggestion failed")
	}
	return derived, nil
}

func (s *CadastralService) replayDetectionRun(run model.TopologyDetectionRun, requestHash string) ([]model.TopologyConflict, error) {
	if run.RequestHash != requestHash {
		return nil, conflict("Idempotency-Key has already been used with a different request", nil)
	}
	var resultIDs []uint
	if err := json.Unmarshal([]byte(run.ResultIDs), &resultIDs); err != nil {
		return nil, internal("stored idempotency result is invalid", err)
	}
	items, err := s.store.Conflicts.ListByIDs(resultIDs)
	if err != nil {
		return nil, internal("load idempotent detection result failed", err)
	}
	return items, nil
}

func canTransitionObservation(from, to string) bool {
	return (from == "accepted" && (to == "rejected" || to == "superseded")) || (from == "rejected" && to == "accepted")
}

func requireAnyRole(actor Actor, roles ...string) error {
	for _, role := range roles {
		if actor.Role == role {
			return nil
		}
	}
	return &AppError{CodeForbidden, http.StatusForbidden, "role is not permitted for this operation", nil}
}

func conflictSeverity(kind string, magnitude, tolerance float64) string {
	if kind == string(constants.ConflictOverlap) && magnitude > 100 {
		return "high"
	}
	if kind == string(constants.ConflictGap) && magnitude > tolerance*10 {
		return "high"
	}
	return "medium"
}

func uniqueSortedIDs(ids []uint) []uint {
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	output := ids[:0]
	for _, id := range ids {
		if len(output) == 0 || output[len(output)-1] != id {
			output = append(output, id)
		}
	}
	return output
}

func geoInvalid(err error) error {
	return &AppError{CodeInvalidInput, http.StatusUnprocessableEntity, "geometry or coordinate system is invalid: " + err.Error(), err}
}

func wrapCadastral(err error, message string) error {
	var appErr *AppError
	if errors.As(err, &appErr) {
		return err
	}
	return internal(message, err)
}

// ----------------------------------------------------------------------------
// ensureBatchForRun returns the review batch that owns every conflict of a
// detection run, creating it lazily the first time a reviewer opens the run.
func (s *CadastralService) ensureBatchForRun(runID uint, actor Actor) (model.ConflictReviewBatch, error) {
	if batch, err := s.store.ReviewBatches.GetByDetectionRun(runID); err == nil {
		return batch, nil
	} else if !errors.Is(err, repository.ErrNotFound) {
		return model.ConflictReviewBatch{}, internal("load review batch failed", err)
	}
	run, err := s.store.DetectionRuns.Get(runID)
	if errors.Is(err, repository.ErrNotFound) {
		return model.ConflictReviewBatch{}, notFound("detection run")
	}
	if err != nil {
		return model.ConflictReviewBatch{}, internal("load detection run failed", err)
	}
	batch := model.ConflictReviewBatch{
		DetectionRunID: run.ID, ProposalID: run.ProposalID, BatchState: constants.BatchOpen,
		ConflictIDs: run.ResultIDs,
	}
	err = s.store.Transaction(func(tx *repository.Store) error {
		if createErr := tx.ReviewBatches.Create(&batch); createErr != nil {
			return createErr
		}
		return tx.Audits.Create(audit(actor, "conflict.batch_opened", "ConflictReviewBatch", batch.ID, &batch.ProposalID, "{}", snapshot(batch)))
	})
	if err != nil {
		// A concurrent request may have created the batch first; reuse it.
		if existing, findErr := s.store.ReviewBatches.GetByDetectionRun(runID); findErr == nil {
			return existing, nil
		}
		return model.ConflictReviewBatch{}, wrapCadastral(err, "open review batch failed")
	}
	return batch, nil
}

func (s *CadastralService) GetReviewBatchByProposal(proposalID uint, actor Actor) (dto.ReviewBatchView, error) {
	run, err := s.store.DetectionRuns.GetLatestByProposal(proposalID)
	if errors.Is(err, repository.ErrNotFound) {
		return dto.ReviewBatchView{}, notFound("detection run for proposal")
	}
	if err != nil {
		return dto.ReviewBatchView{}, internal("load detection run failed", err)
	}
	batch, err := s.ensureBatchForRun(run.ID, actor)
	if err != nil {
		return dto.ReviewBatchView{}, err
	}
	return s.buildBatchView(batch)
}

func (s *CadastralService) GetReviewBatch(id uint) (dto.ReviewBatchView, error) {
	batch, err := s.store.ReviewBatches.Get(id)
	if errors.Is(err, repository.ErrNotFound) {
		return dto.ReviewBatchView{}, notFound("review batch")
	}
	if err != nil {
		return dto.ReviewBatchView{}, internal("load review batch failed", err)
	}
	return s.buildBatchView(batch)
}

func (s *CadastralService) buildBatchView(batch model.ConflictReviewBatch) (dto.ReviewBatchView, error) {
	conflictIDs, err := decodeIDList(batch.ConflictIDs)
	if err != nil {
		return dto.ReviewBatchView{}, internal("stored batch conflict list is invalid", err)
	}
	conflicts, err := s.store.Conflicts.ListByIDs(conflictIDs)
	if err != nil {
		return dto.ReviewBatchView{}, internal("load batch conflicts failed", err)
	}
	dispositions, err := s.store.ReviewBatches.ListDispositions(batch.ID)
	if err != nil {
		return dto.ReviewBatchView{}, internal("load dispositions failed", err)
	}
	views := make([]dto.ConflictSummaryView, 0, len(conflicts))
	byID := make(map[uint]model.TopologyConflict, len(conflicts))
	for _, item := range conflicts {
		byID[item.ID] = item
		views = append(views, dto.ConflictSummaryView{ID: item.ID, ProposalID: item.ProposalID, ConflictType: string(item.ConflictType), ConflictState: item.ConflictState, Severity: item.Severity})
	}
	dispositionViews := make([]dto.BatchDispositionView, 0, len(dispositions))
	concluded := make(map[uint]bool, len(dispositions))
	hasSuggestion := false
	for _, item := range dispositions {
		dispositionViews = append(dispositionViews, dto.BatchDispositionView{ConflictID: item.ConflictID, Decision: item.Decision, ReviewNote: item.ReviewNote, ReviewerID: item.ReviewerID})
		if _, known := byID[item.ConflictID]; known {
			concluded[item.ConflictID] = true
		}
		if item.Decision == constants.DispositionResolutionProposed {
			hasSuggestion = true
		}
	}
	missing := 0
	for _, item := range conflicts {
		if !concluded[item.ID] {
			missing++
		}
	}
	return dto.ReviewBatchView{
		Batch: batch, Dispositions: dispositionViews, Conflicts: views,
		ReadyToApply:      batch.BatchState == constants.BatchOpen && missing == 0 && hasSuggestion,
		MissingConclusion: missing, HasSuggestion: hasSuggestion,
	}, nil
}

// SaveBatchDispositions records reviewer conclusions for one batch. The whole
// request is atomic: any invalid or stale conflict fails every conclusion,
// leaving conflict states and audit history untouched.
func (s *CadastralService) SaveBatchDispositions(batchID uint, req dto.BatchDispositionRequest, actor Actor) (dto.ReviewBatchView, error) {
	if err := requireAnyRole(actor, constants.RoleReviewer, constants.RoleAdmin); err != nil {
		return dto.ReviewBatchView{}, err
	}
	batch, err := s.store.ReviewBatches.Get(batchID)
	if errors.Is(err, repository.ErrNotFound) {
		return dto.ReviewBatchView{}, notFound("review batch")
	}
	if err != nil {
		return dto.ReviewBatchView{}, internal("load review batch failed", err)
	}
	if batch.BatchState != constants.BatchOpen {
		return dto.ReviewBatchView{}, conflict("the review batch has already been finalized", nil)
	}
	batchConflictIDs, err := decodeIDList(batch.ConflictIDs)
	if err != nil {
		return dto.ReviewBatchView{}, internal("stored batch conflict list is invalid", err)
	}
	memberSet := make(map[uint]bool, len(batchConflictIDs))
	for _, id := range batchConflictIDs {
		memberSet[id] = true
	}
	inputSet := make(map[uint]bool, len(req.Dispositions))
	for _, input := range req.Dispositions {
		if !memberSet[input.ConflictID] {
			return dto.ReviewBatchView{}, invalid(fmt.Sprintf("conflict %d is not part of review batch %d", input.ConflictID, batch.ID), nil)
		}
		if inputSet[input.ConflictID] {
			return dto.ReviewBatchView{}, invalid(fmt.Sprintf("conflict %d has a duplicated disposition", input.ConflictID), nil)
		}
		if !constants.IsBatchConclusion(input.Decision) {
			return dto.ReviewBatchView{}, invalid("unsupported review decision: "+input.Decision, nil)
		}
		inputSet[input.ConflictID] = true
	}
	items, err := s.store.Conflicts.ListByIDs(batchConflictIDs)
	if err != nil {
		return dto.ReviewBatchView{}, internal("load batch conflicts failed", err)
	}
	currentByID := make(map[uint]model.TopologyConflict, len(items))
	for _, item := range items {
		currentByID[item.ID] = item
	}
	// Validate and preview every state move before opening the transaction.
	type plannedDisposition struct {
		model model.ConflictDisposition
		from  string
		path  []string
	}
	planned := make([]plannedDisposition, 0, len(req.Dispositions))
	for _, input := range req.Dispositions {
		item := currentByID[input.ConflictID]
		path, pathErr := dispositionStatePath(item.ConflictState, input.Decision)
		if pathErr != nil {
			return dto.ReviewBatchView{}, conflict(fmt.Sprintf("conflict %d cannot take conclusion %s from state %s", item.ID, input.Decision, item.ConflictState), pathErr)
		}
		planned = append(planned, plannedDisposition{
			model: model.ConflictDisposition{BatchID: batch.ID, ConflictID: item.ID, Decision: input.Decision, ReviewNote: strings.TrimSpace(input.ReviewNote), ReviewerID: actor.ID},
			from:  item.ConflictState, path: path,
		})
	}
	err = s.store.Transaction(func(tx *repository.Store) error {
		reviewerID := actor.ID
		if batch.ReviewerID == nil {
			if err := tx.DB.Model(&model.ConflictReviewBatch{}).Where("id = ? AND reviewer_id IS NULL", batch.ID).Update("reviewer_id", reviewerID).Error; err != nil {
				return fmt.Errorf("assign batch reviewer: %w", err)
			}
		}
		for _, plan := range planned {
			entry := plan.model
			if upsertErr := tx.ReviewBatches.UpsertDisposition(&entry); upsertErr != nil {
				return upsertErr
			}
			currentState := plan.from
			for _, nextState := range plan.path {
				if transitionErr := tx.Conflicts.Transition(plan.model.ConflictID, currentState, nextState, nil); transitionErr != nil {
					return transitionErr
				}
				currentState = nextState
			}
			if auditErr := tx.Audits.Create(audit(actor, "conflict.disposition_recorded", "ConflictDisposition", entry.ID, &batch.ID, snapshot(map[string]any{"conflict_state": plan.from}), snapshot(map[string]any{"decision": plan.model.Decision, "conflict_state": dispositionTargetState(plan.model.Decision)}))); auditErr != nil {
				return auditErr
			}
		}
		return nil
	})
	if err != nil {
		return dto.ReviewBatchView{}, wrapCadastral(err, "save batch dispositions failed")
	}
	finalBatch, err := s.store.ReviewBatches.Get(batch.ID)
	if err != nil {
		return dto.ReviewBatchView{}, internal("reload review batch failed", err)
	}
	return s.buildBatchView(finalBatch)
}

// ApplyReviewBatch is the only way to turn reviewed conclusions into action.
// It creates one new draft proposal from a snap suggestion and resolves the
// remaining confirmed conflicts in a single transaction.
func (s *CadastralService) ApplyReviewBatch(batchID uint, req dto.BatchApplyRequest, idempotencyKey string, actor Actor) (dto.BatchApplyResult, error) {
	if err := requireAnyRole(actor, constants.RoleReviewer, constants.RoleAdmin); err != nil {
		return dto.BatchApplyResult{}, err
	}
	key := strings.TrimSpace(idempotencyKey)
	if key == "" || len(key) > 128 {
		return dto.BatchApplyResult{}, invalid("Idempotency-Key must contain between 1 and 128 characters", nil)
	}
	if replay, err := s.replayBatchApplication(actor.ID, key); err == nil {
		return replay, nil
	} else if !errors.Is(err, repository.ErrNotFound) {
		return dto.BatchApplyResult{}, err
	}
	batch, err := s.store.ReviewBatches.Get(batchID)
	if errors.Is(err, repository.ErrNotFound) {
		return dto.BatchApplyResult{}, notFound("review batch")
	}
	if err != nil {
		return dto.BatchApplyResult{}, internal("load review batch failed", err)
	}
	if batch.BatchState == constants.BatchApplied && batch.ResultProposalID != nil {
		return dto.BatchApplyResult{}, conflict("the review batch was already applied without a matching Idempotency-Key", nil)
	}
	if batch.BatchState != constants.BatchOpen {
		return dto.BatchApplyResult{}, conflict("only an open review batch can be applied", nil)
	}
	view, err := s.buildBatchView(batch)
	if err != nil {
		return dto.BatchApplyResult{}, err
	}
	if view.MissingConclusion != 0 {
		return dto.BatchApplyResult{}, conflict(fmt.Sprintf("every conflict needs a conclusion before applying; %d remain open", view.MissingConclusion), nil)
	}
	if !view.HasSuggestion {
		return dto.BatchApplyResult{}, conflict("at least one conflict must propose the snap suggestion before applying", nil)
	}
	conflictIDs, err := decodeIDList(batch.ConflictIDs)
	if err != nil {
		return dto.BatchApplyResult{}, internal("stored batch conflict list is invalid", err)
	}
	rationale := strings.TrimSpace(req.Rationale)
	if rationale == "" {
		rationale = fmt.Sprintf("Applied unified review batch %d.", batch.ID)
	}

	var derived model.BoundaryProposal
	var resolvedIDs []uint
	err = s.store.Transaction(func(tx *repository.Store) error {
		// Load conflicts, dispositions and the batch from a single transactional
		// snapshot so concurrent apply attempts cannot observe a half-resolved set.
		txConflicts, loadErr := tx.Conflicts.ListByIDs(conflictIDs)
		if loadErr != nil {
			return internal("load batch conflicts failed", loadErr)
		}
		txDispositions, loadErr := tx.ReviewBatches.ListDispositions(batch.ID)
		if loadErr != nil {
			return internal("load dispositions failed", loadErr)
		}
		decisionByConflict := make(map[uint]string, len(txDispositions))
		for _, entry := range txDispositions {
			decisionByConflict[entry.ConflictID] = entry.Decision
		}
		var snapConflict *model.TopologyConflict
		toResolve := make([]model.TopologyConflict, 0, len(txConflicts))
		for index := range txConflicts {
			item := &txConflicts[index]
			switch decisionByConflict[item.ID] {
			case constants.DispositionResolutionProposed:
				if item.ConflictState != constants.ConflictResolutionProposed {
					return conflict(fmt.Sprintf("conflict %d must be resolution_proposed to supply the snap suggestion", item.ID), nil)
				}
				if snapConflict == nil || item.ID < snapConflict.ID {
					snapConflict = item
				}
				toResolve = append(toResolve, *item)
			case constants.DispositionConfirmed:
				if item.ConflictState != constants.ConflictConfirmed {
					return conflict(fmt.Sprintf("conflict %d changed state before batch apply", item.ID), nil)
				}
				toResolve = append(toResolve, *item)
			case constants.DispositionFalsePositive:
				if item.ConflictState != constants.ConflictFalsePositive {
					return conflict(fmt.Sprintf("conflict %d lost its false-positive conclusion before apply", item.ID), nil)
				}
			default:
				return conflict(fmt.Sprintf("conflict %d has no review conclusion", item.ID), nil)
			}
		}
		if snapConflict == nil {
			return conflict("at least one resolution suggestion is required", nil)
		}
		proposal, parcel, suggestedGeoJSON, inputErr := s.snapProposalInputs(*snapConflict)
		if inputErr != nil {
			return inputErr
		}
		polygon, parseErr := geometry.ParsePolygon(suggestedGeoJSON)
		if parseErr != nil {
			return internal("stored suggested geometry is invalid", parseErr)
		}
		applyRationale := rationale
		if applyRationale == "" {
			applyRationale = fmt.Sprintf("Applied unified review batch %d from topology conflict %d.", batch.ID, snapConflict.ID)
		}
		derived = model.BoundaryProposal{
			ParcelID: proposal.ParcelID, BaseVersion: parcel.BoundaryVersion, ProposedGeoJSON: suggestedGeoJSON, ObservationIDs: proposal.ObservationIDs,
			SnapToleranceM: proposal.SnapToleranceM, AreaDeltaSquareM: polygon.Area - parcel.AreaSquareM, ProposalState: constants.ProposalDraft,
			Rationale: applyRationale, Version: proposal.Version + 1, CreatedBy: actor.ID,
		}
		if createErr := tx.Proposals.Create(&derived); createErr != nil {
			return createErr
		}
		resolvedBy := actor.ID
		for _, item := range toResolve {
			if transitionErr := tx.Conflicts.Transition(item.ID, item.ConflictState, constants.ConflictResolved, &resolvedBy); transitionErr != nil {
				return transitionErr
			}
			resolvedIDs = append(resolvedIDs, item.ID)
		}
		application := model.ConflictBatchApplication{BatchID: batch.ID, ActorID: actor.ID, IdempotencyKey: key, ResultProposalID: derived.ID}
		if createErr := tx.ReviewBatches.CreateApplication(&application); createErr != nil {
			return createErr
		}
		if auditErr := tx.Audits.Create(audit(actor, "proposal.created_from_conflict_batch", "BoundaryProposal", derived.ID, &derived.ParcelID, "{}", snapshot(derived))); auditErr != nil {
			return auditErr
		}
		for _, item := range toResolve {
			if auditErr := tx.Audits.Create(audit(actor, "conflict.suggestion_applied", "TopologyConflict", item.ID, &derived.ID, snapshot(item), snapshot(map[string]any{"conflict_state": constants.ConflictResolved, "proposal_id": derived.ID, "batch_id": batch.ID}))); auditErr != nil {
				return auditErr
			}
		}
		// Final conditional claim: only one transaction can move open -> applied.
		appliedAt := time.Now().UTC()
		if transitionErr := tx.ReviewBatches.Transition(batch.ID, constants.BatchOpen, constants.BatchApplied, map[string]any{
			"result_proposal_id": derived.ID, "applied_at": appliedAt, "reviewer_id": actor.ID,
		}); transitionErr != nil {
			return transitionErr
		}
		return tx.Audits.Create(audit(actor, "conflict.batch_applied", "ConflictReviewBatch", batch.ID, &derived.ID, snapshot(map[string]any{"batch_state": constants.BatchOpen}), snapshot(map[string]any{"batch_state": constants.BatchApplied, "result_proposal_id": derived.ID, "resolved_conflict_ids": resolvedIDs})))
	})
	if err != nil {
		// Concurrent duplicate or retry: the unique application index / batch
		// conditional transition guarantees only one application commits. Wait
		// briefly for the winning transaction to become visible, then either
		// replay the stored outcome or report a clean 409 for a different key.
		deadline := time.Now().Add(2 * time.Second)
		for {
			if replay, replayErr := s.replayBatchApplication(actor.ID, key); replayErr == nil {
				return replay, nil
			} else if !errors.Is(replayErr, repository.ErrNotFound) {
				return dto.BatchApplyResult{}, replayErr
			}
			finalBatch, loadErr := s.store.ReviewBatches.Get(batch.ID)
			switch {
			case loadErr == nil && finalBatch.BatchState == constants.BatchApplied && finalBatch.ResultProposalID != nil:
				// Winner finished with a different key: this request loses.
				return dto.BatchApplyResult{}, conflict("the review batch was applied concurrently by another request", nil)
			case loadErr == nil && finalBatch.BatchState != constants.BatchOpen:
				return dto.BatchApplyResult{}, conflict("the review batch can no longer be applied", nil)
			case loadErr != nil && !errors.Is(loadErr, repository.ErrNotFound):
				return dto.BatchApplyResult{}, wrapCadastral(loadErr, "reload review batch failed")
			case time.Now().After(deadline):
				return dto.BatchApplyResult{}, wrapCadastral(err, "apply review batch failed")
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	batch.BatchState = constants.BatchApplied
	batch.ResultProposalID = &derived.ID
	reloaded, err := s.store.Conflicts.ListByIDs(conflictIDs)
	if err != nil {
		return dto.BatchApplyResult{}, internal("reload applied batch conflicts failed", err)
	}
	return dto.BatchApplyResult{Batch: batch, Proposal: derived, Conflicts: reloaded, Replayed: false}, nil
}

func (s *CadastralService) replayBatchApplication(actorID uint, key string) (dto.BatchApplyResult, error) {
	application, err := s.store.ReviewBatches.GetApplication(actorID, key)
	if err != nil {
		return dto.BatchApplyResult{}, err
	}
	batch, err := s.store.ReviewBatches.Get(application.BatchID)
	if err != nil {
		return dto.BatchApplyResult{}, internal("load applied review batch failed", err)
	}
	proposal, err := s.store.Proposals.Get(application.ResultProposalID)
	if err != nil {
		return dto.BatchApplyResult{}, internal("load created proposal failed", err)
	}
	conflictIDs, err := decodeIDList(batch.ConflictIDs)
	if err != nil {
		return dto.BatchApplyResult{}, internal("stored batch conflict list is invalid", err)
	}
	conflicts, err := s.store.Conflicts.ListByIDs(conflictIDs)
	if err != nil {
		return dto.BatchApplyResult{}, internal("load applied conflicts failed", err)
	}
	return dto.BatchApplyResult{Batch: batch, Proposal: proposal, Conflicts: conflicts, Replayed: true}, nil
}

func (s *CadastralService) snapProposalInputs(item model.TopologyConflict) (model.BoundaryProposal, model.LandParcel, string, error) {
	proposal, err := s.store.Proposals.Get(item.ProposalID)
	if errors.Is(err, repository.ErrNotFound) {
		return model.BoundaryProposal{}, model.LandParcel{}, "", notFound("proposal")
	}
	if err != nil {
		return model.BoundaryProposal{}, model.LandParcel{}, "", internal("load proposal failed", err)
	}
	var suggestion struct {
		SnappedGeoJSON json.RawMessage `json:"snapped_geojson"`
	}
	if err := json.Unmarshal([]byte(item.SuggestedResolutionJSON), &suggestion); err != nil || len(suggestion.SnappedGeoJSON) == 0 {
		return model.BoundaryProposal{}, model.LandParcel{}, "", conflict("the selected conflict has no usable snapped-boundary suggestion", err)
	}
	parcel, parcelErr := s.store.Parcels.Get(proposal.ParcelID)
	if errors.Is(parcelErr, repository.ErrNotFound) {
		return model.BoundaryProposal{}, model.LandParcel{}, "", notFound("parcel")
	}
	if parcelErr != nil {
		return model.BoundaryProposal{}, model.LandParcel{}, "", internal("load parcel failed", parcelErr)
	}
	return proposal, parcel, string(suggestion.SnappedGeoJSON), nil
}

func dispositionTargetState(decision string) string {
	switch decision {
	case constants.DispositionConfirmed:
		return constants.ConflictConfirmed
	case constants.DispositionFalsePositive:
		return constants.ConflictFalsePositive
	case constants.DispositionResolutionProposed:
		return constants.ConflictResolutionProposed
	default:
		return ""
	}
}

// dispositionStatePath returns the ordered conflict states a batch conclusion
// moves through while the batch is still open. A reviewer may revise an earlier
// conclusion (for example confirmed -> false_positive) before applying; a
// conflict that is already resolved or closed is locked out of the batch.
func dispositionStatePath(current, decision string) ([]string, error) {
	target := dispositionTargetState(decision)
	if target == "" {
		return nil, errors.New("unsupported review decision")
	}
	if current == target {
		return nil, nil
	}
	switch current {
	case constants.ConflictDetected:
		switch target {
		case constants.ConflictConfirmed:
			return []string{constants.ConflictConfirmed}, nil
		case constants.ConflictFalsePositive:
			return []string{constants.ConflictFalsePositive}, nil
		case constants.ConflictResolutionProposed:
			return []string{constants.ConflictConfirmed, constants.ConflictResolutionProposed}, nil
		}
	case constants.ConflictConfirmed:
		switch target {
		case constants.ConflictResolutionProposed:
			return []string{constants.ConflictResolutionProposed}, nil
		case constants.ConflictFalsePositive:
			return []string{constants.ConflictFalsePositive}, nil
		}
	case constants.ConflictFalsePositive:
		switch target {
		case constants.ConflictConfirmed:
			return []string{constants.ConflictConfirmed}, nil
		case constants.ConflictResolutionProposed:
			return []string{constants.ConflictConfirmed, constants.ConflictResolutionProposed}, nil
		}
	case constants.ConflictResolutionProposed:
		switch target {
		case constants.ConflictConfirmed:
			return []string{constants.ConflictConfirmed}, nil
		case constants.ConflictFalsePositive:
			return []string{constants.ConflictFalsePositive}, nil
		}
	}
	return nil, errors.New("conflict is already finalized or the conclusion is not reachable")
}

func decodeIDList(raw string) ([]uint, error) {
	var ids []uint
	if err := json.Unmarshal([]byte(raw), &ids); err != nil {
		return nil, err
	}
	return ids, nil
}

func conflictIDsOf(items []model.TopologyConflict) []uint {
	ids := make([]uint, 0, len(items))
	for _, item := range items {
		ids = append(ids, item.ID)
	}
	return ids
}
