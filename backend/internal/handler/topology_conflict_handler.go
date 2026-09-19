package handler

import (
	"net/http"

	"cadastral-boundary-topology-resolution/backend/internal/dto"
	"github.com/gin-gonic/gin"
)

func (h *CadastralHandler) ListConflicts(c *gin.Context) {
	items, meta, err := h.service.ListConflicts(dto.ConflictQuery{ProposalID: queryUint(c, "proposal_id"), State: c.Query("state"), Type: c.Query("conflict_type"), Page: queryInt(c, "page", 1), PageSize: queryInt(c, "page_size", 50)})
	if err != nil {
		fail(c, err)
		return
	}
	ok(c, http.StatusOK, items, meta)
}

func (h *CadastralHandler) GetConflict(c *gin.Context) {
	id, valid := idParam(c)
	if !valid {
		return
	}
	item, err := h.service.GetConflict(id)
	if err != nil {
		fail(c, err)
		return
	}
	ok(c, http.StatusOK, item, nil)
}

func (h *CadastralHandler) DetectConflicts(c *gin.Context) {
	var req dto.DetectConflictRequest
	if !bind(c, h.validate, &req) {
		return
	}
	items, err := h.service.DetectConflicts(req, c.GetHeader("Idempotency-Key"), actor(c))
	if err != nil {
		fail(c, err)
		return
	}
	ok(c, http.StatusOK, items, nil)
}

func (h *CadastralHandler) TransitionConflict(c *gin.Context) {
	id, valid := idParam(c)
	if !valid {
		return
	}
	var req dto.ConflictTransitionRequest
	if !bind(c, h.validate, &req) {
		return
	}
	item, err := h.service.TransitionConflict(id, req, actor(c))
	if err != nil {
		fail(c, err)
		return
	}
	ok(c, http.StatusOK, item, nil)
}

func (h *CadastralHandler) ApplyConflictSuggestion(c *gin.Context) {
	id, valid := idParam(c)
	if !valid {
		return
	}
	var req dto.ApplySuggestionRequest
	if !bind(c, h.validate, &req) {
		return
	}
	item, err := h.service.ApplyConflictSuggestion(id, req, actor(c))
	if err != nil {
		fail(c, err)
		return
	}
	ok(c, http.StatusCreated, item, nil)
}

// GetReviewBatch reads a unified review batch by proposal (latest detection
// run) or batch id so a refreshed page shows identical conclusions.
func (h *CadastralHandler) GetReviewBatch(c *gin.Context) {
	if proposalID := queryUint(c, "proposal_id"); proposalID != nil {
		view, err := h.service.GetReviewBatchByProposal(*proposalID, actor(c))
		if err != nil {
			fail(c, err)
			return
		}
		ok(c, http.StatusOK, view, nil)
		return
	}
	id, valid := idParam(c)
	if !valid {
		return
	}
	view, err := h.service.GetReviewBatch(id)
	if err != nil {
		fail(c, err)
		return
	}
	ok(c, http.StatusOK, view, nil)
}

func (h *CadastralHandler) SaveBatchDispositions(c *gin.Context) {
	id, valid := idParam(c)
	if !valid {
		return
	}
	var req dto.BatchDispositionRequest
	if !bind(c, h.validate, &req) {
		return
	}
	view, err := h.service.SaveBatchDispositions(id, req, actor(c))
	if err != nil {
		fail(c, err)
		return
	}
	ok(c, http.StatusOK, view, nil)
}

func (h *CadastralHandler) ApplyReviewBatch(c *gin.Context) {
	id, valid := idParam(c)
	if !valid {
		return
	}
	var req dto.BatchApplyRequest
	if !bind(c, h.validate, &req) {
		return
	}
	result, err := h.service.ApplyReviewBatch(id, req, c.GetHeader("Idempotency-Key"), actor(c))
	if err != nil {
		fail(c, err)
		return
	}
	ok(c, http.StatusCreated, result, nil)
}
