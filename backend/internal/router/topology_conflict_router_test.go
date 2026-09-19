package router

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"cadastral-boundary-topology-resolution/backend/internal/constants"
	"cadastral-boundary-topology-resolution/backend/internal/dto"
	"cadastral-boundary-topology-resolution/backend/internal/model"
	"cadastral-boundary-topology-resolution/backend/internal/service"
)

func loginToken(t *testing.T, authService *service.AuthService, username string) string {
	t.Helper()
	login, err := authService.Login(dto.LoginRequest{Username: username, Password: "DemoPass123!"})
	if err != nil {
		t.Fatalf("login %s: %v", username, err)
	}
	return login.Token
}

func prepareConflictBatch(t *testing.T, cadastralService *service.CadastralService) uint {
	t.Helper()
	surveyor := service.Actor{ID: 801, Username: "surveyor-fixture", Role: constants.RoleSurveyor, RequestID: "batch-router-parcel"}
	polygon := func(points string) string {
		return `{"type":"Polygon","coordinates":[[` + points + `]]}`
	}
	var base model.LandParcel
	for index, fixture := range []struct{ code, boundary string }{
		{"P-ROUTE-BASE", `[0,0],[10,0],[10,10],[0,10],[0,0]`},
		{"P-ROUTE-EAST", `[10,0],[20,0],[20,10],[10,10],[10,0]`},
		{"P-ROUTE-NORTH", `[0,10],[10,10],[10,20],[0,20],[0,10]`},
	} {
		parcel, err := cadastralService.CreateParcel(dto.CreateParcelRequest{ParcelCode: fixture.code, Name: fixture.code, BoundaryGeoJSON: polygon(fixture.boundary), CoordinateSystem: "EPSG:3857", OwnerOrg: "router fixture"}, surveyor)
		if err != nil {
			t.Fatalf("create parcel %s: %v", fixture.code, err)
		}
		if index == 0 {
			base = parcel
		}
	}
	proposal, err := cadastralService.CreateProposal(dto.CreateProposalRequest{
		ParcelID: base.ID, BaseVersion: base.BoundaryVersion, ProposedGeoJSON: polygon(`[-0.5,0],[11,0],[11,11],[-0.5,11],[-0.5,0]`),
		SnapToleranceM: 0.1, Rationale: "router batch fixture",
	}, surveyor)
	if err != nil {
		t.Fatalf("create proposal: %v", err)
	}
	analyst := service.Actor{ID: 802, Username: "analyst-fixture", Role: constants.RoleGISAnalyst, RequestID: "batch-router-detect"}
	items, err := cadastralService.DetectConflicts(dto.DetectConflictRequest{ProposalID: proposal.ID, SnapToleranceM: 0.1}, "batch-router-detect-key", analyst)
	if err != nil {
		t.Fatalf("detect conflicts: %v", err)
	}
	if len(items) < 2 {
		t.Fatalf("detection produced %d conflicts, want a multi-conflict batch", len(items))
	}
	reviewer := service.Actor{ID: 803, Username: "reviewer-fixture", Role: constants.RoleReviewer, RequestID: "batch-router-review"}
	for index, item := range items {
		if _, err := cadastralService.TransitionConflict(item.ID, dto.ConflictTransitionRequest{To: constants.ConflictConfirmed}, reviewer); err != nil {
			t.Fatalf("confirm conflict %d: %v", item.ID, err)
		}
		if index == 0 {
			if _, err := cadastralService.TransitionConflict(item.ID, dto.ConflictTransitionRequest{To: constants.ConflictResolutionProposed}, reviewer); err != nil {
				t.Fatalf("propose resolution on conflict %d: %v", item.ID, err)
			}
		}
	}
	return items[0].DetectionRunID
}

func TestApplyConflictBatchEndpointAppliesOnce(t *testing.T) {
	engine, cadastralService, authService := newAuditTestRouter(t)
	runID := prepareConflictBatch(t, cadastralService)
	reviewerToken := loginToken(t, authService, "reviewer")
	surveyorToken := loginToken(t, authService, "surveyor")

	body := []byte(fmt.Sprintf(`{"detection_run_id":%d,"rationale":"整批吸附消解"}`, runID))
	call := func(token, key string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/conflicts/apply-batch", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		if key != "" {
			req.Header.Set("Idempotency-Key", key)
		}
		res := httptest.NewRecorder()
		engine.ServeHTTP(res, req)
		return res
	}

	if res := call(surveyorToken, "batch-router-key"); res.Code != http.StatusForbidden {
		t.Fatalf("surveyor apply-batch status = %d, want 403", res.Code)
	}
	if res := call(reviewerToken, ""); res.Code != http.StatusBadRequest {
		t.Fatalf("apply-batch without Idempotency-Key status = %d, want 400", res.Code)
	}

	res := call(reviewerToken, "batch-router-key")
	if res.Code != http.StatusCreated {
		t.Fatalf("apply-batch status = %d, body = %s", res.Code, res.Body.String())
	}
	var envelope struct {
		Data struct {
			Proposal  model.BoundaryProposal   `json:"proposal"`
			Conflicts []model.TopologyConflict `json:"conflicts"`
		} `json:"data"`
	}
	if err := json.Unmarshal(res.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode apply-batch response: %v", err)
	}
	if envelope.Data.Proposal.ProposalState != constants.ProposalDraft {
		t.Fatalf("applied proposal = %#v, want a new draft", envelope.Data.Proposal)
	}
	for _, item := range envelope.Data.Conflicts {
		if item.ConflictState != constants.ConflictResolved {
			t.Fatalf("conflict %d state = %s, want resolved", item.ID, item.ConflictState)
		}
	}

	replayed := call(reviewerToken, "batch-router-key")
	if replayed.Code != http.StatusCreated {
		t.Fatalf("replayed apply-batch status = %d, body = %s", replayed.Code, replayed.Body.String())
	}
	var replayEnvelope struct {
		Data struct {
			Proposal model.BoundaryProposal `json:"proposal"`
		} `json:"data"`
	}
	if err := json.Unmarshal(replayed.Body.Bytes(), &replayEnvelope); err != nil {
		t.Fatalf("decode replayed response: %v", err)
	}
	if replayEnvelope.Data.Proposal.ID != envelope.Data.Proposal.ID {
		t.Fatalf("replayed proposal id = %d, want %d", replayEnvelope.Data.Proposal.ID, envelope.Data.Proposal.ID)
	}
	if res := call(reviewerToken, "batch-router-key-second"); res.Code != http.StatusConflict {
		t.Fatalf("second apply-batch with a new key status = %d, want 409", res.Code)
	}
}
