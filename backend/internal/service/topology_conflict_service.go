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
		run := model.TopologyDetectionRun{ProposalID: proposal.ID, ActorID: actor.ID, IdempotencyKey: key, RequestHash: requestHash, InputHash: inputHash, ResultIDs: "[]"}
		if createErr := tx.DetectionRuns.Create(&run); createErr != nil {
			return createErr
		}
		resultIDs := make([]uint, 0, len(items))
		for index := range items {
			items[index].DetectionRunID = run.ID
			if createErr := tx.Conflicts.Create(&items[index]); createErr != nil {
				return createErr
			}
			resultIDs = append(resultIDs, items[index].ID)
			if auditErr := tx.Audits.Create(audit(actor, "conflict.detected", "TopologyConflict", items[index].ID, &parcel.ID, "{}", snapshot(items[index]))); auditErr != nil {
				return auditErr
			}
		}
		encodedIDs, encodeErr := json.Marshal(resultIDs)
		if encodeErr != nil {
			return encodeErr
		}
		run.ResultIDs = string(encodedIDs)
		if updateErr := tx.DetectionRuns.UpdateResultIDs(run.ID, run.ResultIDs); updateErr != nil {
			return updateErr
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
	items, err := s.detectionRunConflicts(run)
	if err != nil {
		return nil, err
	}
	return items, nil
}

func (s *CadastralService) detectionRunConflicts(run model.TopologyDetectionRun) ([]model.TopologyConflict, error) {
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

// ApplyConflictBatch disposes one detection run as a unit: every conflict must
// already be concluded (confirmed, false_positive, or resolution_proposed) and
// at least one must carry a proposed resolution. It creates exactly one new
// draft proposal from the shared snapped suggestion, resolves every confirmed
// or proposed conflict in the batch, and leaves false positives untouched. The
// draft, conflict transitions, idempotency record, and audit entries commit in
// a single transaction, so any failure leaves all of them unchanged.
func (s *CadastralService) ApplyConflictBatch(req dto.ApplyConflictBatchRequest, idempotencyKey string, actor Actor) (model.BoundaryProposal, []model.TopologyConflict, error) {
	key := strings.TrimSpace(idempotencyKey)
	if key == "" || len(key) > 128 {
		return model.BoundaryProposal{}, nil, invalid("Idempotency-Key must contain between 1 and 128 characters", nil)
	}
	if err := requireAnyRole(actor, constants.RoleReviewer, constants.RoleAdmin); err != nil {
		return model.BoundaryProposal{}, nil, err
	}
	rationale := strings.TrimSpace(req.Rationale)
	requestHash := geometry.Hash(strconv.FormatUint(uint64(req.DetectionRunID), 10), rationale)
	if existing, findErr := s.store.BatchApplyRuns.GetByActorKey(actor.ID, key); findErr == nil {
		return s.replayBatchApply(existing, requestHash)
	} else if !errors.Is(findErr, repository.ErrNotFound) {
		return model.BoundaryProposal{}, nil, internal("check batch apply idempotency failed", findErr)
	}

	run, err := s.store.DetectionRuns.Get(req.DetectionRunID)
	if errors.Is(err, repository.ErrNotFound) {
		return model.BoundaryProposal{}, nil, notFound("detection run")
	}
	if err != nil {
		return model.BoundaryProposal{}, nil, internal("load detection run failed", err)
	}
	items, err := s.detectionRunConflicts(run)
	if err != nil {
		return model.BoundaryProposal{}, nil, err
	}
	if len(items) == 0 {
		return model.BoundaryProposal{}, nil, conflict("the detection run has no conflicts to dispose", nil)
	}
	proposed := 0
	for _, item := range items {
		switch item.ConflictState {
		case constants.ConflictConfirmed, constants.ConflictFalsePositive:
		case constants.ConflictResolutionProposed:
			proposed++
		default:
			return model.BoundaryProposal{}, nil, conflict(fmt.Sprintf("conflict %d is still %s; every conflict in the batch must be confirmed, marked false positive, or given a proposed resolution before applying", item.ID, item.ConflictState), nil)
		}
	}
	if proposed == 0 {
		return model.BoundaryProposal{}, nil, conflict("at least one conflict in the batch must have a proposed resolution before applying", nil)
	}
	proposal, err := s.store.Proposals.Get(run.ProposalID)
	if errors.Is(err, repository.ErrNotFound) {
		return model.BoundaryProposal{}, nil, notFound("proposal")
	}
	if err != nil {
		return model.BoundaryProposal{}, nil, internal("load proposal failed", err)
	}
	parcel, err := s.store.Parcels.Get(proposal.ParcelID)
	if errors.Is(err, repository.ErrNotFound) {
		return model.BoundaryProposal{}, nil, notFound("parcel")
	}
	if err != nil {
		return model.BoundaryProposal{}, nil, internal("load parcel failed", err)
	}
	var snappedGeoJSON json.RawMessage
	for _, item := range items {
		if item.ConflictState != constants.ConflictResolutionProposed {
			continue
		}
		var suggestion struct {
			SnappedGeoJSON json.RawMessage `json:"snapped_geojson"`
		}
		if err := json.Unmarshal([]byte(item.SuggestedResolutionJSON), &suggestion); err != nil || len(suggestion.SnappedGeoJSON) == 0 {
			return model.BoundaryProposal{}, nil, conflict(fmt.Sprintf("conflict %d has no usable snapped-boundary suggestion", item.ID), err)
		}
		snappedGeoJSON = suggestion.SnappedGeoJSON
		break
	}
	polygon, parseErr := geometry.ParsePolygon(string(snappedGeoJSON))
	if parseErr != nil {
		return model.BoundaryProposal{}, nil, internal("stored suggested geometry is invalid", parseErr)
	}
	if rationale == "" {
		rationale = fmt.Sprintf("Applied batch disposition of detection run %d.", run.ID)
	}
	derived := model.BoundaryProposal{
		ParcelID: proposal.ParcelID, BaseVersion: parcel.BoundaryVersion, ProposedGeoJSON: string(snappedGeoJSON), ObservationIDs: proposal.ObservationIDs,
		SnapToleranceM: proposal.SnapToleranceM, AreaDeltaSquareM: polygon.Area - parcel.AreaSquareM, ProposalState: constants.ProposalDraft,
		Rationale: rationale, Version: proposal.Version + 1, CreatedBy: actor.ID,
	}
	resolvedBy := actor.ID
	err = s.store.Transaction(func(tx *repository.Store) error {
		if createErr := tx.Proposals.Create(&derived); createErr != nil {
			return createErr
		}
		for index := range items {
			item := items[index]
			if item.ConflictState == constants.ConflictFalsePositive {
				continue
			}
			if transitionErr := tx.Conflicts.Transition(item.ID, item.ConflictState, constants.ConflictResolved, &resolvedBy); transitionErr != nil {
				return conflict(fmt.Sprintf("conflict %d changed state while applying the batch; no changes were kept", item.ID), transitionErr)
			}
			if auditErr := tx.Audits.Create(audit(actor, "conflict.state_changed", "TopologyConflict", item.ID, &derived.ID, snapshot(item), snapshot(map[string]any{"conflict_state": constants.ConflictResolved, "detection_run_id": run.ID, "proposal_id": derived.ID}))); auditErr != nil {
				return auditErr
			}
		}
		applyRun := model.TopologyBatchApplyRun{DetectionRunID: run.ID, ProposalID: proposal.ID, ActorID: actor.ID, IdempotencyKey: key, RequestHash: requestHash, CreatedProposalID: derived.ID}
		if createErr := tx.BatchApplyRuns.Create(&applyRun); createErr != nil {
			return createErr
		}
		if auditErr := tx.Audits.Create(audit(actor, "proposal.created_from_conflict", "BoundaryProposal", derived.ID, &derived.ParcelID, "{}", snapshot(derived))); auditErr != nil {
			return auditErr
		}
		return tx.Audits.Create(audit(actor, "conflict.batch_applied", "TopologyBatchApplyRun", applyRun.ID, &derived.ID, "{}", snapshot(applyRun)))
	})
	if err != nil {
		if existing, findErr := s.store.BatchApplyRuns.GetByActorKey(actor.ID, key); findErr == nil {
			return s.replayBatchApply(existing, requestHash)
		}
		return model.BoundaryProposal{}, nil, wrapCadastral(err, "apply conflict batch failed")
	}
	reloaded, reloadErr := s.detectionRunConflicts(run)
	if reloadErr != nil {
		return model.BoundaryProposal{}, nil, reloadErr
	}
	return derived, reloaded, nil
}

func (s *CadastralService) replayBatchApply(run model.TopologyBatchApplyRun, requestHash string) (model.BoundaryProposal, []model.TopologyConflict, error) {
	if run.RequestHash != requestHash {
		return model.BoundaryProposal{}, nil, conflict("Idempotency-Key has already been used with a different request", nil)
	}
	proposal, err := s.store.Proposals.Get(run.CreatedProposalID)
	if err != nil {
		return model.BoundaryProposal{}, nil, internal("load idempotent batch apply proposal failed", err)
	}
	detectionRun, err := s.store.DetectionRuns.Get(run.DetectionRunID)
	if err != nil {
		return model.BoundaryProposal{}, nil, internal("load idempotent batch apply run failed", err)
	}
	items, err := s.detectionRunConflicts(detectionRun)
	if err != nil {
		return model.BoundaryProposal{}, nil, err
	}
	return proposal, items, nil
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
