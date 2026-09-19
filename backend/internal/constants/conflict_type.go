package constants

// ConflictType is the stable vocabulary shared by geometry services and clients.
type ConflictType string

const (
	ConflictOverlap          ConflictType = "overlap"
	ConflictGap              ConflictType = "gap"
	ConflictSelfIntersection ConflictType = "self_intersection"
	ConflictDanglingEdge     ConflictType = "dangling_edge"
)

const (
	ConflictDetected           = "detected"
	ConflictConfirmed          = "confirmed"
	ConflictFalsePositive      = "false_positive"
	ConflictResolutionProposed = "resolution_proposed"
	ConflictResolved           = "resolved"
	ConflictClosed             = "closed"
)

var conflictTransitions = map[string]map[string]bool{
	ConflictDetected:           {ConflictConfirmed: true, ConflictFalsePositive: true},
	ConflictConfirmed:          {ConflictResolutionProposed: true},
	ConflictFalsePositive:      {ConflictClosed: true},
	ConflictResolutionProposed: {ConflictResolved: true},
	ConflictResolved:           {ConflictClosed: true},
	ConflictClosed:             {},
}

func CanConflictTransition(from, to string) bool { return conflictTransitions[from][to] }

// A review batch gathers every conflict produced by one detection run so a
// reviewer disposes of the whole result set together.
const (
	BatchOpen        = "open"
	BatchApplied     = "applied"
	BatchStateClosed = "closed"
)

// DispositionDecision is the reviewer conclusion recorded for one conflict.
const (
	DispositionConfirmed          = "confirmed"
	DispositionFalsePositive      = "false_positive"
	DispositionResolutionProposed = "resolution_proposed"
)

// CanBatchTransition governs the lifecycle of a unified review batch. Once a
// batch has been applied or closed its conclusion set is immutable.
var batchTransitions = map[string]map[string]bool{
	BatchOpen:        {BatchApplied: true, BatchStateClosed: true},
	BatchApplied:     {},
	BatchStateClosed: {},
}

func CanBatchTransition(from, to string) bool { return batchTransitions[from][to] }

// IsBatchConclusion reports whether a reviewer disposition is a final
// per-conflict conclusion that survives a refresh of the review page.
func IsBatchConclusion(decision string) bool {
	return decision == DispositionConfirmed || decision == DispositionFalsePositive || decision == DispositionResolutionProposed
}
