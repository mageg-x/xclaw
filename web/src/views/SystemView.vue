<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref } from 'vue'
import { Message } from '@arco-design/web-vue'
import { useSystemStore } from '@/stores/system'
import {
  getSystemMetrics,
  getTokenStats,
  getUpdateCheck,
  getVectorStatus,
  installSystemUpdate,
  restartSystem
} from '@/api/client'

const store = useSystemStore()

const prettyStatus = computed(() => JSON.stringify(store.status, null, 2))
const metrics = ref<Record<string, unknown>>({})
const tokenStats = ref<Record<string, unknown>>({})
const updateInfo = ref<Record<string, unknown>>({})
const vector = ref<Record<string, unknown>>({})
const refreshing = ref(false)
const showRaw = ref(false)
const restarting = ref(false)
const updating = ref(false)
const polling = ref(false)
let pollTimer: ReturnType<typeof setInterval> | null = null

const startupURL = computed(() => {
  if (!store.status?.server) {
    return ''
  }
  const schema = store.status.server.tls ? 'https' : 'http'
  return `${schema}://${store.status.server.host}:${store.status.server.port}`
})
const browserURL = computed(() => {
  if (typeof window === 'undefined') {
    return ''
  }
  return `${window.location.protocol}//${window.location.host}`
})
const schemaMismatch = computed(() => {
  if (!startupURL.value || !browserURL.value) {
    return false
  }
  const startupSchema = startupURL.value.startsWith('https://') ? 'https' : 'http'
  const browserSchema = browserURL.value.startsWith('https://') ? 'https' : 'http'
  return startupSchema !== browserSchema
})

const prettyMetrics = computed(() => JSON.stringify(metrics.value, null, 2))
const prettyTokenStats = computed(() => JSON.stringify(tokenStats.value, null, 2))
const prettyUpdateInfo = computed(() => JSON.stringify(updateInfo.value, null, 2))
const releaseInfo = computed(() => (updateInfo.value.release || {}) as Record<string, any>)
const selectedAsset = computed(() => (updateInfo.value.selected_asset || {}) as Record<string, any>)
const isDraining = computed(() => !!store.status?.draining)
const recoveringSessions = computed(() => Number(store.status?.recovering_sessions || 0))
const canOperate = computed(() => !isDraining.value && !refreshing.value && !restarting.value && !updating.value)

async function refreshAll(silent = false) {
  if (polling.value) {
    return
  }
  polling.value = true
  if (!silent) {
    refreshing.value = true
  }
  try {
    await store.refreshStatus()
    if (silent) {
      return
    }
    metrics.value = await getSystemMetrics()
    tokenStats.value = await getTokenStats()
    updateInfo.value = await getUpdateCheck()
    vector.value = await getVectorStatus()
  } finally {
    polling.value = false
    if (!silent) {
      refreshing.value = false
    }
  }
}

function startPolling() {
  if (pollTimer) return
  pollTimer = setInterval(() => {
    if (typeof document !== 'undefined' && document.hidden) {
      return
    }
    void refreshAll(true)
  }, 3000)
}

function stopPolling() {
  if (!pollTimer) return
  clearInterval(pollTimer)
  pollTimer = null
}

async function warmRefresh() {
  refreshing.value = true
  try {
    await refreshAll(true)
    metrics.value = await getSystemMetrics()
    tokenStats.value = await getTokenStats()
    updateInfo.value = await getUpdateCheck()
    vector.value = await getVectorStatus()
  } finally {
    refreshing.value = false
  }
}

function handleManualRefresh() {
  void warmRefresh()
}

async function handleRestart() {
  restarting.value = true
  try {
    await restartSystem()
    Message.success('已接受重启请求，服务正在重启')
  } catch (err: any) {
    Message.error(err?.message || '重启失败')
  } finally {
    restarting.value = false
  }
}

async function handleUpdateInstall() {
  updating.value = true
  try {
    const result = await installSystemUpdate('stable')
    if (result.already_latest) {
      Message.info('当前已经是最新版本')
    } else {
      Message.success('更新已下载并开始重启')
    }
  } catch (err: any) {
    Message.error(err?.message || '更新失败')
  } finally {
    updating.value = false
  }
}

onMounted(() => {
  warmRefresh()
  startPolling()
})

onBeforeUnmount(() => {
  stopPolling()
})
</script>

<template>
  <div class="page system-page">
    <div class="page-header">
      <div>
        <h2>系统总览</h2>
        <p>查看 XClaw 运行状态、关键指标和配置信息</p>
      </div>
      <a-button type="primary" :loading="refreshing" @click="handleManualRefresh" size="large">
        刷新状态
      </a-button>
      <a-space>
        <a-button :loading="updating" :disabled="!canOperate" status="warning" @click="handleUpdateInstall" size="large">
          下载并重启更新
        </a-button>
        <a-button :loading="restarting" :disabled="!canOperate" @click="handleRestart" size="large">
          重启服务
        </a-button>
      </a-space>
    </div>

    <a-alert v-if="isDraining" type="warning" style="margin-bottom: 20px">
      服务正在切换，后端已进入请求排空阶段。系统页会自动轮询最新运行态，直到新进程完全接管。
    </a-alert>

    <a-alert v-else-if="recoveringSessions > 0" type="info" style="margin-bottom: 20px">
      当前有 {{ recoveringSessions }} 个会话正在恢复未完成任务。恢复完成后会自动转回正常运行态。
    </a-alert>

    <a-alert type="info" style="margin-bottom: 20px">
      后端监听地址（配置值）：<strong>{{ startupURL || '加载中...' }}</strong><br />
      你当前访问地址（浏览器）：<strong>{{ browserURL || '加载中...' }}</strong>
    </a-alert>
    <a-alert v-if="schemaMismatch" type="warning" style="margin-bottom: 20px">
      你现在看到“浏览器是 {{ browserURL.startsWith('https://') ? 'HTTPS' : 'HTTP' }}，后端配置是
      {{ startupURL.startsWith('https://') ? 'HTTPS' : 'HTTP' }}”。
      <br />
      这通常表示：后端 TLS 配置和你实际访问方式不一致。若你要纯 HTTP，请把服务端 <code>server.tls</code> 设为 <code>false</code>。
    </a-alert>

    <div class="section-title">核心指标</div>
    <a-row :gutter="16" style="margin-bottom: 24px">
      <a-col :xs="24" :sm="12" :lg="6">
        <a-card class="metric-card" :bordered="false">
          <div class="metric-title">系统初始化</div>
          <div class="metric-value">{{ store.status?.bootstrapped ? '已完成' : '未完成' }}</div>
        </a-card>
      </a-col>
      <a-col :xs="24" :sm="12" :lg="6">
        <a-card class="metric-card" :bordered="false">
          <div class="metric-title">Agent 数量</div>
          <div class="metric-value">{{ store.status?.agents_count ?? 0 }}</div>
        </a-card>
      </a-col>
      <a-col :xs="24" :sm="12" :lg="6">
        <a-card class="metric-card" :bordered="false">
          <div class="metric-title">向量索引策略</div>
          <div class="metric-value">{{ String(vector.active_index_strategy ?? 'unknown') }}</div>
        </a-card>
      </a-col>
      <a-col :xs="24" :sm="12" :lg="6">
        <a-card class="metric-card" :bordered="false">
          <div class="metric-title">HNSW</div>
          <div class="metric-value">{{ vector.hnsw_supported ? '支持' : '不支持' }}</div>
        </a-card>
      </a-col>
    </a-row>

    <div class="section-title">运行态</div>
    <a-row :gutter="16" style="margin-bottom: 24px">
      <a-col :xs="24" :sm="12" :lg="6">
        <a-card class="metric-card" :bordered="false">
          <div class="metric-title">Draining</div>
          <div class="metric-value">{{ store.status?.draining ? '是' : '否' }}</div>
        </a-card>
      </a-col>
      <a-col :xs="24" :sm="12" :lg="6">
        <a-card class="metric-card" :bordered="false">
          <div class="metric-title">活跃请求</div>
          <div class="metric-value">{{ store.status?.active_requests ?? 0 }}</div>
        </a-card>
      </a-col>
      <a-col :xs="24" :sm="12" :lg="6">
        <a-card class="metric-card" :bordered="false">
          <div class="metric-title">SSE 连接</div>
          <div class="metric-value">{{ store.status?.active_sse ?? 0 }}</div>
        </a-card>
      </a-col>
      <a-col :xs="24" :sm="12" :lg="6">
        <a-card class="metric-card" :bordered="false">
          <div class="metric-title">WebSocket 连接</div>
          <div class="metric-value">{{ store.status?.active_ws ?? 0 }}</div>
        </a-card>
      </a-col>
      <a-col :xs="24" :sm="12" :lg="6">
        <a-card class="metric-card" :bordered="false">
          <div class="metric-title">恢复中会话</div>
          <div class="metric-value">{{ store.status?.recovering_sessions ?? 0 }}</div>
        </a-card>
      </a-col>
      <a-col :xs="24" :sm="12" :lg="6">
        <a-card class="metric-card" :bordered="false">
          <div class="metric-title">运行中会话</div>
          <div class="metric-value">{{ store.status?.running_sessions ?? 0 }}</div>
        </a-card>
      </a-col>
    </a-row>

    <div class="section-title">运行配置</div>
    <a-card :bordered="false" style="margin-bottom: 24px">
      <a-descriptions :column="{ xs: 1, md: 2 }" bordered>
        <a-descriptions-item label="主机">{{ store.status?.server.host || '-' }}</a-descriptions-item>
        <a-descriptions-item label="端口">{{ store.status?.server.port ?? '-' }}</a-descriptions-item>
        <a-descriptions-item label="沙箱模式">{{ store.status?.sandbox.mode || '-' }}</a-descriptions-item>
        <a-descriptions-item label="工作区权限">{{ store.status?.sandbox.workspace_access || '-' }}</a-descriptions-item>
        <a-descriptions-item label="作用域">{{ store.status?.sandbox.scope || '-' }}</a-descriptions-item>
        <a-descriptions-item label="TLS">{{ store.status?.server.tls ? '开启' : '关闭' }}</a-descriptions-item>
        <a-descriptions-item label="当前版本">{{ store.status?.version || '-' }}</a-descriptions-item>
        <a-descriptions-item label="请求排空中">{{ store.status?.draining ? '是' : '否' }}</a-descriptions-item>
      </a-descriptions>
    </a-card>

    <div class="section-title">更新控制</div>
    <a-card :bordered="false" style="margin-bottom: 24px">
      <a-descriptions :column="{ xs: 1, md: 2 }" bordered>
        <a-descriptions-item label="最新版本">{{ String(releaseInfo.tag_name || '-') }}</a-descriptions-item>
        <a-descriptions-item label="当前版本">{{ String(store.status?.version || '-') }}</a-descriptions-item>
        <a-descriptions-item label="发布资产">{{ String(selectedAsset.name || '-') }}</a-descriptions-item>
        <a-descriptions-item label="资产大小">{{ selectedAsset.size ? `${selectedAsset.size} bytes` : '-' }}</a-descriptions-item>
      </a-descriptions>
    </a-card>

    <a-card :bordered="false" title="高级诊断" style="margin-bottom: 24px">
      <a-space style="margin-bottom: 16px">
        <a-typography-text>显示原始 JSON（高级）</a-typography-text>
        <a-switch v-model="showRaw" />
      </a-space>
      <a-tabs v-if="showRaw" lazy-load>
        <a-tab-pane key="status" title="系统状态 JSON">
          <pre class="json-panel">{{ prettyStatus }}</pre>
        </a-tab-pane>
        <a-tab-pane key="metrics" title="系统指标 JSON">
          <pre class="json-panel">{{ prettyMetrics }}</pre>
        </a-tab-pane>
        <a-tab-pane key="token" title="Token 统计 JSON">
          <pre class="json-panel">{{ prettyTokenStats }}</pre>
        </a-tab-pane>
        <a-tab-pane key="update" title="更新检查 JSON">
          <pre class="json-panel">{{ prettyUpdateInfo }}</pre>
        </a-tab-pane>
      </a-tabs>
    </a-card>
  </div>
</template>

<style scoped>
.system-page {
  min-height: 100%;
}

.startup-alert strong {
  font-family: 'JetBrains Mono', monospace;
  font-size: 13px;
}
</style>
