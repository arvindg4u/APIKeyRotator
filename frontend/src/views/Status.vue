<template>
  <div class="status-page">
    <div class="header-bar">
      <h1>{{ $t('status.title') }}</h1>
      <div class="header-actions">
        <el-tag :type="connected ? 'success' : 'info'" size="small" effect="dark">
          {{ connected ? $t('status.connected') : $t('status.disconnected') }}
        </el-tag>
        <el-switch v-model="autoRefresh" :active-text="$t('status.refresh')" />
        <el-button size="small" @click="refreshAll">{{ $t('status.refreshNow') }}</el-button>
      </div>
    </div>

    <el-tabs v-model="activeTab">
      <!-- Overview -->
      <el-tab-pane :label="$t('status.overview')" name="overview">
        <el-card v-loading="loading.overview" shadow="never">
          <el-descriptions :column="descColumns" border>
            <el-descriptions-item :label="$t('status.field.status')">
              <el-tag type="success" size="small">{{ overview?.status || '-' }}</el-tag>
            </el-descriptions-item>
            <el-descriptions-item :label="$t('status.field.uptime')">
              {{ formatUptime(overview?.uptime_seconds) }}
            </el-descriptions-item>
            <el-descriptions-item :label="$t('status.field.version')">
              {{ overview?.version || '-' }}
            </el-descriptions-item>
            <el-descriptions-item :label="$t('status.field.goVersion')">
              {{ overview?.go_version || '-' }}
            </el-descriptions-item>
            <el-descriptions-item :label="$t('status.field.cache')">
              {{ overview?.cache_type || '-' }}
            </el-descriptions-item>
            <el-descriptions-item :label="$t('status.field.database')">
              {{ overview?.db_type || '-' }}
            </el-descriptions-item>
            <el-descriptions-item :label="$t('status.field.egress')">
              {{ overview?.egress_enabled ? $t('status.enabled') : $t('status.disabled') }}
            </el-descriptions-item>
            <el-descriptions-item :label="$t('status.field.configs')">
              {{ overview?.active_configs ?? '-' }} / {{ overview?.configs ?? '-' }}
            </el-descriptions-item>
            <el-descriptions-item :label="$t('status.field.keys')">
              {{ overview?.active_keys ?? '-' }} / {{ overview?.keys ?? '-' }}
            </el-descriptions-item>
            <el-descriptions-item :label="$t('status.field.coolingKeys')">
              <el-tag :type="(overview?.cooling_keys || 0) > 0 ? 'warning' : 'info'" size="small">
                {{ overview?.cooling_keys ?? 0 }}
              </el-tag>
            </el-descriptions-item>
            <el-descriptions-item :label="$t('status.field.requests')">
              {{ overview?.total_requests ?? 0 }}
            </el-descriptions-item>
          </el-descriptions>
        </el-card>

        <el-card class="section-card" shadow="never">
          <template #header>{{ $t('status.perConfig.title') }}</template>
          <el-table :data="overview?.per_config || []" size="small" style="width: 100%">
            <el-table-column prop="name" :label="$t('status.perConfig.name')" min-width="140" />
            <el-table-column prop="slug" :label="$t('status.perConfig.slug')" min-width="120" />
            <el-table-column prop="type" :label="$t('status.perConfig.type')" width="90" />
            <el-table-column :label="$t('status.perConfig.active')" width="90">
              <template #default="scope">
                <el-tag :type="scope.row.active ? 'success' : 'info'" size="small">
                  {{ scope.row.active ? $t('status.yes') : $t('status.no') }}
                </el-tag>
              </template>
            </el-table-column>
            <el-table-column prop="keys" :label="$t('status.perConfig.keys')" width="80" />
            <el-table-column prop="active_keys" :label="$t('status.perConfig.activeKeys')" width="90" />
            <el-table-column :label="$t('status.perConfig.coolingKeys')" width="100">
              <template #default="scope">
                <el-tag :type="scope.row.cooling_keys > 0 ? 'warning' : 'info'" size="small">
                  {{ scope.row.cooling_keys }}
                </el-tag>
              </template>
            </el-table-column>
          </el-table>
        </el-card>
      </el-tab-pane>

      <!-- Traffic -->
      <el-tab-pane :label="$t('status.traffic')" name="traffic">
        <el-row :gutter="12" class="stat-row">
          <el-col :xs="12" :sm="8" :md="4">
            <el-card shadow="never"><el-statistic :title="$t('status.traffic.requests')" :value="stats.requests" /></el-card>
          </el-col>
          <el-col :xs="12" :sm="8" :md="5">
            <el-card shadow="never"><el-statistic :title="$t('status.traffic.rateLimited')" :value="stats.rateLimited" /></el-card>
          </el-col>
          <el-col :xs="12" :sm="8" :md="4">
            <el-card shadow="never"><el-statistic :title="$t('status.traffic.clientErrors')" :value="stats.clientErrors" /></el-card>
          </el-col>
          <el-col :xs="12" :sm="8" :md="4">
            <el-card shadow="never"><el-statistic :title="$t('status.traffic.serverErrors')" :value="stats.serverErrors" /></el-card>
          </el-col>
          <el-col :xs="12" :sm="8" :md="5">
            <el-card shadow="never"><el-statistic :title="$t('status.traffic.avgLatency')" :value="stats.avgLatency" suffix="ms" /></el-card>
          </el-col>
        </el-row>

        <el-card class="section-card" shadow="never">
          <template #header>
            <div class="card-header">
              <span>{{ $t('status.traffic.rpm') }}</span>
              <span class="muted">{{ $t('status.traffic.since', { time: sinceLabel }) }}</span>
            </div>
          </template>
          <div class="rpm-chart">
            <div
              v-for="(bar, i) in rpmBars"
              :key="i"
              class="rpm-bar"
              :class="{ 'rpm-bar-last': bar.last }"
              :style="{ height: bar.h + '%' }"
              :title="bar.v"
            ></div>
          </div>
        </el-card>

        <el-card class="section-card" shadow="never">
          <template #header>{{ $t('status.traffic.perSlug') }}</template>
          <el-table :data="traffic?.per_slug || []" size="small" style="width: 100%">
            <el-table-column prop="slug" :label="$t('status.traffic.slug')" min-width="140" />
            <el-table-column prop="requests" :label="$t('status.traffic.requests')" width="110" />
            <el-table-column prop="rate_limited" :label="$t('status.traffic.rateLimited')" width="140" />
            <el-table-column prop="client_errors" :label="$t('status.traffic.clientErrors')" width="90" />
            <el-table-column prop="server_errors" :label="$t('status.traffic.serverErrors')" width="90" />
            <el-table-column prop="avg_latency_ms" :label="$t('status.traffic.latency')" width="120" />
          </el-table>
        </el-card>
      </el-tab-pane>

      <!-- Live logs -->
      <el-tab-pane :label="$t('status.logs')" name="logs">
        <el-card shadow="never">
          <div class="log-toolbar">
            <el-radio-group v-model="logView" size="small">
              <el-radio-button label="events">{{ $t('status.logs.events') }}</el-radio-button>
              <el-radio-button label="logs">{{ $t('status.logs.appLogs') }}</el-radio-button>
            </el-radio-group>
            <el-select v-model="levelFilter" size="small" style="width: 130px">
              <el-option :label="$t('status.logs.all')" value="all" />
              <el-option label="info" value="info" />
              <el-option label="warning" value="warning" />
              <el-option label="error" value="error" />
            </el-select>
            <el-input
              v-model="textFilter"
              size="small"
              clearable
              :placeholder="$t('status.logs.filter')"
              class="log-filter"
            />
            <el-switch
              v-model="paused"
              size="small"
              :active-text="paused ? $t('status.logs.resume') : $t('status.logs.pause')"
            />
            <el-button size="small" @click="clearEntries">{{ $t('status.logs.clear') }}</el-button>
          </div>

          <el-table
            v-if="logView === 'events'"
            :data="filteredEvents"
            size="small"
            max-height="520"
            style="width: 100%"
          >
            <el-table-column :label="$t('status.logs.time')" width="110">
              <template #default="scope">{{ formatTime(scope.row.time) }}</template>
            </el-table-column>
            <el-table-column :label="$t('status.logs.level')" width="100">
              <template #default="scope">
                <el-tag :type="levelTag(scope.row.level)" size="small">{{ scope.row.level }}</el-tag>
              </template>
            </el-table-column>
            <el-table-column prop="kind" :label="$t('status.logs.kind')" width="100" />
            <el-table-column prop="slug" :label="$t('status.logs.slug')" width="130" />
            <el-table-column prop="status" :label="$t('status.logs.status')" width="80" />
            <el-table-column prop="latency_ms" :label="$t('status.logs.latency')" width="110" />
            <el-table-column prop="message" :label="$t('status.logs.message')" min-width="320" />
          </el-table>

          <div v-else ref="logPanel" class="log-terminal">
            <div v-if="filteredLogs.length === 0" class="log-empty">{{ $t('status.logs.empty') }}</div>
            <div v-for="line in filteredLogs" :key="line.id" class="log-line" :class="'log-' + line.level">
              <span class="log-time">{{ formatTime(line.time) }}</span>
              <span class="log-level">{{ (line.level || 'info').toUpperCase() }}</span>
              <span class="log-msg">{{ line.message }}</span>
            </div>
          </div>
        </el-card>
      </el-tab-pane>
    </el-tabs>
  </div>
</template>

<script setup>
import { ref, reactive, computed, watch, onMounted, onBeforeUnmount, nextTick } from 'vue'
import {
  getStatusOverview,
  getStatusTraffic,
  getStatusEvents,
  getStatusLogs,
} from '../api'
import { openStatusStream } from '../api/stream'

const MAX_EVENTS = 200
const MAX_LOGS = 500

const activeTab = ref('overview')
const autoRefresh = ref(true)
const connected = ref(false)
const overview = ref(null)
const traffic = ref(null)
const events = ref([])
const logs = ref([])
const paused = ref(false)
const logView = ref('events')
const levelFilter = ref('all')
const textFilter = ref('')
const logPanel = ref(null)
const loading = reactive({ overview: false, traffic: false })

const lastEventId = ref(0)
const lastLogId = ref(0)

let stopStream = null
let pollTimer = null

const descColumns = computed(() => (window.innerWidth <= 768 ? 1 : 3))

const stats = computed(() => {
  const t = traffic.value
  const classes = t?.status_classes || {}
  return {
    requests: t?.total_requests ?? 0,
    rateLimited: Number(t?.status_counts?.['429'] ?? 0),
    clientErrors: classes['4xx'] ?? 0,
    serverErrors: classes['5xx'] ?? 0,
    avgLatency: t?.avg_latency_ms ?? 0,
  }
})

const sinceLabel = computed(() => (traffic.value?.since ? formatDateTime(traffic.value.since) : '-'))

const rpmBars = computed(() => {
  const series = traffic.value?.requests_per_minute || []
  const max = Math.max(1, ...series)
  return series.map((v, i) => ({
    v,
    h: Math.max(v > 0 ? 4 : 0, Math.round((v / max) * 100)),
    last: i === series.length - 1,
  }))
})

const filteredEvents = computed(() => {
  const lf = levelFilter.value
  const tf = textFilter.value.trim().toLowerCase()
  return events.value.filter((ev) => {
    if (lf !== 'all' && ev.level !== lf) return false
    if (!tf) return true
    return `${ev.message || ''} ${ev.slug || ''} ${ev.path || ''}`.toLowerCase().includes(tf)
  })
})

const filteredLogs = computed(() => {
  const lf = levelFilter.value
  const tf = textFilter.value.trim().toLowerCase()
  return logs.value.filter((line) => {
    if (lf !== 'all' && line.level !== lf) return false
    if (!tf) return true
    return (line.message || '').toLowerCase().includes(tf)
  })
})

function mergeEvents(list) {
  if (!Array.isArray(list) || list.length === 0) return
  let maxId = lastEventId.value
  const add = []
  for (const ev of list) {
    if (!ev || typeof ev.id !== 'number' || ev.id <= lastEventId.value) continue
    add.push(ev)
    if (ev.id > maxId) maxId = ev.id
  }
  if (add.length === 0) return
  events.value.push(...add)
  lastEventId.value = maxId
  if (events.value.length > MAX_EVENTS) {
    events.value.splice(0, events.value.length - MAX_EVENTS)
  }
}

function mergeLogs(list) {
  if (!Array.isArray(list) || list.length === 0) return
  let maxId = lastLogId.value
  const add = []
  for (const line of list) {
    if (!line || typeof line.id !== 'number' || line.id <= lastLogId.value) continue
    add.push(line)
    if (line.id > maxId) maxId = line.id
  }
  if (add.length === 0) return
  logs.value.push(...add)
  lastLogId.value = maxId
  if (logs.value.length > MAX_LOGS) {
    logs.value.splice(0, logs.value.length - MAX_LOGS)
  }
}

async function loadOverview() {
  loading.overview = true
  try {
    const { data } = await getStatusOverview()
    overview.value = data
  } catch (e) {
    // The axios interceptor already surfaces the error.
  } finally {
    loading.overview = false
  }
}

async function loadTraffic() {
  loading.traffic = true
  try {
    const { data } = await getStatusTraffic()
    traffic.value = data
  } catch (e) {
    // handled by interceptor
  } finally {
    loading.traffic = false
  }
}

async function loadHistory() {
  try {
    const [{ data: ev }, { data: lg }] = await Promise.all([
      getStatusEvents(MAX_EVENTS),
      getStatusLogs(MAX_LOGS),
    ])
    mergeEvents(ev?.events || [])
    mergeLogs(lg?.logs || [])
  } catch (e) {
    // handled by interceptor
  }
}

function refreshAll() {
  loadOverview()
  loadTraffic()
  loadHistory()
}

function startStream() {
  stopStream = openStatusStream({
    onOpen: () => {
      connected.value = true
    },
    onError: () => {
      connected.value = false
    },
    onEvent: (ev) => {
      if (paused.value) return
      mergeEvents([ev])
    },
    onLog: (line) => {
      if (paused.value) return
      mergeLogs([line])
    },
  })
}

function clearEntries() {
  if (logView.value === 'events') {
    events.value = []
  } else {
    logs.value = []
  }
}

function setupPolling() {
  if (pollTimer) {
    clearInterval(pollTimer)
    pollTimer = null
  }
  if (!autoRefresh.value) return
  pollTimer = setInterval(() => {
    loadOverview()
    loadTraffic()
  }, 5000)
}

watch(autoRefresh, setupPolling)

watch(
  () => filteredLogs.value.length,
  async () => {
    if (paused.value || logView.value !== 'logs') return
    await nextTick()
    const el = logPanel.value
    if (el) el.scrollTop = el.scrollHeight
  }
)

onMounted(() => {
  refreshAll()
  startStream()
  setupPolling()
})

onBeforeUnmount(() => {
  if (stopStream) stopStream()
  if (pollTimer) clearInterval(pollTimer)
})

function formatUptime(seconds) {
  if (seconds === undefined || seconds === null) return '-'
  const s = Math.max(0, Math.floor(seconds))
  const d = Math.floor(s / 86400)
  const h = Math.floor((s % 86400) / 3600)
  const m = Math.floor((s % 3600) / 60)
  const sec = s % 60
  const parts = []
  if (d) parts.push(`${d}d`)
  if (h) parts.push(`${h}h`)
  if (m) parts.push(`${m}m`)
  parts.push(`${sec}s`)
  return parts.join(' ')
}

function formatTime(value) {
  try {
    return new Date(value).toLocaleTimeString()
  } catch (e) {
    return String(value || '')
  }
}

function formatDateTime(value) {
  try {
    return new Date(value).toLocaleString()
  } catch (e) {
    return String(value || '')
  }
}

function levelTag(level) {
  if (level === 'error') return 'danger'
  if (level === 'warning') return 'warning'
  return 'info'
}
</script>

<style scoped>
.status-page {
  display: flex;
  flex-direction: column;
  gap: 12px;
}
.header-bar {
  display: flex;
  justify-content: space-between;
  align-items: center;
  flex-wrap: wrap;
  gap: 10px;
}
.header-bar h1 {
  margin: 0;
  font-size: 20px;
}
.header-actions {
  display: flex;
  align-items: center;
  gap: 12px;
  flex-wrap: wrap;
}
.section-card {
  margin-top: 12px;
}
.card-header {
  display: flex;
  justify-content: space-between;
  align-items: center;
  gap: 8px;
  flex-wrap: wrap;
}
.muted {
  color: var(--el-text-color-secondary);
  font-size: 12px;
}
.stat-row {
  row-gap: 12px;
}
.stat-row .el-card {
  height: 100%;
}
.rpm-chart {
  display: flex;
  align-items: flex-end;
  gap: 2px;
  height: 90px;
  padding: 4px 0;
  border-bottom: 1px solid var(--el-border-color-lighter);
}
.rpm-bar {
  flex: 1 1 auto;
  min-width: 2px;
  background: var(--el-color-primary-light-5);
  border-radius: 2px 2px 0 0;
  transition: height 0.2s ease;
}
.rpm-bar-last {
  background: var(--el-color-primary);
}
.log-toolbar {
  display: flex;
  align-items: center;
  gap: 12px;
  flex-wrap: wrap;
  margin-bottom: 12px;
}
.log-filter {
  max-width: 240px;
}
.log-terminal {
  height: 520px;
  overflow-y: auto;
  background: #1e1e1e;
  color: #d4d4d4;
  font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace;
  font-size: 12px;
  line-height: 1.5;
  padding: 10px 12px;
  border-radius: 6px;
}
.log-line {
  white-space: pre-wrap;
  word-break: break-word;
  border-bottom: 1px solid rgba(255, 255, 255, 0.05);
  padding: 2px 0;
}
.log-time {
  color: #7f8c98;
  margin-right: 8px;
}
.log-level {
  display: inline-block;
  min-width: 64px;
  margin-right: 8px;
  color: #9cdcfe;
}
.log-error .log-level,
.log-error .log-msg {
  color: #f48771;
}
.log-warning .log-level,
.log-warning .log-msg {
  color: #dcdcaa;
}
.log-empty {
  color: #7f8c98;
}
@media (max-width: 768px) {
  .header-bar h1 {
    font-size: 17px;
  }
  .log-filter {
    max-width: 100%;
    width: 100%;
  }
  .log-terminal {
    height: 60vh;
  }
}
</style>