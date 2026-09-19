package router

import (
	"cadastral-boundary-topology-resolution/backend/internal/constants"
	appmw "cadastral-boundary-topology-resolution/backend/internal/middleware"
	"github.com/gin-gonic/gin"
)

func registerTopologyConflictRoutes(api *gin.RouterGroup, deps Dependencies) {
	h := deps.CadastralHandler
	conflicts := api.Group("/conflicts")
	conflicts.GET("", h.ListConflicts)
	conflicts.GET("/:id", h.GetConflict)
	conflicts.POST("/detect", appmw.RateLimitMiddleware(deps.AnalyzeLimiter, "conflict_detection"), appmw.RBACMiddleware(constants.RoleGISAnalyst, constants.RoleAdmin), h.DetectConflicts)
	conflicts.POST("/:id/transition", appmw.RBACMiddleware(constants.RoleReviewer, constants.RoleAdmin), h.TransitionConflict)
	conflicts.POST("/:id/apply-suggestion", appmw.RBACMiddleware(constants.RoleReviewer, constants.RoleAdmin), h.ApplyConflictSuggestion)

	// Unified batch review: conclusions are saved per batch, then applied once
	// atomically with an Idempotency-Key.
	reviewBatches := api.Group("/conflict-review-batches")
	reviewBatches.GET("", appmw.RBACMiddleware(constants.RoleReviewer, constants.RoleGISAnalyst, constants.RoleAdmin), h.GetReviewBatch)
	reviewBatches.GET("/:id", appmw.RBACMiddleware(constants.RoleReviewer, constants.RoleGISAnalyst, constants.RoleAdmin), h.GetReviewBatch)
	reviewBatches.POST("/:id/dispositions", appmw.RateLimitMiddleware(deps.AnalyzeLimiter, "conflict_batch_review"), appmw.RBACMiddleware(constants.RoleReviewer, constants.RoleAdmin), h.SaveBatchDispositions)
	reviewBatches.POST("/:id/apply", appmw.RateLimitMiddleware(deps.AnalyzeLimiter, "conflict_batch_apply"), appmw.RBACMiddleware(constants.RoleReviewer, constants.RoleAdmin), h.ApplyReviewBatch)
}
