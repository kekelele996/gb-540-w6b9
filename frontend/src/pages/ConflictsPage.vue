<script setup lang="ts">
import { computed, onMounted, reactive, ref } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import { Check, ClipboardCheck, FilePlus, RefreshCw, ScanSearch, X } from 'lucide-vue-next'
import PageHeader from '@/components/common/PageHeader.vue'
import TopologyLegend from '@/components/common/TopologyLegend.vue'
import GeometryEvidenceDrawer from '@/components/common/GeometryEvidenceDrawer.vue'
import ProposalStateBadge from '@/components/common/ProposalStateBadge.vue'
import { useTopologyConflictStore } from '@/stores/topology-conflict'
import { useBoundaryProposalStore } from '@/stores/boundary-proposal'
import { useLandParcelStore } from '@/stores/land-parcel'
import { useConflictReviewBatchStore } from '@/stores/conflict-review-batch'
import { useAuth } from '@/hooks/useAuth'
import { conflictTypeLabel } from '@/types/enums/conflict-type'
import type { ProposalState } from '@/types/enums/proposal-state'
import type { ConflictDispositionDecision } from '@/types/conflict-review-batch'
import type { TopologyConflict } from '@/types/topology-conflict'

const conflicts = useTopologyConflictStore()
const proposals = useBoundaryProposalStore()
const parcels = useLandParcelStore()
const reviewBatch = useConflictReviewBatchStore()
const auth = useAuth()
const detectOpen = ref(false)
const evidenceOpen = ref(false)
const batchOpen = ref(false)
const batchBusyProposal = ref(false)
const selected = ref<TopologyConflict | null>(null)
const form = reactive({ proposal_id: 0 })
const batchForm = reactive({ proposal_id: 0, rationale: '' })

const decisionOptions: Array<{ value: ConflictDispositionDecision; label: string }> = [
  { value: 'confirmed', label: '确认' },
  { value: 'false_positive', label: '误报' },
  { value: 'resolution_proposed', label: '吸附建议' },
]

const batchableProposals = computed(() => {
  const ids = new Set(conflicts.items.map((item) => item.proposal_id))
  return proposals.items.filter((proposal) => ids.has(proposal.id))
})

const batchConflictDetails = computed(() => {
  if (!reviewBatch.view) return [] as TopologyConflict[]
  const ids = new Set(reviewBatch.view.conflicts.map((item) => item.id))
  return conflicts.items.filter((item) => ids.has(item.id)).sort((a, b) => a.id - b.id)
})

const draftCount = computed(() => reviewBatch.draftList.length)
const hasSnapDraft = computed(() => reviewBatch.draftList.some((item) => item.decision === 'resolution_proposed'))

function typeLabel(value: unknown) {
  return conflictTypeLabel[value as keyof typeof conflictTypeLabel] ?? String(value)
}

function proposalStateFor(conflict: TopologyConflict): ProposalState | null {
  return proposals.items.find((proposal) => proposal.id === conflict.proposal_id)?.proposal_state ?? null
}

function parcelLabelFor(conflict: TopologyConflict) {
  const labels = conflict.parcel_ids.map((id) => {
    const parcel = parcels.items.find((item) => item.id === id)
    return parcel ? parcel.parcel_code : `地块 #${id}`
  })
  return labels.length ? labels.join(' · ') : '未记录参与地块'
}

async function load() {
  await Promise.all([
    parcels.fetch({ page_size: 100 }),
    proposals.fetch({ page_size: 100 }),
    conflicts.fetch({ page_size: 100 }),
  ])
}

async function detect() {
  await conflicts.detect({ proposal_id: form.proposal_id })
  detectOpen.value = false
  await load()
}

async function transition(item: TopologyConflict, to: string) {
  await conflicts.transition(item.id, { to })
  await load()
}

async function applySuggestion(item: TopologyConflict) {
  try {
    await ElMessageBox.confirm(
      `将基于冲突 #${item.id} 的吸附证据创建新的草稿提案；原始提案和地块边界不会被改写。`,
      '应用建议',
      { confirmButtonText: '创建草稿', cancelButtonText: '取消', type: 'warning' },
    )
  } catch (reason) {
    if (reason === 'cancel' || reason === 'close') return
    throw reason
  }
  await conflicts.applySuggestion(item.id)
  await load()
}

function showEvidence(item: TopologyConflict) {
  selected.value = item
  evidenceOpen.value = true
}

async function openBatchReview() {
  if (!batchForm.proposal_id) return
  batchBusyProposal.value = true
  try {
    const opened = await reviewBatch.openForProposal(batchForm.proposal_id)
    if (!opened) {
      ElMessage.warning('该提案暂无可整批复核的检测结果，请先运行检测。')
      return
    }
    batchOpen.value = true
  } finally {
    batchBusyProposal.value = false
  }
}

async function openBatchLauncher() {
  batchForm.rationale = ''
  reviewBatch.reset()
  batchForm.proposal_id = batchableProposals.value[0]?.id ?? 0
  batchOpen.value = true
  if (batchForm.proposal_id) {
    await openBatchReview()
  }
}

async function saveBatchConclusions() {
  const next = await reviewBatch.saveDispositions()
  if (next) {
    ElMessage.success(`已保存 ${next.dispositions.length} 条复核结论`)
    await load()
  }
}

function canApplyBatch(): boolean {
  const view = reviewBatch.view
  if (!view || view.batch.batch_state !== 'open') return false
  return view.ready_to_apply || (view.missing_conclusion === 0 && hasSnapDraft.value)
}

async function applyBatch() {
  if (draftCount.value === 0) {
    ElMessage.warning('请先为冲突填写复核结论。')
    return
  }
  if (!hasSnapDraft.value) {
    ElMessage.warning('至少要为一条冲突提出吸附建议。')
    return
  }
  if (reviewBatch.view && reviewBatch.view.missing_conclusion > 0) {
    ElMessage.warning(`还有 ${reviewBatch.view.missing_conclusion} 条冲突没有结论，请先保存全部结论。`)
    return
  }
  try {
    await ElMessageBox.confirm(
      '应用后将创建一个新的草稿提案，同批其余已确认冲突一起转为已 resolved；重复或并发应用只会成功一次。',
      '应用整批复核',
      { confirmButtonText: '创建草稿并解决冲突', cancelButtonText: '取消', type: 'warning' },
    )
  } catch (reason) {
    if (reason === 'cancel' || reason === 'close') return
    throw reason
  }
  const result = await reviewBatch.apply(batchForm.rationale)
  if (result) {
    batchForm.rationale = ''
    ElMessage.success(`已创建草稿提案 #${result.batch.result_proposal_id ?? ''}，同批冲突已统一处置。`)
    batchOpen.value = false
    await load()
  }
}

function batchStateLabel(state: string) {
  return state === 'applied' ? '已应用' : state === 'closed' ? '已关闭' : '待处置'
}

onMounted(load)
</script>

<template>
  <PageHeader title="冲突消解" eyebrow="TOPOLOGY CONFLICTS" description="查看重叠、缝隙和无效拓扑证据，所有建议都需要人工确认。">
    <div class="header-actions">
      <el-button v-if="auth.hasRole('reviewer', 'admin')" type="success" plain @click="openBatchLauncher"><ClipboardCheck :size="15" />整批复核</el-button>
      <el-button v-if="auth.hasRole('gis_analyst', 'admin')" type="primary" @click="detectOpen = true"><ScanSearch :size="15" />运行检测</el-button>
    </div>
  </PageHeader>

  <section class="content-band">
    <div class="toolbar"><el-button @click="load"><RefreshCw :size="15" />刷新</el-button><TopologyLegend /><span class="toolbar-spacer subtle-count">{{ conflicts.items.length }} 条冲突</span></div>
    <div class="data-surface">
      <el-table v-loading="conflicts.loading" :data="conflicts.items" row-key="id">
        <el-table-column label="冲突" width="90"><template #default="scope"><strong>#{{ scope.row.id }}</strong></template></el-table-column>
        <el-table-column label="参与地块" min-width="180"><template #default="scope"><strong>{{ parcelLabelFor(scope.row) }}</strong><small class="muted">提案 #{{ scope.row.proposal_id }}</small><ProposalStateBadge :state="proposalStateFor(scope.row)" /></template></el-table-column>
        <el-table-column label="类型" width="125"><template #default="scope"><span :class="['conflict-tag', `tone-${scope.row.conflict_type}`]">{{ typeLabel(scope.row.conflict_type) }}</span></template></el-table-column>
        <el-table-column prop="severity" label="严重度" width="95" />
        <el-table-column label="量级" width="125"><template #default="scope">{{ scope.row.magnitude_square_m.toFixed(2) }} m²</template></el-table-column>
        <el-table-column prop="explanation" label="说明" min-width="220" show-overflow-tooltip />
        <el-table-column label="状态" width="145"><template #default="scope"><span class="status-pill" :class="scope.row.conflict_state">{{ scope.row.conflict_state }}</span></template></el-table-column>
        <el-table-column label="动作" width="270"><template #default="scope"><div class="conflict-actions"><el-button text @click="showEvidence(scope.row)">证据</el-button><template v-if="auth.hasRole('reviewer', 'admin')"><el-button v-if="scope.row.conflict_state === 'detected'" text type="primary" @click="transition(scope.row, 'confirmed')"><Check :size="14" />确认</el-button><el-button v-if="scope.row.conflict_state === 'detected'" text type="warning" @click="transition(scope.row, 'false_positive')"><X :size="14" />误报</el-button><el-button v-if="scope.row.conflict_state === 'confirmed'" text type="primary" @click="transition(scope.row, 'resolution_proposed')"><FilePlus :size="14" />准备建议</el-button><el-button v-if="scope.row.conflict_state === 'resolution_proposed'" text type="primary" @click="applySuggestion(scope.row)"><FilePlus :size="14" />应用建议</el-button><el-button v-if="scope.row.conflict_state === 'false_positive' || scope.row.conflict_state === 'resolved'" text @click="transition(scope.row, 'closed')"><X :size="14" />关闭</el-button></template></div></template></el-table-column>
      </el-table>
      <div v-if="!conflicts.loading && !conflicts.items.length" class="empty-state"><div><strong>暂无冲突</strong><span>选择一个提案运行检测，系统会记录算法版本和输入哈希。</span></div></div>
    </div>
  </section>

  <el-dialog v-model="detectOpen" title="检测提案拓扑" width="min(480px, calc(100vw - 28px))">
    <el-form label-position="top"><el-form-item label="边界提案"><el-select v-model="form.proposal_id" placeholder="选择提案" style="width: 100%"><el-option v-for="item in proposals.items" :key="item.id" :label="`提案 #${item.id} · ${item.proposal_state}`" :value="item.id" /></el-select></el-form-item><el-alert type="warning" :closable="false" title="检测是离线决策支持，不会修改原始地块边界或法定登记。" /></el-form>
    <template #footer><el-button @click="detectOpen = false">取消</el-button><el-button type="primary" :disabled="!form.proposal_id" @click="detect">开始检测</el-button></template>
  </el-dialog>

  <el-dialog
    :model-value="batchOpen"
    title="整批复核：统一处置一次检测的全部冲突"
    width="min(860px, calc(100vw - 28px))"
    @update:model-value="batchOpen = $event"
  >
    <el-form label-position="inline" class="batch-picker">
      <el-form-item label="检测提案">
        <el-select v-model="batchForm.proposal_id" placeholder="选择已有冲突的提案" style="width: 280px" @change="openBatchReview">
          <el-option v-for="item in batchableProposals" :key="item.id" :label="`提案 #${item.id} · ${item.proposal_state}`" :value="item.id" />
        </el-select>
      </el-form-item>
      <el-button :loading="batchBusyProposal" @click="openBatchReview">读取批次</el-button>
      <span v-if="reviewBatch.view" class="batch-meta">
        批次 #{{ reviewBatch.view.batch.id }} · {{ batchStateLabel(reviewBatch.view.batch.batch_state) }}
        <template v-if="reviewBatch.view.batch.result_proposal_id"> · 草稿提案 #{{ reviewBatch.view.batch.result_proposal_id }}</template>
      </span>
    </el-form>

    <el-alert
      v-if="reviewBatch.view"
      :type="canApplyBatch() ? 'success' : 'info'"
      :closable="false"
      show-icon
      :title="`${reviewBatch.view.conflicts.length} 条冲突，已保存结论 ${reviewBatch.view.dispositions.length} 条，待结论 ${reviewBatch.view.missing_conclusion} 条；至少一条须为「吸附建议」。`"
      class="batch-alert"
    />

    <el-table v-loading="reviewBatch.loading" :data="batchConflictDetails" row-key="id" class="batch-table">
      <el-table-column label="冲突" width="80"><template #default="scope">#{{ scope.row.id }}</template></el-table-column>
      <el-table-column label="类型" width="130"><template #default="scope">{{ typeLabel(scope.row.conflict_type) }}</template></el-table-column>
      <el-table-column label="当前状态" width="150"><template #default="scope"><span class="status-pill" :class="scope.row.conflict_state">{{ scope.row.conflict_state }}</span></template></el-table-column>
      <el-table-column label="复核结论（每条必选）" min-width="300">
        <template #default="scope">
          <el-radio-group
            :model-value="reviewBatch.drafts[scope.row.id]?.decision"
            :disabled="reviewBatch.view?.batch.batch_state !== 'open'"
            @update:model-value="(value: ConflictDispositionDecision) => reviewBatch.setDecision(scope.row.id, value)"
          >
            <el-radio-button v-for="option in decisionOptions" :key="option.value" :value="option.value">{{ option.label }}</el-radio-button>
          </el-radio-group>
          <el-input
            :model-value="reviewBatch.drafts[scope.row.id]?.review_note ?? ''"
            class="batch-note"
            size="small"
            maxlength="2000"
            placeholder="复核备注（可选）"
            :disabled="reviewBatch.view?.batch.batch_state !== 'open'"
            @update:model-value="(value: string) => reviewBatch.setNote(scope.row.id, value)"
          />
        </template>
      </el-table-column>
      <el-table-column label="证据" width="80"><template #default="scope"><el-button text @click="showEvidence(scope.row)">查看</el-button></template></el-table-column>
    </el-table>

    <el-form v-if="reviewBatch.view?.batch.batch_state === 'open'" label-position="top" class="batch-rationale">
      <el-form-item label="应用说明（可选）"><el-input v-model="batchForm.rationale" type="textarea" :rows="2" maxlength="2000" show-word-limit placeholder="将写入新草稿提案的 rationale" /></el-form-item>
    </el-form>

    <template #footer>
      <el-button @click="batchOpen = false">关闭</el-button>
      <el-button
        v-if="reviewBatch.view?.batch.batch_state === 'open' && auth.hasRole('reviewer', 'admin')"
        :loading="reviewBatch.saving"
        :disabled="draftCount === 0"
        @click="saveBatchConclusions"
      >保存结论</el-button>
      <el-button
        v-if="reviewBatch.view?.batch.batch_state === 'open' && auth.hasRole('reviewer', 'admin')"
        type="primary"
        :loading="reviewBatch.applying"
        :disabled="!canApplyBatch()"
        @click="applyBatch"
      >应用整批（创建草稿）</el-button>
    </template>
  </el-dialog>

  <GeometryEvidenceDrawer v-model="evidenceOpen" title="冲突证据" :geometry="selected?.geometry_geojson" :explanation="selected?.explanation" />
</template>

<style scoped>
.header-actions { display: flex; gap: 8px; }
.conflict-tag { display: inline-flex; padding: 3px 8px; border: 1px solid var(--line-strong); font-size: 12px; font-weight: 800; }
.tone-overlap { color: #9c3028; background: #fbeceb; border-color: #e6afaa; }.tone-gap { color: #755310; background: #fff5db; border-color: #e0c16b; }.tone-self_intersection { color: #7b4c9e; background: #f4ecfa; }.tone-dangling_edge { color: #2b6f96; background: #e8f2f7; }
.muted { display: block; margin-top: 4px; color: var(--text-muted); font-size: 11px; }
.conflict-actions { display: flex; flex-wrap: wrap; gap: 2px; }
.status-pill.confirmed, .status-pill.resolution_proposed { color: #755310; background: #fff5db; }.status-pill.resolved { color: #17604e; background: #e8f4f0; }.status-pill.false_positive, .status-pill.closed { color: #4b5551; background: #e8ecea; }
.batch-picker { display: flex; align-items: center; gap: 10px; margin-bottom: 10px; }
.batch-meta { color: var(--text-muted); font-size: 13px; }
.batch-alert { margin-bottom: 12px; }
.batch-table { margin-bottom: 12px; }
.batch-note { margin-top: 8px; }
.batch-rationale { margin-top: 4px; }
</style>
