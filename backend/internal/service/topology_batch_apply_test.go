package service

import (
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"

	"cadastral-boundary-topology-resolution/backend/internal/constants"
	"cadastral-boundary-topology-resolution/backend/internal/dto"
	"cadastral-boundary-topology-resolution/backend/internal/model"
	"cadastral-boundary-topology-resolution/backend/internal/repository"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// newBatchTestService uses a temporary file database in WAL mode: shared-cache
// in-memory SQLite reports SQLITE_LOCKED for concurrent writers and ignores the
// busy timeout, while a file database serializes concurrent batch-apply
// transactions so the state guards can arbitrate a single winner.
func newBatchTestService(t *testing.T) (*CadastralService, *repository.Store) {
	t.Helper()
	dsn := "file:" + filepath.Join(t.TempDir(), "batch-apply.db") + "?_busy_timeout=10000&_journal_mode=WAL"
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatalf("open SQLite: %v", err)
	}
	if err := db.AutoMigrate(&model.User{}, &model.AuditLog{}, &model.LandParcel{}, &model.SurveyObservation{}, &model.BoundaryProposal{}, &model.TopologyConflict{}, &model.TopologyDetectionRun{}, &model.TopologyBatchApplyRun{}); err != nil {
		t.Fatalf("migrate SQLite: %v", err)
	}
	store := repository.NewStore(db)
	return NewCadastralService(store), store
}

// createConflictBatch runs one detection whose candidate overlaps three
// neighbouring parcels, so a single run returns several conflicts to dispose.
func createConflictBatch(t *testing.T, svc *CadastralService, detectionKey string) (model.BoundaryProposal, []model.TopologyConflict) {
	t.Helper()
	surveyor := testActor(601, constants.RoleSurveyor, "batch-parcel-create")
	base := createTestParcel(t, svc, "P-BATCH-BASE", serviceTestPolygon(`[0,0],[10,0],[10,10],[0,10],[0,0]`), surveyor)
	createTestParcel(t, svc, "P-BATCH-EAST", serviceTestPolygon(`[10,0],[20,0],[20,10],[10,10],[10,0]`), surveyor)
	createTestParcel(t, svc, "P-BATCH-NORTH", serviceTestPolygon(`[0,10],[10,10],[10,20],[0,20],[0,10]`), surveyor)
	createTestParcel(t, svc, "P-BATCH-WEST", serviceTestPolygon(`[-10,0],[0,0],[0,10],[-10,10],[-10,0]`), surveyor)
	proposal := createTestProposal(t, svc, base, serviceTestPolygon(`[-1,0],[11,0],[11,11],[-1,11],[-1,0]`), surveyor)
	items, err := svc.DetectConflicts(dto.DetectConflictRequest{ProposalID: proposal.ID, SnapToleranceM: 0.1}, detectionKey, testActor(602, constants.RoleGISAnalyst, "batch-detect"))
	if err != nil {
		t.Fatalf("DetectConflicts() error = %v", err)
	}
	if len(items) < 3 {
		t.Fatalf("DetectConflicts() produced %d conflicts, want at least 3 for a batch fixture", len(items))
	}
	for _, item := range items {
		if item.DetectionRunID == 0 {
			t.Fatalf("conflict %d has no detection run id", item.ID)
		}
		if item.DetectionRunID != items[0].DetectionRunID {
			t.Fatalf("conflict %d run id = %d, want shared run %d", item.ID, item.DetectionRunID, items[0].DetectionRunID)
		}
	}
	return proposal, items
}

// concludeBatch disposes every conflict in the batch: the first carries the
// proposed resolution, the last is marked false positive, the rest confirmed.
func concludeBatch(t *testing.T, svc *CadastralService, items []model.TopologyConflict, reviewer Actor) {
	t.Helper()
	for index, item := range items {
		target := constants.ConflictConfirmed
		if index == len(items)-1 {
			target = constants.ConflictFalsePositive
		}
		if _, err := svc.TransitionConflict(item.ID, dto.ConflictTransitionRequest{To: target}, reviewer); err != nil {
			t.Fatalf("transition conflict %d to %s: %v", item.ID, target, err)
		}
	}
	if _, err := svc.TransitionConflict(items[0].ID, dto.ConflictTransitionRequest{To: constants.ConflictResolutionProposed}, reviewer); err != nil {
		t.Fatalf("propose resolution on conflict %d: %v", items[0].ID, err)
	}
}

func countRows(t *testing.T, store *repository.Store, modelValue any, where string, args ...any) int64 {
	t.Helper()
	var count int64
	query := store.DB.Model(modelValue)
	if where != "" {
		query = query.Where(where, args...)
	}
	if err := query.Count(&count).Error; err != nil {
		t.Fatalf("count rows: %v", err)
	}
	return count
}

func conflictStates(t *testing.T, svc *CadastralService, items []model.TopologyConflict) map[uint]string {
	t.Helper()
	states := make(map[uint]string, len(items))
	for _, item := range items {
		reloaded, err := svc.GetConflict(item.ID)
		if err != nil {
			t.Fatalf("reload conflict %d: %v", item.ID, err)
		}
		states[item.ID] = reloaded.ConflictState
	}
	return states
}

func TestApplyConflictBatchDisposesRunAtomically(t *testing.T) {
	svc, store := newBatchTestService(t)
	proposal, items := createConflictBatch(t, svc, "batch-happy-detect")
	reviewer := testActor(610, constants.RoleReviewer, "batch-happy-review")
	concludeBatch(t, svc, items, reviewer)

	applyActor := testActor(610, constants.RoleReviewer, "batch-happy-apply")
	derived, applied, err := svc.ApplyConflictBatch(dto.ApplyConflictBatchRequest{DetectionRunID: items[0].DetectionRunID, Rationale: "整批吸附消解"}, "batch-happy-key", applyActor)
	if err != nil {
		t.Fatalf("ApplyConflictBatch() error = %v", err)
	}
	if derived.ProposalState != constants.ProposalDraft || derived.Version != proposal.Version+1 || derived.CreatedBy != reviewer.ID || derived.ParcelID != proposal.ParcelID {
		t.Fatalf("derived draft = %#v, want a new draft version of proposal %d", derived, proposal.ID)
	}
	if len(applied) != len(items) {
		t.Fatalf("applied conflicts = %d, want the whole batch of %d", len(applied), len(items))
	}
	for _, item := range applied {
		want := constants.ConflictResolved
		if item.ID == items[len(items)-1].ID {
			want = constants.ConflictFalsePositive
		}
		if item.ConflictState != want {
			t.Fatalf("conflict %d state = %s, want %s", item.ID, item.ConflictState, want)
		}
		if want == constants.ConflictResolved && (item.ResolvedBy == nil || *item.ResolvedBy != reviewer.ID) {
			t.Fatalf("conflict %d resolved_by = %v, want reviewer %d", item.ID, item.ResolvedBy, reviewer.ID)
		}
	}
	// A refresh re-reads exactly what the transaction committed.
	states := conflictStates(t, svc, items)
	for id, state := range states {
		want := constants.ConflictResolved
		if id == items[len(items)-1].ID {
			want = constants.ConflictFalsePositive
		}
		if state != want {
			t.Fatalf("reloaded conflict %d state = %s, want %s", id, state, want)
		}
	}
	if got := countRows(t, store, &model.BoundaryProposal{}, ""); got != 2 {
		t.Fatalf("proposal count = %d, want the source proposal plus one derived draft", got)
	}
	if got := countRows(t, store, &model.AuditLog{}, "request_id = ?", "batch-happy-apply"); got != int64(len(items)+1) {
		t.Fatalf("apply audit entries = %d, want %d state changes plus draft and batch records", got, len(items)+1)
	}
	if got := countRows(t, store, &model.AuditLog{}, "action = ? AND resource_type = ?", "conflict.batch_applied", "TopologyBatchApplyRun"); got != 1 {
		t.Fatalf("batch_applied audit entries = %d, want 1", got)
	}
}

func TestApplyConflictBatchRequiresConcludedBatch(t *testing.T) {
	svc, store := newBatchTestService(t)
	_, items := createConflictBatch(t, svc, "batch-guard-detect")
	reviewer := testActor(620, constants.RoleReviewer, "batch-guard-review")
	request := dto.ApplyConflictBatchRequest{DetectionRunID: items[0].DetectionRunID}

	if _, _, err := svc.ApplyConflictBatch(request, "batch-guard-key-a", reviewer); err == nil {
		t.Fatal("ApplyConflictBatch() with undecided conflicts succeeded, want 409")
	} else {
		var appErr *AppError
		if !errors.As(err, &appErr) || appErr.Code != CodeConflict || appErr.Status != 409 {
			t.Fatalf("undecided batch error = %v, want 409 %s", err, CodeConflict)
		}
	}
	for _, item := range items {
		if _, err := svc.TransitionConflict(item.ID, dto.ConflictTransitionRequest{To: constants.ConflictConfirmed}, reviewer); err != nil {
			t.Fatalf("confirm conflict %d: %v", item.ID, err)
		}
	}
	if _, _, err := svc.ApplyConflictBatch(request, "batch-guard-key-b", reviewer); err == nil {
		t.Fatal("ApplyConflictBatch() without a proposed resolution succeeded, want 409")
	} else {
		var appErr *AppError
		if !errors.As(err, &appErr) || appErr.Code != CodeConflict || appErr.Status != 409 {
			t.Fatalf("no-proposal batch error = %v, want 409 %s", err, CodeConflict)
		}
	}
	if got := countRows(t, store, &model.BoundaryProposal{}, ""); got != 1 {
		t.Fatalf("proposal count = %d, want only the source proposal after rejected applies", got)
	}
	if got := countRows(t, store, &model.TopologyBatchApplyRun{}, ""); got != 0 {
		t.Fatalf("batch apply runs = %d, want 0 after rejected applies", got)
	}
	states := conflictStates(t, svc, items)
	for id, state := range states {
		if state != constants.ConflictConfirmed {
			t.Fatalf("conflict %d state = %s, want confirmed after rejected applies", id, state)
		}
	}

	var appErr *AppError
	if _, _, err := svc.ApplyConflictBatch(dto.ApplyConflictBatchRequest{DetectionRunID: 999999}, "batch-guard-key-c", reviewer); !errors.As(err, &appErr) || appErr.Code != CodeNotFound {
		t.Fatalf("unknown detection run error = %v, want 404 %s", err, CodeNotFound)
	}
	if _, _, err := svc.ApplyConflictBatch(request, "", reviewer); !errors.As(err, &appErr) || appErr.Code != CodeInvalidInput {
		t.Fatalf("missing idempotency key error = %v, want 400 %s", err, CodeInvalidInput)
	}
	if _, _, err := svc.ApplyConflictBatch(request, "batch-guard-key-d", testActor(621, constants.RoleGISAnalyst, "batch-guard-role")); !errors.As(err, &appErr) || appErr.Code != CodeForbidden {
		t.Fatalf("non-reviewer apply error = %v, want 403 %s", err, CodeForbidden)
	}
}

func TestApplyConflictBatchIsIdempotent(t *testing.T) {
	svc, store := newBatchTestService(t)
	_, items := createConflictBatch(t, svc, "batch-idem-detect")
	reviewer := testActor(630, constants.RoleReviewer, "batch-idem-review")
	concludeBatch(t, svc, items, reviewer)
	request := dto.ApplyConflictBatchRequest{DetectionRunID: items[0].DetectionRunID, Rationale: "整批吸附消解"}

	derived, first, err := svc.ApplyConflictBatch(request, "batch-idem-key", reviewer)
	if err != nil {
		t.Fatalf("first ApplyConflictBatch() error = %v", err)
	}
	replayed, second, err := svc.ApplyConflictBatch(request, "batch-idem-key", reviewer)
	if err != nil {
		t.Fatalf("replayed ApplyConflictBatch() error = %v", err)
	}
	if replayed.ID != derived.ID {
		t.Fatalf("replayed proposal id = %d, want %d", replayed.ID, derived.ID)
	}
	for index := range second {
		if second[index].ID != first[index].ID || second[index].ConflictState != first[index].ConflictState {
			t.Fatalf("replayed conflict = %#v, want %#v", second[index], first[index])
		}
	}
	if got := countRows(t, store, &model.BoundaryProposal{}, ""); got != 2 {
		t.Fatalf("proposal count = %d, want exactly one derived draft after replay", got)
	}
	if got := countRows(t, store, &model.TopologyBatchApplyRun{}, ""); got != 1 {
		t.Fatalf("batch apply runs = %d, want 1 after replay", got)
	}

	var appErr *AppError
	changed := dto.ApplyConflictBatchRequest{DetectionRunID: items[0].DetectionRunID, Rationale: "另一份理由"}
	if _, _, err := svc.ApplyConflictBatch(changed, "batch-idem-key", reviewer); !errors.As(err, &appErr) || appErr.Code != CodeConflict || appErr.Status != 409 {
		t.Fatalf("changed request with same key error = %v, want 409 %s", err, CodeConflict)
	}
	if _, _, err := svc.ApplyConflictBatch(request, "batch-idem-key-other", reviewer); !errors.As(err, &appErr) || appErr.Code != CodeConflict || appErr.Status != 409 {
		t.Fatalf("second apply with a new key error = %v, want 409 %s", err, CodeConflict)
	}
	if got := countRows(t, store, &model.BoundaryProposal{}, ""); got != 2 {
		t.Fatalf("proposal count = %d, want no extra draft after duplicate applies", got)
	}
	if got := countRows(t, store, &model.TopologyBatchApplyRun{}, ""); got != 1 {
		t.Fatalf("batch apply runs = %d, want 1 after duplicate applies", got)
	}
}

func TestApplyConflictBatchConcurrentAppliesSucceedOnce(t *testing.T) {
	svc, store := newBatchTestService(t)
	_, items := createConflictBatch(t, svc, "batch-race-detect")
	reviewer := testActor(640, constants.RoleReviewer, "batch-race-review")
	concludeBatch(t, svc, items, reviewer)
	request := dto.ApplyConflictBatchRequest{DetectionRunID: items[0].DetectionRunID}

	const attempts = 8
	errs := make([]error, attempts)
	var wg sync.WaitGroup
	for index := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, err := svc.ApplyConflictBatch(request, fmt.Sprintf("batch-race-key-%d", index), reviewer)
			errs[index] = err
		}()
	}
	wg.Wait()

	succeeded := 0
	for _, err := range errs {
		if err == nil {
			succeeded++
			continue
		}
		var appErr *AppError
		if !errors.As(err, &appErr) || appErr.Code != CodeConflict || appErr.Status != 409 {
			t.Fatalf("losing apply error = %v, want 409 %s", err, CodeConflict)
		}
	}
	if succeeded != 1 {
		t.Fatalf("concurrent applies succeeded %d times, want exactly 1", succeeded)
	}
	if got := countRows(t, store, &model.BoundaryProposal{}, ""); got != 2 {
		t.Fatalf("proposal count = %d, want exactly one derived draft", got)
	}
	if got := countRows(t, store, &model.TopologyBatchApplyRun{}, ""); got != 1 {
		t.Fatalf("batch apply runs = %d, want 1", got)
	}
	if got := countRows(t, store, &model.AuditLog{}, "action = ?", "conflict.batch_applied"); got != 1 {
		t.Fatalf("batch_applied audit entries = %d, want 1", got)
	}
	states := conflictStates(t, svc, items)
	for id, state := range states {
		want := constants.ConflictResolved
		if id == items[len(items)-1].ID {
			want = constants.ConflictFalsePositive
		}
		if state != want {
			t.Fatalf("conflict %d state = %s, want %s after the race", id, state, want)
		}
	}
}

func TestApplyConflictBatchRollsBackWhenAnyDispositionFails(t *testing.T) {
	svc, store := newBatchTestService(t)
	_, items := createConflictBatch(t, svc, "batch-rollback-detect")
	reviewer := testActor(650, constants.RoleReviewer, "batch-rollback-review")
	concludeBatch(t, svc, items, reviewer)
	auditsBefore := countRows(t, store, &model.AuditLog{}, "")

	// An empty request id makes the first audit insert fail after the draft and
	// transitions were written, forcing a mid-batch rollback.
	faulty := testActor(650, constants.RoleReviewer, "")
	if _, _, err := svc.ApplyConflictBatch(dto.ApplyConflictBatchRequest{DetectionRunID: items[0].DetectionRunID}, "batch-rollback-key", faulty); err == nil {
		t.Fatal("ApplyConflictBatch() with a failing audit succeeded, want an error")
	}
	if got := countRows(t, store, &model.BoundaryProposal{}, ""); got != 1 {
		t.Fatalf("proposal count = %d, want the derived draft rolled back", got)
	}
	if got := countRows(t, store, &model.TopologyBatchApplyRun{}, ""); got != 0 {
		t.Fatalf("batch apply runs = %d, want the idempotency record rolled back", got)
	}
	if got := countRows(t, store, &model.AuditLog{}, ""); got != auditsBefore {
		t.Fatalf("audit entries = %d, want the original %d after rollback", got, auditsBefore)
	}
	states := conflictStates(t, svc, items)
	for index, item := range items {
		want := constants.ConflictConfirmed
		switch index {
		case 0:
			want = constants.ConflictResolutionProposed
		case len(items) - 1:
			want = constants.ConflictFalsePositive
		}
		if states[item.ID] != want {
			t.Fatalf("conflict %d state = %s, want %s preserved after rollback", item.ID, states[item.ID], want)
		}
	}

	// The batch is not wedged: a valid retry applies cleanly afterwards.
	derived, _, err := svc.ApplyConflictBatch(dto.ApplyConflictBatchRequest{DetectionRunID: items[0].DetectionRunID}, "batch-rollback-key", reviewer)
	if err != nil {
		t.Fatalf("retry ApplyConflictBatch() error = %v", err)
	}
	if derived.ProposalState != constants.ProposalDraft {
		t.Fatalf("retried derived proposal = %#v, want a draft", derived)
	}
}

func TestApplyConflictBatchRejectsEmptyDetectionRun(t *testing.T) {
	svc, _ := newBatchTestService(t)
	surveyor := testActor(660, constants.RoleSurveyor, "batch-empty-parcel")
	base := createTestParcel(t, svc, "P-BATCH-LONE", serviceTestPolygon(`[0,0],[10,0],[10,10],[0,10],[0,0]`), surveyor)
	proposal := createTestProposal(t, svc, base, serviceTestPolygon(`[0,0],[10,0],[10,10],[0,10],[0,0]`), surveyor)
	detectActor := testActor(661, constants.RoleGISAnalyst, "batch-empty-detect")
	items, err := svc.DetectConflicts(dto.DetectConflictRequest{ProposalID: proposal.ID, SnapToleranceM: 0.1}, "batch-empty-detect-key", detectActor)
	if err != nil {
		t.Fatalf("DetectConflicts() error = %v", err)
	}
	if len(items) != 0 {
		t.Fatalf("DetectConflicts() = %d conflicts, want none for an identical boundary", len(items))
	}
	run, err := svc.store.DetectionRuns.GetByActorKey(detectActor.ID, "batch-empty-detect-key")
	if err != nil {
		t.Fatalf("load empty detection run: %v", err)
	}
	_, _, err = svc.ApplyConflictBatch(dto.ApplyConflictBatchRequest{DetectionRunID: run.ID}, "batch-empty-apply", testActor(662, constants.RoleReviewer, "batch-empty-review"))
	var appErr *AppError
	if !errors.As(err, &appErr) || appErr.Code != CodeConflict || appErr.Status != 409 {
		t.Fatalf("empty batch apply error = %v, want 409 %s", err, CodeConflict)
	}
}
