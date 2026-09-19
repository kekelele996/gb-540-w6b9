package service

import (
	"errors"
	"sync"
	"testing"

	"cadastral-boundary-topology-resolution/backend/internal/constants"
	"cadastral-boundary-topology-resolution/backend/internal/dto"
	"cadastral-boundary-topology-resolution/backend/internal/model"
)

// detectConflictBatchFixture runs detection on a proposal that overlaps a
// neighbour and returns the resulting conflicts for batch review tests.
func detectConflictBatchFixture(t *testing.T, svc *CadastralService) (proposalID uint, conflictIDs []uint, actor Actor) {
	t.Helper()
	surveyor := testActor(901, constants.RoleSurveyor, "batch-parcel")
	base := createTestParcel(t, svc, "P-BATCH-BASE", serviceTestPolygon(`[0,0],[10,0],[10,10],[0,10],[0,0]`), surveyor)
	createTestParcel(t, svc, "P-BATCH-NEIGHBOUR", serviceTestPolygon(`[10,0],[20,0],[20,10],[10,10],[10,0]`), testActor(902, constants.RoleSurveyor, "batch-neighbour"))
	createTestParcel(t, svc, "P-BATCH-NEIGHBOUR-2", serviceTestPolygon(`[0,9],[10,9],[10,20],[0,20],[0,9]`), testActor(905, constants.RoleSurveyor, "batch-neighbour-2"))
	proposal := createTestProposal(t, svc, base, serviceTestPolygon(`[0,0],[11,0],[11,10],[0,10],[0,0]`), testActor(901, constants.RoleSurveyor, "batch-proposal"))
	analyst := testActor(903, constants.RoleGISAnalyst, "batch-detect")
	findings, err := svc.DetectConflicts(dto.DetectConflictRequest{ProposalID: proposal.ID, SnapToleranceM: 0.1}, "batch-detect-key", analyst)
	if err != nil {
		t.Fatalf("DetectConflicts() error = %v", err)
	}
	if len(findings) < 2 {
		t.Fatalf("detection returned %d conflicts, want at least two", len(findings))
	}
	for _, finding := range findings {
		conflictIDs = append(conflictIDs, finding.ID)
	}
	return proposal.ID, conflictIDs, testActor(904, constants.RoleReviewer, "batch-reviewer")
}

func dispositionInputs(conflictIDs []uint, snapIndex int) []dto.ConflictDispositionInput {
	inputs := make([]dto.ConflictDispositionInput, 0, len(conflictIDs))
	for index, id := range conflictIDs {
		decision := constants.DispositionConfirmed
		if index == snapIndex {
			decision = constants.DispositionResolutionProposed
		}
		inputs = append(inputs, dto.ConflictDispositionInput{ConflictID: id, Decision: decision})
	}
	return inputs
}

func TestReviewBatchRejectsApplyBeforeEveryConclusion(t *testing.T) {
	svc, store := newCadastralTestService(t)
	proposalID, conflictIDs, reviewer := detectConflictBatchFixture(t, svc)

	view, err := svc.GetReviewBatchByProposal(proposalID, reviewer)
	if err != nil {
		t.Fatalf("GetReviewBatchByProposal() error = %v", err)
	}
	if view.Batch.BatchState != constants.BatchOpen || len(view.Conflicts) != len(conflictIDs) {
		t.Fatalf("initial batch view = %#v", view)
	}

	// Save only one conclusion; the batch must remain not ready to apply.
	partial := []dto.ConflictDispositionInput{{ConflictID: conflictIDs[0], Decision: constants.DispositionResolutionProposed}}
	if _, err := svc.SaveBatchDispositions(view.Batch.ID, dto.BatchDispositionRequest{Dispositions: partial}, reviewer); err != nil {
		t.Fatalf("SaveBatchDispositions(partial) error = %v", err)
	}
	reloaded, err := svc.GetReviewBatch(view.Batch.ID)
	if err != nil {
		t.Fatalf("GetReviewBatch() error = %v", err)
	}
	if reloaded.ReadyToApply || reloaded.MissingConclusion != len(conflictIDs)-1 || !reloaded.HasSuggestion {
		t.Fatalf("partial review view = %#v, want one suggestion and %d missing", reloaded, len(conflictIDs)-1)
	}

	_, err = svc.ApplyReviewBatch(view.Batch.ID, dto.BatchApplyRequest{}, "apply-too-early", reviewer)
	var appErr *AppError
	if !errors.As(err, &appErr) || appErr.Status != 409 {
		t.Fatalf("early apply error = %v, want 409", err)
	}

	// No draft proposal may have been created by the rejected apply.
	var proposalCount int64
	if err := store.DB.Model(&model.BoundaryProposal{}).Count(&proposalCount).Error; err != nil {
		t.Fatalf("count proposals: %v", err)
	}
	if proposalCount != 1 {
		t.Fatalf("proposal count = %d, want only the original proposal", proposalCount)
	}
}

func TestReviewBatchApplyCreatesOneDraftAndResolvesConflicts(t *testing.T) {
	svc, store := newCadastralTestService(t)
	proposalID, conflictIDs, reviewer := detectConflictBatchFixture(t, svc)
	view, err := svc.GetReviewBatchByProposal(proposalID, reviewer)
	if err != nil {
		t.Fatalf("open batch: %v", err)
	}
	inputs := dispositionInputs(conflictIDs, 0)
	saved, err := svc.SaveBatchDispositions(view.Batch.ID, dto.BatchDispositionRequest{Dispositions: inputs}, reviewer)
	if err != nil {
		t.Fatalf("SaveBatchDispositions() error = %v", err)
	}
	if saved.MissingConclusion != 0 || !saved.ReadyToApply {
		t.Fatalf("saved view = %#v, want ready batch", saved)
	}

	result, err := svc.ApplyReviewBatch(view.Batch.ID, dto.BatchApplyRequest{Rationale: "unified batch resolution"}, "apply-once-key", reviewer)
	if err != nil {
		t.Fatalf("ApplyReviewBatch() error = %v", err)
	}
	if result.Replayed || result.Proposal.ProposalState != constants.ProposalDraft || result.Proposal.Rationale != "unified batch resolution" {
		t.Fatalf("apply result = %#v", result)
	}
	if result.Batch.BatchState != constants.BatchApplied || result.Batch.ResultProposalID == nil || *result.Batch.ResultProposalID != result.Proposal.ID {
		t.Fatalf("applied batch = %#v", result.Batch)
	}
	resolved := 0
	for _, item := range result.Conflicts {
		if item.ConflictState == constants.ConflictResolved {
			resolved++
		}
	}
	if resolved != len(conflictIDs) {
		t.Fatalf("resolved conflicts = %d, want %d; states = %#v", resolved, len(conflictIDs), result.Conflicts)
	}

	persistedView, err := svc.GetReviewBatch(view.Batch.ID)
	if err != nil {
		t.Fatalf("reload batch: %v", err)
	}
	if persistedView.Batch.BatchState != constants.BatchApplied {
		t.Fatalf("read-back batch state = %s, want applied", persistedView.Batch.BatchState)
	}

	// Exactly one new draft proposal exists.
	var proposals []model.BoundaryProposal
	if err := store.DB.Order("id ASC").Find(&proposals).Error; err != nil {
		t.Fatalf("list proposals: %v", err)
	}
	if len(proposals) != 2 || proposals[1].ProposalState != constants.ProposalDraft || proposals[1].Version != proposals[0].Version+1 {
		t.Fatalf("proposals after apply = %#v", proposals)
	}
}

func TestReviewBatchApplyIsIdempotentUnderReplayAndConcurrency(t *testing.T) {
	svc, store := newCadastralTestService(t)
	proposalID, conflictIDs, reviewer := detectConflictBatchFixture(t, svc)
	view, err := svc.GetReviewBatchByProposal(proposalID, reviewer)
	if err != nil {
		t.Fatalf("open batch: %v", err)
	}
	if _, err := svc.SaveBatchDispositions(view.Batch.ID, dto.BatchDispositionRequest{Dispositions: dispositionInputs(conflictIDs, 0)}, reviewer); err != nil {
		t.Fatalf("save dispositions: %v", err)
	}

	// Replaying the exact same actor/key returns the stored outcome.
	first, err := svc.ApplyReviewBatch(view.Batch.ID, dto.BatchApplyRequest{}, "replay-apply-key", reviewer)
	if err != nil {
		t.Fatalf("first apply: %v", err)
	}
	replay, err := svc.ApplyReviewBatch(view.Batch.ID, dto.BatchApplyRequest{}, "replay-apply-key", reviewer)
	if err != nil {
		t.Fatalf("replayed apply: %v", err)
	}
	if !replay.Replayed || replay.Proposal.ID != first.Proposal.ID {
		t.Fatalf("replay = %#v, first = %#v", replay, first)
	}

	// Every further attempt, even with a distinct key, is rejected.
	_, err = svc.ApplyReviewBatch(view.Batch.ID, dto.BatchApplyRequest{}, "later-distinct-key", reviewer)
	var appErr *AppError
	if !errors.As(err, &appErr) || appErr.Status != 409 {
		t.Fatalf("post-apply request error = %v, want 409", err)
	}

	var proposalCount int64
	if err := store.DB.Model(&model.BoundaryProposal{}).Count(&proposalCount).Error; err != nil {
		t.Fatalf("count proposals: %v", err)
	}
	if proposalCount != 2 {
		t.Fatalf("proposal count = %d, want exactly one created draft", proposalCount)
	}
	var applicationCount int64
	if err := store.DB.Model(&model.ConflictBatchApplication{}).Count(&applicationCount).Error; err != nil {
		t.Fatalf("count applications: %v", err)
	}
	if applicationCount != 1 {
		t.Fatalf("application records = %d, want 1", applicationCount)
	}
}

func TestReviewBatchConcurrentFirstApplySucceedsOnce(t *testing.T) {
	svc, store := newCadastralTestService(t)
	proposalID, conflictIDs, reviewer := detectConflictBatchFixture(t, svc)
	view, err := svc.GetReviewBatchByProposal(proposalID, reviewer)
	if err != nil {
		t.Fatalf("open batch: %v", err)
	}
	if _, err := svc.SaveBatchDispositions(view.Batch.ID, dto.BatchDispositionRequest{Dispositions: dispositionInputs(conflictIDs, 0)}, reviewer); err != nil {
		t.Fatalf("save dispositions: %v", err)
	}

	const workers = 8
	start := make(chan struct{})
	var wg sync.WaitGroup
	results := make([]uint, workers)
	replayed := make([]bool, workers)
	errs := make([]error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			<-start
			result, applyErr := svc.ApplyReviewBatch(view.Batch.ID, dto.BatchApplyRequest{}, "concurrent-first-key", reviewer)
			if applyErr == nil {
				results[index] = result.Proposal.ID
				replayed[index] = result.Replayed
			}
			errs[index] = applyErr
		}(i)
	}
	close(start)
	wg.Wait()

	created := 0
	winnerProposal := uint(0)
	for index, applyErr := range errs {
		if applyErr != nil {
			t.Fatalf("same-key concurrent apply %d error = %v, want replay success", index, applyErr)
		}
		if results[index] == 0 {
			t.Fatalf("apply %d returned no proposal", index)
		}
		if winnerProposal == 0 {
			winnerProposal = results[index]
		}
		if results[index] != winnerProposal {
			t.Fatalf("apply %d proposal = %d, want shared winner %d", index, results[index], winnerProposal)
		}
		if !replayed[index] {
			created++
		}
	}
	if created != 1 {
		t.Fatalf("committing applies = %d, want exactly one transaction creating the draft", created)
	}

	var proposalCount int64
	if err := store.DB.Model(&model.BoundaryProposal{}).Count(&proposalCount).Error; err != nil {
		t.Fatalf("count proposals: %v", err)
	}
	if proposalCount != 2 {
		t.Fatalf("proposal count = %d, want exactly one created draft", proposalCount)
	}
	var applicationCount int64
	if err := store.DB.Model(&model.ConflictBatchApplication{}).Count(&applicationCount).Error; err != nil {
		t.Fatalf("count applications: %v", err)
	}
	if applicationCount != 1 {
		t.Fatalf("application records = %d, want 1", applicationCount)
	}
}

func TestReviewBatchConcurrentDistinctKeysOnlyOneWins(t *testing.T) {
	svc, store := newCadastralTestService(t)
	proposalID, conflictIDs, reviewer := detectConflictBatchFixture(t, svc)
	view, err := svc.GetReviewBatchByProposal(proposalID, reviewer)
	if err != nil {
		t.Fatalf("open batch: %v", err)
	}
	if _, err := svc.SaveBatchDispositions(view.Batch.ID, dto.BatchDispositionRequest{Dispositions: dispositionInputs(conflictIDs, 0)}, reviewer); err != nil {
		t.Fatalf("save dispositions: %v", err)
	}

	const workers = 8
	start := make(chan struct{})
	var wg sync.WaitGroup
	results := make([]uint, workers)
	errs := make([]error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			<-start
			result, applyErr := svc.ApplyReviewBatch(view.Batch.ID, dto.BatchApplyRequest{}, "distinct-race-key-"+string(rune('a'+index)), reviewer)
			if applyErr == nil {
				results[index] = result.Proposal.ID
			}
			errs[index] = applyErr
		}(i)
	}
	close(start)
	wg.Wait()

	successes := map[uint]bool{}
	failures := 0
	for index, applyErr := range errs {
		if applyErr == nil {
			successes[results[index]] = true
			continue
		}
		failures++
		var appErr *AppError
		if !errors.As(applyErr, &appErr) || appErr.Status != 409 {
			t.Fatalf("losing concurrent apply error = %v, want 409", applyErr)
		}
	}
	if len(successes) != 1 || failures != workers-1 {
		t.Fatalf("concurrent outcomes = %d successes %v, %d failures; want exactly one success", len(successes), successes, failures)
	}

	var proposalCount int64
	if err := store.DB.Model(&model.BoundaryProposal{}).Count(&proposalCount).Error; err != nil {
		t.Fatalf("count proposals: %v", err)
	}
	if proposalCount != 2 {
		t.Fatalf("proposal count = %d, want exactly one created draft", proposalCount)
	}
	var applicationCount int64
	if err := store.DB.Model(&model.ConflictBatchApplication{}).Count(&applicationCount).Error; err != nil {
		t.Fatalf("count applications: %v", err)
	}
	if applicationCount != 1 {
		t.Fatalf("application records = %d, want 1", applicationCount)
	}
}

func TestReviewBatchFailedDispositionLeavesStateUntouched(t *testing.T) {
	svc, store := newCadastralTestService(t)
	proposalID, conflictIDs, reviewer := detectConflictBatchFixture(t, svc)
	view, err := svc.GetReviewBatchByProposal(proposalID, reviewer)
	if err != nil {
		t.Fatalf("open batch: %v", err)
	}

	// First resolve one conflict out-of-band through the normal lifecycle, then
	// submit a batch that tries to re-open the locked conflict as a false
	// positive. A resolved or closed conflict must never be rewritten by the
	// batch.
	locked := conflictIDs[0]
	if _, err := svc.TransitionConflict(locked, dto.ConflictTransitionRequest{To: constants.ConflictConfirmed}, reviewer); err != nil {
		t.Fatalf("pre-confirm conflict: %v", err)
	}
	if _, err := svc.TransitionConflict(locked, dto.ConflictTransitionRequest{To: constants.ConflictResolutionProposed}, reviewer); err != nil {
		t.Fatalf("pre-propose conflict: %v", err)
	}
	if _, err := svc.TransitionConflict(locked, dto.ConflictTransitionRequest{To: constants.ConflictResolved}, reviewer); err != nil {
		t.Fatalf("pre-resolve conflict: %v", err)
	}
	stale := []dto.ConflictDispositionInput{{ConflictID: locked, Decision: constants.DispositionFalsePositive}}
	_, err = svc.SaveBatchDispositions(view.Batch.ID, dto.BatchDispositionRequest{Dispositions: stale}, reviewer)
	var appErr *AppError
	if !errors.As(err, &appErr) || appErr.Status != 409 {
		t.Fatalf("stale disposition error = %v, want 409", err)
	}

	// The conflict must still be resolved, no disposition row and no audit
	// evidence for the rejected request may exist.
	item, err := svc.GetConflict(locked)
	if err != nil {
		t.Fatalf("reload conflict: %v", err)
	}
	if item.ConflictState != constants.ConflictResolved {
		t.Fatalf("conflict state = %s, want resolved untouched", item.ConflictState)
	}
	var dispositionCount int64
	if err := store.DB.Model(&model.ConflictDisposition{}).Count(&dispositionCount).Error; err != nil {
		t.Fatalf("count dispositions: %v", err)
	}
	if dispositionCount != 0 {
		t.Fatalf("disposition rows = %d, want 0 after failed batch", dispositionCount)
	}
	var auditCount int64
	if err := store.DB.Model(&model.AuditLog{}).Where("action = ?", "conflict.disposition_recorded").Count(&auditCount).Error; err != nil {
		t.Fatalf("count disposition audits: %v", err)
	}
	if auditCount != 0 {
		t.Fatalf("disposition audit rows = %d, want 0", auditCount)
	}
}

func TestReviewBatchRequiresSuggestionForAtLeastOneConflict(t *testing.T) {
	svc, _ := newCadastralTestService(t)
	proposalID, conflictIDs, reviewer := detectConflictBatchFixture(t, svc)
	view, err := svc.GetReviewBatchByProposal(proposalID, reviewer)
	if err != nil {
		t.Fatalf("open batch: %v", err)
	}
	inputs := make([]dto.ConflictDispositionInput, 0, len(conflictIDs))
	for _, id := range conflictIDs {
		inputs = append(inputs, dto.ConflictDispositionInput{ConflictID: id, Decision: constants.DispositionConfirmed})
	}
	if _, err := svc.SaveBatchDispositions(view.Batch.ID, dto.BatchDispositionRequest{Dispositions: inputs}, reviewer); err != nil {
		t.Fatalf("save all-confirmed dispositions: %v", err)
	}
	reloaded, err := svc.GetReviewBatch(view.Batch.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if reloaded.ReadyToApply || reloaded.HasSuggestion {
		t.Fatalf("batch without suggestion = %#v, want not ready", reloaded)
	}
	_, err = svc.ApplyReviewBatch(view.Batch.ID, dto.BatchApplyRequest{}, "apply-without-snap", reviewer)
	var appErr *AppError
	if !errors.As(err, &appErr) || appErr.Status != 409 {
		t.Fatalf("apply without suggestion error = %v, want 409", err)
	}
}

func TestReviewBatchDispositionsAreReadBackAfterRefresh(t *testing.T) {
	svc, _ := newCadastralTestService(t)
	proposalID, conflictIDs, reviewer := detectConflictBatchFixture(t, svc)
	view, err := svc.GetReviewBatchByProposal(proposalID, reviewer)
	if err != nil {
		t.Fatalf("open batch: %v", err)
	}
	inputs := []dto.ConflictDispositionInput{
		{ConflictID: conflictIDs[0], Decision: constants.DispositionResolutionProposed, ReviewNote: "use deterministic snap"},
	}
	for _, id := range conflictIDs[1:] {
		inputs = append(inputs, dto.ConflictDispositionInput{ConflictID: id, Decision: constants.DispositionFalsePositive, ReviewNote: "survey noise"})
	}
	saved, err := svc.SaveBatchDispositions(view.Batch.ID, dto.BatchDispositionRequest{Dispositions: inputs}, reviewer)
	if err != nil {
		t.Fatalf("save dispositions: %v", err)
	}
	readBack, err := svc.GetReviewBatch(saved.Batch.ID)
	if err != nil {
		t.Fatalf("refresh batch: %v", err)
	}
	if len(readBack.Dispositions) != len(conflictIDs) || readBack.MissingConclusion != 0 || !readBack.HasSuggestion {
		t.Fatalf("read-back = %#v", readBack)
	}
	for _, disposition := range readBack.Dispositions {
		if disposition.ReviewerID != reviewer.ID {
			t.Fatalf("disposition reviewer = %d, want %d", disposition.ReviewerID, reviewer.ID)
		}
	}
}
