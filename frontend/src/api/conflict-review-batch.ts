import { api } from './client'
import type { ApiEnvelope } from '@/types/api'
import type { BatchApplyResult, BatchDispositionInput, ReviewBatchView } from '@/types/conflict-review-batch'

function newIdempotencyKey() {
  return crypto.randomUUID()
}

export const conflictReviewBatchApi = {
  async getByProposal(proposalID: number) {
    const response = await api.get<ApiEnvelope<ReviewBatchView>>('/conflict-review-batches', {
      params: { proposal_id: proposalID },
    })
    return response.data.data
  },
  async get(id: number) {
    const response = await api.get<ApiEnvelope<ReviewBatchView>>(`/conflict-review-batches/${id}`)
    return response.data.data
  },
  async saveDispositions(id: number, dispositions: BatchDispositionInput[]) {
    const response = await api.post<ApiEnvelope<ReviewBatchView>>(`/conflict-review-batches/${id}/dispositions`, { dispositions })
    return response.data.data
  },
  async apply(id: number, body: { rationale?: string }, idempotencyKey: string = newIdempotencyKey()) {
    const response = await api.post<ApiEnvelope<BatchApplyResult>>(`/conflict-review-batches/${id}/apply`, body, {
      headers: { 'Idempotency-Key': idempotencyKey },
    })
    return response.data.data
  },
}
