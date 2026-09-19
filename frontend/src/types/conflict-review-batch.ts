export type ConflictDispositionDecision = 'confirmed' | 'false_positive' | 'resolution_proposed'

export interface ConflictReviewBatch {
  id: number
  detection_run_id: number
  proposal_id: number
  reviewer_id?: number | null
  batch_state: 'open' | 'applied' | 'closed'
  conflict_ids: number[] | string
  result_proposal_id?: number | null
  applied_at?: string | null
  created_at: string
  updated_at: string
}

export interface BatchDisposition {
  conflict_id: number
  decision: ConflictDispositionDecision
  review_note: string
  reviewer_id: number
}

export interface BatchConflictSummary {
  id: number
  proposal_id: number
  conflict_type: string
  conflict_state: string
  severity: string
}

export interface ReviewBatchView {
  batch: ConflictReviewBatch
  dispositions: BatchDisposition[]
  conflicts: BatchConflictSummary[]
  ready_to_apply: boolean
  missing_conclusion: number
  has_suggestion: boolean
}

export interface BatchDispositionInput {
  conflict_id: number
  decision: ConflictDispositionDecision
  review_note?: string
}

export interface BatchApplyResult {
  batch: ConflictReviewBatch
  proposal: {
    id: number
    parcel_id: number
    proposal_state: string
    version: number
    rationale: string
  }
  conflicts: BatchConflictSummary[]
  replayed: boolean
}

export function decisionLabel(decision: ConflictDispositionDecision): string {
  switch (decision) {
    case 'confirmed':
      return '确认'
    case 'false_positive':
      return '误报'
    case 'resolution_proposed':
      return '吸附建议'
    default:
      return decision
  }
}
