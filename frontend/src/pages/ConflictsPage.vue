<script setup lang="ts">
import { computed, onMounted, reactive, ref } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import { Check, FilePlus, Layers, RefreshCw, ScanSearch, X } from 'lucide-vue-next'
import PageHeader from '@/components/common/PageHeader.vue'
import TopologyLegend from '@/components/common/TopologyLegend.vue'
import GeometryEvidenceDrawer from '@/components/common/GeometryEvidenceDrawer.vue'
import ProposalStateBadge from '@/components/common/ProposalStateBadge.vue'
import { useTopologyConflictStore } from '@/stores/topology-conflict'
import { useBoundaryProposalStore } from '@/stores/boundary-proposal'
import { useLandParcelStore } from '@/stores/land-parcel'
import { useAuth } from '@/hooks/useAuth'
import { conflictTypeLabel } from '@/types/enums/conflict-type'
import type { ProposalState } from '@/types/enums/proposal-state'
import type { TopologyConflict } from '@/types/topology-conflict'

const conflicts = useTopologyConflictStore()
const proposals = useBoundaryProposalStore()
const parcels = useLandParcelStore()
const auth = useAuth()
const detectOpen = ref(false)
const evidenceOpen = ref(false)
const selected = ref<TopologyConflict | null>(null)
const applyingRunId = ref(0)
const form = reactive({ proposal_id: 0 })

interface ConflictBatch {
  runId: number
  proposalId: number
  items: TopologyConflict[]
}

const CONCLUDED_STATES = ['confirmed', 'false_positive', 'resolution_proposed']

// One detection run is disposed as a unit: the batch bar tracks every run that
// still has open conflicts so the reviewer can conclude and apply it atomically.
const batches = computed<ConflictBatch[]>(() => {
  const groups = new Map<number, TopologyConflict[]>()
  for (const item of conflicts.items) {
    if (!item.detection_run_id) continue
    const list = groups.get(item.detection_run_id) ?? []
    list.push(item)
    groups.set(item.detection_run_id, list)
  }
  return [...groups.entries()]
    .map(([runId, items]) => ({
      runId,
      proposalId: items[0]?.proposal_id ?? 0,
      items: [...items].sort((left, right) => left.id - right.id),
    }))
    .filter((group) => group.items.length > 1 && group.items.some((item) => !['resolved', 'closed'].includes(item.conflict_state)))
    .sort((left, right) => right.runId - left.runId)
})

function batchSizeOf(item: TopologyConflict) {
  if (!item.detection_run_id) return 1
  return conflicts.items.filter((current) => current.detection_run_id === item.detection_run_id).length
}

function batchCount(batch: ConflictBatch, state: string) {
  return batch.items.filter((item) => item.conflict_state === state).length
}

function batchReady(batch: ConflictBatch) {
  return (
    batch.items.every((item) => CONCLUDED_STATES.includes(item.conflict_state)) &&
    batch.items.some((item) => item.conflict_state === 'resolution_proposed')
  )
}

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

async function applyBatch(batch: ConflictBatch) {
  const resolved = batch.items.filter((item) => item.conflict_state !== 'false_positive').length
  try {
    await ElMessageBox.confirm(
      `将基于批次 #${batch.runId} 的吸附证据一次创建 1 个新草稿提案，并把本批 ${resolved} 条已确认/已建议冲突一并转为已解决；误报保持原状。任一处置失败则全部回滚，重复提交只生效一次。`,
      '应用整批建议',
      { confirmButtonText: '创建草稿并解决整批', cancelButtonText: '取消', type: 'warning' },
    )
  } catch (reason) {
    if (reason === 'cancel' || reason === 'close') return
    throw reason
  }
  applyingRunId.value = batch.runId
  try {
    const result = await conflicts.applyBatch({ detection_run_id: batch.runId })
    ElMessage.success(`已创建草稿提案 #${result.proposal.id}，批次 #${batch.runId} 的 ${resolved} 条冲突已解决`)
  } finally {
    applyingRunId.value = 0
  }
  await load()
}

function showEvidence(item: TopologyConflict) {
  selected.value = item
  evidenceOpen.value = true
}

onMounted(load)
</script>

<template>
  <PageHeader title="冲突消解" eyebrow="TOPOLOGY CONFLICTS" description="查看重叠、缝隙和无效拓扑证据，所有建议都需要人工确认。">
    <el-button v-if="auth.hasRole('gis_analyst', 'admin')" type="primary" @click="detectOpen = true"><ScanSearch :size="15" />运行检测</el-button>
  </PageHeader>

  <section class="content-band">
    <div class="toolbar"><el-button @click="load"><RefreshCw :size="15" />刷新</el-button><TopologyLegend /><span class="toolbar-spacer subtle-count">{{ conflicts.items.length }} 条冲突</span></div>
    <div v-if="batches.length" class="batch-strip">
      <div v-for="batch in batches" :key="batch.runId" class="batch-row">
        <span class="batch-title"><Layers :size="14" /><strong>检测批次 #{{ batch.runId }}</strong><small class="muted">提案 #{{ batch.proposalId }} · {{ batch.items.length }} 条冲突</small></span>
        <span class="batch-counts">
          <span v-if="batchCount(batch, 'detected')" class="batch-chip tone-pending">待处置 {{ batchCount(batch, 'detected') }}</span>
          <span v-if="batchCount(batch, 'confirmed')" class="batch-chip tone-progress">已确认 {{ batchCount(batch, 'confirmed') }}</span>
          <span v-if="batchCount(batch, 'false_positive')" class="batch-chip tone-muted">误报 {{ batchCount(batch, 'false_positive') }}</span>
          <span v-if="batchCount(batch, 'resolution_proposed')" class="batch-chip tone-ready">已建议 {{ batchCount(batch, 'resolution_proposed') }}</span>
        </span>
        <span class="batch-spacer" />
        <span v-if="!batchReady(batch)" class="batch-hint">每条需确认或标记误报，且至少一条准备吸附建议后才能整批应用</span>
        <el-button v-if="auth.hasRole('reviewer', 'admin')" type="primary" size="small" :disabled="!batchReady(batch)" :loading="applyingRunId === batch.runId" @click="applyBatch(batch)"><FilePlus :size="14" />应用整批建议</el-button>
      </div>
    </div>
    <div class="data-surface">
      <el-table v-loading="conflicts.loading" :data="conflicts.items" row-key="id">
        <el-table-column label="冲突" width="110"><template #default="scope"><strong>#{{ scope.row.id }}</strong><small v-if="scope.row.detection_run_id" class="muted batch-ref">批次 #{{ scope.row.detection_run_id }}</small></template></el-table-column>
        <el-table-column label="参与地块" min-width="180"><template #default="scope"><strong>{{ parcelLabelFor(scope.row) }}</strong><small class="muted">提案 #{{ scope.row.proposal_id }}</small><ProposalStateBadge :state="proposalStateFor(scope.row)" /></template></el-table-column>
        <el-table-column label="类型" width="125"><template #default="scope"><span :class="['conflict-tag', `tone-${scope.row.conflict_type}`]">{{ typeLabel(scope.row.conflict_type) }}</span></template></el-table-column>
        <el-table-column prop="severity" label="严重度" width="95" />
        <el-table-column label="量级" width="125"><template #default="scope">{{ scope.row.magnitude_square_m.toFixed(2) }} m²</template></el-table-column>
        <el-table-column prop="explanation" label="说明" min-width="220" show-overflow-tooltip />
        <el-table-column label="状态" width="145"><template #default="scope"><span class="status-pill" :class="scope.row.conflict_state">{{ scope.row.conflict_state }}</span></template></el-table-column>
        <el-table-column label="动作" width="270"><template #default="scope"><div class="conflict-actions"><el-button text @click="showEvidence(scope.row)">证据</el-button><template v-if="auth.hasRole('reviewer', 'admin')"><el-button v-if="scope.row.conflict_state === 'detected'" text type="primary" @click="transition(scope.row, 'confirmed')"><Check :size="14" />确认</el-button><el-button v-if="scope.row.conflict_state === 'detected'" text type="warning" @click="transition(scope.row, 'false_positive')"><X :size="14" />误报</el-button><el-button v-if="scope.row.conflict_state === 'confirmed'" text type="primary" @click="transition(scope.row, 'resolution_proposed')"><FilePlus :size="14" />准备建议</el-button><el-button v-if="scope.row.conflict_state === 'resolution_proposed' && batchSizeOf(scope.row) < 2" text type="primary" @click="applySuggestion(scope.row)"><FilePlus :size="14" />应用建议</el-button><el-button v-if="scope.row.conflict_state === 'false_positive' || scope.row.conflict_state === 'resolved'" text @click="transition(scope.row, 'closed')"><X :size="14" />关闭</el-button></template></div></template></el-table-column>
      </el-table>
      <div v-if="!conflicts.loading && !conflicts.items.length" class="empty-state"><div><strong>暂无冲突</strong><span>选择一个提案运行检测，系统会记录算法版本和输入哈希。</span></div></div>
    </div>
  </section>

  <el-dialog v-model="detectOpen" title="检测提案拓扑" width="min(480px, calc(100vw - 28px))">
    <el-form label-position="top"><el-form-item label="边界提案"><el-select v-model="form.proposal_id" placeholder="选择提案" style="width: 100%"><el-option v-for="item in proposals.items" :key="item.id" :label="`提案 #${item.id} · ${item.proposal_state}`" :value="item.id" /></el-select></el-form-item><el-alert type="warning" :closable="false" title="检测是离线决策支持，不会修改原始地块边界或法定登记。" /></el-form>
    <template #footer><el-button @click="detectOpen = false">取消</el-button><el-button type="primary" :disabled="!form.proposal_id" @click="detect">开始检测</el-button></template>
  </el-dialog>

  <GeometryEvidenceDrawer v-model="evidenceOpen" title="冲突证据" :geometry="selected?.geometry_geojson" :explanation="selected?.explanation" />
</template>

<style scoped>
.conflict-tag { display: inline-flex; padding: 3px 8px; border: 1px solid var(--line-strong); font-size: 12px; font-weight: 800; }
.tone-overlap { color: #9c3028; background: #fbeceb; border-color: #e6afaa; }.tone-gap { color: #755310; background: #fff5db; border-color: #e0c16b; }.tone-self_intersection { color: #7b4c9e; background: #f4ecfa; }.tone-dangling_edge { color: #2b6f96; background: #e8f2f7; }
.muted { display: block; margin-top: 4px; color: var(--text-muted); font-size: 11px; }
.conflict-actions { display: flex; flex-wrap: wrap; gap: 2px; }
.status-pill.confirmed, .status-pill.resolution_proposed { color: #755310; background: #fff5db; }.status-pill.resolved { color: #17604e; background: #e8f4f0; }.status-pill.false_positive, .status-pill.closed { color: #4b5551; background: #e8ecea; }
.batch-strip { display: flex; flex-direction: column; gap: 8px; margin-bottom: 12px; }
.batch-row { display: flex; flex-wrap: wrap; align-items: center; gap: 10px; padding: 8px 12px; border: 1px solid var(--line-strong); background: var(--surface-strong); }
.batch-title { display: inline-flex; align-items: center; gap: 6px; }
.batch-title .muted { display: inline; margin-top: 0; }
.batch-counts { display: inline-flex; flex-wrap: wrap; gap: 6px; }
.batch-chip { padding: 2px 8px; border: 1px solid var(--line-strong); font-size: 11px; font-weight: 700; }
.batch-chip.tone-pending { color: #9c3028; background: #fbeceb; border-color: #e6afaa; }
.batch-chip.tone-progress { color: #755310; background: #fff5db; border-color: #e0c16b; }
.batch-chip.tone-muted { color: #4b5551; background: #e8ecea; }
.batch-chip.tone-ready { color: #17604e; background: #e8f4f0; }
.batch-spacer { flex: 1; }
.batch-hint { color: var(--text-muted); font-size: 12px; }
.batch-ref { display: block; margin-top: 2px; }
</style>
