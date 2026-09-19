import { defineStore } from 'pinia'
import { computed, ref } from 'vue'
import { conflictReviewBatchApi } from '@/api/conflict-review-batch'
import type {
  BatchDispositionInput,
  ConflictDispositionDecision,
  ReviewBatchView,
} from '@/types/conflict-review-batch'

interface DispositionDraft {
  decision: ConflictDispositionDecision
  review_note: string
}

export const useConflictReviewBatchStore = defineStore('conflict-review-batch', () => {
  const view = ref<ReviewBatchView | null>(null)
  const loading = ref(false)
  const saving = ref(false)
  const applying = ref(false)
  const drafts = ref<Record<number, DispositionDraft>>({})

  const batchId = computed(() => view.value?.batch.id ?? null)
  const draftList = computed<BatchDispositionInput[]>(() =>
    Object.entries(drafts.value)
      .map(([id, draft]) => ({ conflict_id: Number(id), decision: draft.decision, review_note: draft.review_note }))
      .sort((a, b) => a.conflict_id - b.conflict_id),
  )

  function hydrateDrafts(next: ReviewBatchView) {
    const seeded: Record<number, DispositionDraft> = {}
    for (const disposition of next.dispositions) {
      seeded[disposition.conflict_id] = { decision: disposition.decision, review_note: disposition.review_note ?? '' }
    }
    drafts.value = seeded
    view.value = next
  }

  async function openForProposal(proposalID: number): Promise<ReviewBatchView | null> {
    loading.value = true
    try {
      const next = await conflictReviewBatchApi.getByProposal(proposalID)
      hydrateDrafts(next)
      return next
    } catch {
      view.value = null
      drafts.value = {}
      return null
    } finally {
      loading.value = false
    }
  }

  function setDecision(conflictID: number, decision: ConflictDispositionDecision) {
    const existing = drafts.value[conflictID]
    drafts.value[conflictID] = { decision, review_note: existing?.review_note ?? '' }
  }

  function setNote(conflictID: number, note: string) {
    const existing = drafts.value[conflictID]
    drafts.value[conflictID] = { decision: existing?.decision ?? 'confirmed', review_note: note }
  }

  async function saveDispositions(): Promise<ReviewBatchView | null> {
    if (!view.value || draftList.value.length === 0) return view.value
    saving.value = true
    try {
      const next = await conflictReviewBatchApi.saveDispositions(view.value.batch.id, draftList.value)
      hydrateDrafts(next)
      return next
    } finally {
      saving.value = false
    }
  }

  async function apply(rationale = ''): Promise<ReviewBatchView | null> {
    if (!view.value) return null
    applying.value = true
    try {
      const result = await conflictReviewBatchApi.apply(view.value.batch.id, { rationale })
      const next = await conflictReviewBatchApi.get(result.batch.id)
      hydrateDrafts(next)
      return next
    } finally {
      applying.value = false
    }
  }

  function reset() {
    view.value = null
    drafts.value = {}
    loading.value = false
    saving.value = false
    applying.value = false
  }

  return { view, loading, saving, applying, drafts, batchId, draftList, openForProposal, setDecision, setNote, saveDispositions, apply, reset }
})
