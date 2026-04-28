<script setup lang="ts">
import { computed, onMounted, reactive, ref } from 'vue'
import { Message } from '@arco-design/web-vue'
import {
  deleteGatewayBinding,
  deleteGatewayDLQ,
  deleteGatewayRoute,
  getGatewayConfig,
  listGatewayBindings,
  listGatewayDLQ,
  listGatewayProviders,
  listGatewayRoutes,
  purgeGatewayDLQ,
  replayGatewayDLQ,
  retryAllGatewayDLQ,
  updateGatewayConfig,
  upsertGatewayBinding,
  upsertGatewayProvider,
  upsertGatewayRoute,
  type GatewayBinding,
  type GatewayConfig,
  type GatewayDLQItem,
  type GatewayProviderConfig,
  type GatewayProviderHealth,
  type GatewayRouteRule
} from '@/api/client'
import { useSystemStore } from '@/stores/system'

const systemStore = useSystemStore()

const loading = ref(false)
const notSupported = ref(false)

const configForm = reactive<GatewayConfig>({
  default_target: '',
  fallback_targets: [],
  quiet_hours_start: '',
  quiet_hours_end: ''
})
const fallbackTargetsText = ref('')
const savingConfig = ref(false)

const providers = ref<GatewayProviderConfig[]>([])
const providerHealthMap = ref<Record<string, GatewayProviderHealth>>({})
const providerSettingsText = ref('')
const savingProvider = ref(false)
const providerForm = reactive<GatewayProviderConfig>({
  name: '',
  protocol: '',
  enabled: true,
  settings: {}
})

const filterAgentID = ref('')
const bindings = ref<GatewayBinding[]>([])
const bindingMetadataText = ref('{}')
const savingBinding = ref(false)
const bindingForm = reactive({
  id: '',
  agent_id: '',
  platform: '',
  chat_id: '',
  thread_id: '',
  sender_id: '',
  display_name: '',
  enabled: true
})

const routes = ref<GatewayRouteRule[]>([])
const routeMatchMetadataText = ref('{}')
const savingRoute = ref(false)
const routeForm = reactive<GatewayRouteRule>({
  name: '',
  priority: 100,
  enabled: true,
  match: {
    platform: '',
    chat_id: '',
    thread_id: '',
    sender_id: '',
    event_type: '',
    content_prefix: '',
    regex: '',
    mention: '',
    metadata: {}
  },
  action: {
    target_agent: '',
    target: '',
    target_session: '',
    strip_prefix: false,
    create_session: true,
    priority: 'normal'
  }
})

const dlq = ref<GatewayDLQItem[]>([])
const dlqLoading = ref(false)
const dlqOperatingIDs = ref<string[]>([])
const supportsGateway = computed(() => !notSupported.value)

function parseKVText(text: string) {
  const out: Record<string, string> = {}
  for (const rawLine of text.split('\n')) {
    const line = rawLine.trim()
    if (!line || line.startsWith('#')) continue
    const idx = line.indexOf('=')
    if (idx <= 0) continue
    const key = line.slice(0, idx).trim()
    const value = line.slice(idx + 1).trim()
    if (key) out[key] = value
  }
  return out
}

function kvToText(data: Record<string, string>) {
  return Object.entries(data)
    .map(([k, v]) => `${k}=${v}`)
    .join('\n')
}

function safeJSONMap(text: string) {
  const raw = text.trim()
  if (!raw) return {}
  const parsed = JSON.parse(raw)
  if (parsed && typeof parsed === 'object' && !Array.isArray(parsed)) {
    const out: Record<string, string> = {}
    for (const [k, v] of Object.entries(parsed)) {
      out[k] = String(v ?? '')
    }
    return out
  }
  throw new Error('请输入 JSON 对象，例如 {"key":"value"}')
}

function resetProviderForm() {
  providerForm.name = ''
  providerForm.protocol = ''
  providerForm.enabled = true
  providerSettingsText.value = ''
}

function resetBindingForm() {
  bindingForm.id = ''
  bindingForm.agent_id = systemStore.agents[0]?.id || ''
  bindingForm.platform = ''
  bindingForm.chat_id = ''
  bindingForm.thread_id = ''
  bindingForm.sender_id = ''
  bindingForm.display_name = ''
  bindingForm.enabled = true
  bindingMetadataText.value = '{}'
}

function resetRouteForm() {
  routeForm.name = ''
  routeForm.priority = 100
  routeForm.enabled = true
  routeForm.match.platform = ''
  routeForm.match.chat_id = ''
  routeForm.match.thread_id = ''
  routeForm.match.sender_id = ''
  routeForm.match.event_type = ''
  routeForm.match.content_prefix = ''
  routeForm.match.regex = ''
  routeForm.match.mention = ''
  routeForm.action.target_agent = ''
  routeForm.action.target = ''
  routeForm.action.target_session = ''
  routeForm.action.strip_prefix = false
  routeForm.action.create_session = true
  routeForm.action.priority = 'normal'
  routeMatchMetadataText.value = '{}'
}

function loadProviderToForm(item: GatewayProviderConfig) {
  providerForm.name = item.name
  providerForm.protocol = item.protocol
  providerForm.enabled = item.enabled
  providerSettingsText.value = kvToText(item.settings || {})
}

function loadBindingToForm(item: GatewayBinding) {
  bindingForm.id = item.id || ''
  bindingForm.agent_id = item.agent_id || ''
  bindingForm.platform = item.platform || ''
  bindingForm.chat_id = item.chat_id || ''
  bindingForm.thread_id = item.thread_id || ''
  bindingForm.sender_id = item.sender_id || ''
  bindingForm.display_name = item.display_name || ''
  bindingForm.enabled = item.enabled
  bindingMetadataText.value = JSON.stringify(item.metadata || {}, null, 2)
}

function loadRouteToForm(item: GatewayRouteRule) {
  routeForm.name = item.name
  routeForm.priority = item.priority
  routeForm.enabled = item.enabled
  routeForm.match.platform = item.match.platform || ''
  routeForm.match.chat_id = item.match.chat_id || ''
  routeForm.match.thread_id = item.match.thread_id || ''
  routeForm.match.sender_id = item.match.sender_id || ''
  routeForm.match.event_type = item.match.event_type || ''
  routeForm.match.content_prefix = item.match.content_prefix || ''
  routeForm.match.regex = item.match.regex || ''
  routeForm.match.mention = item.match.mention || ''
  routeForm.action.target_agent = item.action.target_agent || ''
  routeForm.action.target = item.action.target || ''
  routeForm.action.target_session = item.action.target_session || ''
  routeForm.action.strip_prefix = item.action.strip_prefix
  routeForm.action.create_session = item.action.create_session
  routeForm.action.priority = item.action.priority || 'normal'
  routeMatchMetadataText.value = JSON.stringify(item.match.metadata || {}, null, 2)
}

async function refreshConfig() {
  const cfg = await getGatewayConfig()
  configForm.default_target = cfg.default_target || ''
  configForm.fallback_targets = cfg.fallback_targets || []
  configForm.quiet_hours_start = cfg.quiet_hours_start || ''
  configForm.quiet_hours_end = cfg.quiet_hours_end || ''
  fallbackTargetsText.value = configForm.fallback_targets.join('\n')
}

async function refreshProviders() {
  const data = await listGatewayProviders()
  providers.value = data.configs || []
  providerHealthMap.value = data.health || {}
}

async function refreshBindings() {
  bindings.value = await listGatewayBindings(filterAgentID.value)
}

async function refreshRoutes() {
  routes.value = await listGatewayRoutes()
}

async function refreshDLQ() {
  dlqLoading.value = true
  try {
    dlq.value = await listGatewayDLQ()
  } finally {
    dlqLoading.value = false
  }
}

async function refreshAll() {
  loading.value = true
  notSupported.value = false
  try {
    await systemStore.refreshAgents()
    await Promise.all([refreshConfig(), refreshProviders(), refreshBindings(), refreshRoutes(), refreshDLQ()])
    if (!bindingForm.agent_id && systemStore.agents.length > 0) {
      bindingForm.agent_id = systemStore.agents[0].id
    }
  } catch (error) {
    const status = (error as any)?.response?.status
    if (status === 501) {
      notSupported.value = true
      return
    }
    Message.error((error as Error).message || '加载 gateway 数据失败')
  } finally {
    loading.value = false
  }
}

async function saveGatewayConfig() {
  savingConfig.value = true
  try {
    configForm.fallback_targets = fallbackTargetsText.value
      .split(/[\n,]/)
      .map((item) => item.trim())
      .filter(Boolean)
    const data = await updateGatewayConfig({ ...configForm })
    configForm.default_target = data.default_target || ''
    configForm.fallback_targets = data.fallback_targets || []
    configForm.quiet_hours_start = data.quiet_hours_start || ''
    configForm.quiet_hours_end = data.quiet_hours_end || ''
    fallbackTargetsText.value = configForm.fallback_targets.join('\n')
    Message.success('Gateway 基础配置已保存')
  } catch (error) {
    Message.error((error as Error).message)
  } finally {
    savingConfig.value = false
  }
}

async function saveProvider() {
  if (!providerForm.name.trim() || !providerForm.protocol.trim()) {
    Message.warning('Provider 名称和协议不能为空')
    return
  }
  savingProvider.value = true
  try {
    await upsertGatewayProvider({
      name: providerForm.name.trim().toLowerCase(),
      protocol: providerForm.protocol.trim().toLowerCase(),
      enabled: providerForm.enabled,
      settings: parseKVText(providerSettingsText.value)
    })
    Message.success('Provider 配置已保存')
    await refreshProviders()
  } catch (error) {
    Message.error((error as Error).message)
  } finally {
    savingProvider.value = false
  }
}

async function saveBinding() {
  if (!bindingForm.agent_id || !bindingForm.platform.trim() || !bindingForm.chat_id.trim()) {
    Message.warning('Agent、平台、会话 ID 为必填项')
    return
  }
  savingBinding.value = true
  try {
    await upsertGatewayBinding({
      id: bindingForm.id.trim() || undefined,
      agent_id: bindingForm.agent_id,
      platform: bindingForm.platform.trim().toLowerCase(),
      chat_id: bindingForm.chat_id.trim(),
      thread_id: bindingForm.thread_id.trim(),
      sender_id: bindingForm.sender_id.trim(),
      display_name: bindingForm.display_name.trim(),
      enabled: bindingForm.enabled,
      metadata: safeJSONMap(bindingMetadataText.value)
    })
    Message.success('绑定已保存')
    await refreshBindings()
    resetBindingForm()
  } catch (error) {
    Message.error((error as Error).message)
  } finally {
    savingBinding.value = false
  }
}

async function removeBinding(id: string) {
  try {
    await deleteGatewayBinding(id)
    Message.success('绑定已删除')
    await refreshBindings()
  } catch (error) {
    Message.error((error as Error).message)
  }
}

async function saveRoute() {
  if (!routeForm.name.trim()) {
    Message.warning('路由名称不能为空')
    return
  }
  savingRoute.value = true
  try {
    await upsertGatewayRoute({
      name: routeForm.name.trim(),
      priority: routeForm.priority,
      enabled: routeForm.enabled,
      match: {
        ...routeForm.match,
        platform: routeForm.match.platform.trim().toLowerCase(),
        chat_id: routeForm.match.chat_id.trim(),
        thread_id: routeForm.match.thread_id.trim(),
        sender_id: routeForm.match.sender_id.trim(),
        event_type: routeForm.match.event_type.trim(),
        content_prefix: routeForm.match.content_prefix.trim(),
        regex: routeForm.match.regex.trim(),
        mention: routeForm.match.mention.trim(),
        metadata: safeJSONMap(routeMatchMetadataText.value)
      },
      action: {
        ...routeForm.action,
        target_agent: routeForm.action.target_agent.trim(),
        target: routeForm.action.target.trim(),
        target_session: routeForm.action.target_session.trim(),
        priority: routeForm.action.priority.trim() || 'normal'
      }
    })
    Message.success('路由规则已保存')
    await refreshRoutes()
  } catch (error) {
    Message.error((error as Error).message)
  } finally {
    savingRoute.value = false
  }
}

async function removeRoute(name: string) {
  try {
    await deleteGatewayRoute(name)
    Message.success('路由已删除')
    await refreshRoutes()
  } catch (error) {
    Message.error((error as Error).message)
  }
}

function markDLQOperating(id: string, loadingStatus: boolean) {
  if (loadingStatus) {
    dlqOperatingIDs.value = Array.from(new Set([...dlqOperatingIDs.value, id]))
    return
  }
  dlqOperatingIDs.value = dlqOperatingIDs.value.filter((item) => item !== id)
}

async function replayOneDLQ(id: string) {
  markDLQOperating(id, true)
  try {
    await replayGatewayDLQ(id)
    Message.success(`已重试 ${id}`)
    await refreshDLQ()
  } catch (error) {
    Message.error((error as Error).message)
  } finally {
    markDLQOperating(id, false)
  }
}

async function removeOneDLQ(id: string) {
  markDLQOperating(id, true)
  try {
    await deleteGatewayDLQ(id)
    Message.success(`已删除 ${id}`)
    await refreshDLQ()
  } catch (error) {
    Message.error((error as Error).message)
  } finally {
    markDLQOperating(id, false)
  }
}

async function retryAllDLQItems() {
  try {
    const result = await retryAllGatewayDLQ()
    Message.success(`批量重试完成：成功 ${result.retried.length}，失败 ${result.failed.length}`)
    await refreshDLQ()
  } catch (error) {
    Message.error((error as Error).message)
  }
}

async function purgeAllDLQItems() {
  try {
    const result = await purgeGatewayDLQ()
    Message.success(`批量清空完成：成功 ${result.purged.length}，失败 ${result.failed.length}`)
    await refreshDLQ()
  } catch (error) {
    Message.error((error as Error).message)
  }
}

onMounted(async () => {
  resetBindingForm()
  await refreshAll()
})
</script>

<template>
  <div class="page">
    <div class="page-header">
      <div>
        <h2>社交通道与消息网关</h2>
        <p>统一管理 Telegram/Discord/企业微信/Slack 等平台接入、路由和失败重试。</p>
      </div>
      <a-space>
        <a-button :loading="loading" @click="refreshAll">刷新</a-button>
      </a-space>
    </div>

    <a-alert type="info" style="margin-bottom: 16px">
      建议顺序：先配置 Provider 凭据，再创建 Binding（平台会话绑定到 Agent），最后设置 Route（规则转发）。
    </a-alert>

    <a-empty v-if="notSupported" description="当前后端未启用 Gateway 服务，请在服务端配置中开启后再使用。" />

    <a-tabs v-else lazy-load>
      <a-tab-pane key="config" title="基础配置">
        <a-card :bordered="false">
          <a-form :model="configForm" layout="vertical">
            <a-row :gutter="16">
              <a-col :xs="24" :md="12">
                <a-form-item label="默认目标 Target">
                  <a-input v-model="configForm.default_target" placeholder="例如 telegram:ops-room 或 slack:#alerts" />
                </a-form-item>
              </a-col>
              <a-col :xs="24" :md="12">
                <a-form-item label="兜底目标（每行一个或逗号分隔）">
                  <a-textarea
                    v-model="fallbackTargetsText"
                    :auto-size="{ minRows: 3, maxRows: 8 }"
                    placeholder="例如\ntelegram:ops-room\nslack:#oncall"
                  />
                </a-form-item>
              </a-col>
            </a-row>
            <a-row :gutter="16">
              <a-col :xs="24" :md="6">
                <a-form-item label="静默开始（HH:mm）">
                  <a-input v-model="configForm.quiet_hours_start" placeholder="23:00" />
                </a-form-item>
              </a-col>
              <a-col :xs="24" :md="6">
                <a-form-item label="静默结束（HH:mm）">
                  <a-input v-model="configForm.quiet_hours_end" placeholder="07:00" />
                </a-form-item>
              </a-col>
            </a-row>
            <a-space>
              <a-button type="primary" :loading="savingConfig" @click="saveGatewayConfig">保存配置</a-button>
            </a-space>
          </a-form>
        </a-card>
      </a-tab-pane>

      <a-tab-pane key="providers" title="Provider 配置">
        <div class="split" style="align-items: start">
          <a-card title="已注册 Provider" :bordered="false">
            <a-table :data="providers" :pagination="false" size="small" row-key="name">
              <a-table-column title="名称" data-index="name" />
              <a-table-column title="协议" data-index="protocol" :width="100" />
              <a-table-column title="启用" :width="80">
                <template #cell="{ record }">
                  <a-tag :color="record.enabled ? 'green' : 'gray'">{{ record.enabled ? '是' : '否' }}</a-tag>
                </template>
              </a-table-column>
              <a-table-column title="健康状态" :width="140">
                <template #cell="{ record }">
                  <a-tag :color="providerHealthMap[record.name]?.status === 'ok' ? 'green' : 'orange'">
                    {{ providerHealthMap[record.name]?.status || 'unknown' }}
                  </a-tag>
                </template>
              </a-table-column>
              <a-table-column title="操作" :width="90">
                <template #cell="{ record }">
                  <a-button size="mini" @click="loadProviderToForm(record)">编辑</a-button>
                </template>
              </a-table-column>
            </a-table>
          </a-card>

          <a-card title="编辑 Provider" :bordered="false">
            <a-form :model="providerForm" layout="vertical">
              <a-form-item label="Provider 名称">
                <a-input v-model="providerForm.name" placeholder="例如 telegram / weixin / slack" />
              </a-form-item>
              <a-form-item label="协议">
                <a-input v-model="providerForm.protocol" placeholder="例如 bot / webhook / api" />
              </a-form-item>
              <a-form-item label="启用状态">
                <a-switch v-model="providerForm.enabled" />
              </a-form-item>
              <a-form-item label="Settings（key=value，每行一项）">
                <a-textarea
                  v-model="providerSettingsText"
                  :auto-size="{ minRows: 8, maxRows: 14 }"
                  placeholder="token=xxx\nwebhook_secret=yyy\ninbound_token=zzz"
                />
              </a-form-item>
              <a-space>
                <a-button type="primary" :loading="savingProvider" @click="saveProvider">保存 Provider</a-button>
                <a-button @click="resetProviderForm">清空</a-button>
              </a-space>
            </a-form>
          </a-card>
        </div>
      </a-tab-pane>

      <a-tab-pane key="bindings" title="账号绑定">
        <div class="split" style="align-items: start">
          <a-card title="绑定列表" :bordered="false">
            <a-space style="margin-bottom: 12px">
              <a-select v-model="filterAgentID" placeholder="按 Agent 过滤" allow-clear style="width: 260px" @change="refreshBindings">
                <a-option value="">全部 Agent</a-option>
                <a-option v-for="agent in systemStore.agents" :key="agent.id" :value="agent.id">
                  {{ agent.emoji }} {{ agent.name }}
                </a-option>
              </a-select>
              <a-button @click="refreshBindings">刷新绑定</a-button>
            </a-space>
            <a-table :data="bindings" :pagination="{ pageSize: 8 }" size="small" row-key="id">
              <a-table-column title="ID" data-index="id" :width="180" :ellipsis="true" :tooltip="true" />
              <a-table-column title="Agent" data-index="agent_id" :width="180" :ellipsis="true" :tooltip="true" />
              <a-table-column title="平台" data-index="platform" :width="90" />
              <a-table-column title="Chat ID" data-index="chat_id" :width="170" :ellipsis="true" :tooltip="true" />
              <a-table-column title="启用" :width="70">
                <template #cell="{ record }">
                  <a-tag :color="record.enabled ? 'green' : 'gray'">{{ record.enabled ? '是' : '否' }}</a-tag>
                </template>
              </a-table-column>
              <a-table-column title="操作" :width="160">
                <template #cell="{ record }">
                  <a-space>
                    <a-button size="mini" @click="loadBindingToForm(record)">编辑</a-button>
                    <a-popconfirm content="确认删除该绑定？" @ok="removeBinding(record.id)">
                      <a-button size="mini" status="danger">删除</a-button>
                    </a-popconfirm>
                  </a-space>
                </template>
              </a-table-column>
            </a-table>
          </a-card>

          <a-card title="新建 / 编辑绑定" :bordered="false">
            <a-form :model="bindingForm" layout="vertical">
              <a-form-item label="绑定 ID（编辑时可填）">
                <a-input v-model="bindingForm.id" placeholder="留空将自动生成" />
              </a-form-item>
              <a-form-item label="目标 Agent">
                <a-select v-model="bindingForm.agent_id" placeholder="选择 Agent">
                  <a-option v-for="agent in systemStore.agents" :key="agent.id" :value="agent.id">
                    {{ agent.emoji }} {{ agent.name }}
                  </a-option>
                </a-select>
              </a-form-item>
              <a-form-item label="平台">
                <a-input v-model="bindingForm.platform" placeholder="例如 telegram / weixin / slack" />
              </a-form-item>
              <a-form-item label="Chat ID">
                <a-input v-model="bindingForm.chat_id" placeholder="平台会话 ID（必填）" />
              </a-form-item>
              <a-row :gutter="12">
                <a-col :span="12">
                  <a-form-item label="Thread ID">
                    <a-input v-model="bindingForm.thread_id" placeholder="可选" />
                  </a-form-item>
                </a-col>
                <a-col :span="12">
                  <a-form-item label="Sender ID">
                    <a-input v-model="bindingForm.sender_id" placeholder="可选" />
                  </a-form-item>
                </a-col>
              </a-row>
              <a-form-item label="显示名称">
                <a-input v-model="bindingForm.display_name" placeholder="可选" />
              </a-form-item>
              <a-form-item label="附加 Metadata（JSON 对象）">
                <a-textarea
                  v-model="bindingMetadataText"
                  :auto-size="{ minRows: 4, maxRows: 10 }"
                  placeholder='例如 {"team":"ops","lang":"zh-CN"}'
                />
              </a-form-item>
              <a-form-item label="启用状态">
                <a-switch v-model="bindingForm.enabled" />
              </a-form-item>
              <a-space>
                <a-button type="primary" :loading="savingBinding" @click="saveBinding">保存绑定</a-button>
                <a-button @click="resetBindingForm">清空</a-button>
              </a-space>
            </a-form>
          </a-card>
        </div>
      </a-tab-pane>

      <a-tab-pane key="routes" title="路由规则">
        <div class="split" style="align-items: start">
          <a-card title="路由列表" :bordered="false">
            <a-table :data="routes" :pagination="{ pageSize: 8 }" size="small" row-key="name">
              <a-table-column title="名称" data-index="name" :width="180" :ellipsis="true" :tooltip="true" />
              <a-table-column title="优先级" data-index="priority" :width="90" />
              <a-table-column title="平台匹配" :width="120">
                <template #cell="{ record }">{{ record.match.platform || '任意' }}</template>
              </a-table-column>
              <a-table-column title="目标 Agent" :width="180" :ellipsis="true" :tooltip="true">
                <template #cell="{ record }">{{ record.action.target_agent || '-' }}</template>
              </a-table-column>
              <a-table-column title="启用" :width="70">
                <template #cell="{ record }">
                  <a-tag :color="record.enabled ? 'green' : 'gray'">{{ record.enabled ? '是' : '否' }}</a-tag>
                </template>
              </a-table-column>
              <a-table-column title="操作" :width="160">
                <template #cell="{ record }">
                  <a-space>
                    <a-button size="mini" @click="loadRouteToForm(record)">编辑</a-button>
                    <a-popconfirm content="确认删除该路由？" @ok="removeRoute(record.name)">
                      <a-button size="mini" status="danger">删除</a-button>
                    </a-popconfirm>
                  </a-space>
                </template>
              </a-table-column>
            </a-table>
          </a-card>

          <a-card title="新建 / 编辑路由" :bordered="false">
            <a-form :model="routeForm" layout="vertical">
              <a-row :gutter="12">
                <a-col :span="12">
                  <a-form-item label="规则名称">
                    <a-input v-model="routeForm.name" placeholder="例如 triage-ops" />
                  </a-form-item>
                </a-col>
                <a-col :span="12">
                  <a-form-item label="优先级（数字越小越优先）">
                    <a-input-number v-model="routeForm.priority" :min="1" :max="9999" />
                  </a-form-item>
                </a-col>
              </a-row>

              <a-divider>匹配条件</a-divider>
              <a-row :gutter="12">
                <a-col :span="12">
                  <a-form-item label="平台">
                    <a-input v-model="routeForm.match.platform" placeholder="例如 telegram；留空表示任意" />
                  </a-form-item>
                </a-col>
                <a-col :span="12">
                  <a-form-item label="事件类型">
                    <a-input v-model="routeForm.match.event_type" placeholder="例如 message" />
                  </a-form-item>
                </a-col>
              </a-row>
              <a-row :gutter="12">
                <a-col :span="12">
                  <a-form-item label="Chat ID">
                    <a-input v-model="routeForm.match.chat_id" placeholder="留空表示任意" />
                  </a-form-item>
                </a-col>
                <a-col :span="12">
                  <a-form-item label="Thread ID">
                    <a-input v-model="routeForm.match.thread_id" placeholder="留空表示任意" />
                  </a-form-item>
                </a-col>
              </a-row>
              <a-row :gutter="12">
                <a-col :span="12">
                  <a-form-item label="前缀匹配">
                    <a-input v-model="routeForm.match.content_prefix" placeholder="例如 /ops" />
                  </a-form-item>
                </a-col>
                <a-col :span="12">
                  <a-form-item label="Regex 匹配">
                    <a-input v-model="routeForm.match.regex" placeholder="例如 (告警|报警)" />
                  </a-form-item>
                </a-col>
              </a-row>
              <a-form-item label="Match Metadata（JSON 对象）">
                <a-textarea
                  v-model="routeMatchMetadataText"
                  :auto-size="{ minRows: 3, maxRows: 8 }"
                  placeholder='例如 {"priority":"p1"}'
                />
              </a-form-item>

              <a-divider>动作配置</a-divider>
              <a-form-item label="目标 Agent">
                <a-select v-model="routeForm.action.target_agent" allow-clear>
                  <a-option v-for="agent in systemStore.agents" :key="agent.id" :value="agent.id">
                    {{ agent.emoji }} {{ agent.name }}
                  </a-option>
                </a-select>
              </a-form-item>
              <a-row :gutter="12">
                <a-col :span="12">
                  <a-form-item label="目标 Target">
                    <a-input v-model="routeForm.action.target" placeholder="例如 telegram:ops-room" />
                  </a-form-item>
                </a-col>
                <a-col :span="12">
                  <a-form-item label="目标 Session">
                    <a-input v-model="routeForm.action.target_session" placeholder="可选" />
                  </a-form-item>
                </a-col>
              </a-row>
              <a-row :gutter="12">
                <a-col :span="8">
                  <a-form-item label="优先级标签">
                    <a-input v-model="routeForm.action.priority" placeholder="normal/high" />
                  </a-form-item>
                </a-col>
                <a-col :span="8">
                  <a-form-item label="去除前缀">
                    <a-switch v-model="routeForm.action.strip_prefix" />
                  </a-form-item>
                </a-col>
                <a-col :span="8">
                  <a-form-item label="自动建会话">
                    <a-switch v-model="routeForm.action.create_session" />
                  </a-form-item>
                </a-col>
              </a-row>
              <a-form-item label="启用状态">
                <a-switch v-model="routeForm.enabled" />
              </a-form-item>
              <a-space>
                <a-button type="primary" :loading="savingRoute" @click="saveRoute">保存路由</a-button>
                <a-button @click="resetRouteForm">清空</a-button>
              </a-space>
            </a-form>
          </a-card>
        </div>
      </a-tab-pane>

      <a-tab-pane key="dlq" title="失败队列 DLQ">
        <a-card :bordered="false">
          <a-space style="margin-bottom: 12px">
            <a-button @click="refreshDLQ" :loading="dlqLoading">刷新 DLQ</a-button>
            <a-popconfirm content="确认批量重试所有失败消息？" @ok="retryAllDLQItems">
              <a-button type="primary" status="success">批量重试</a-button>
            </a-popconfirm>
            <a-popconfirm content="确认清空所有 DLQ 记录？" @ok="purgeAllDLQItems">
              <a-button status="danger">批量清空</a-button>
            </a-popconfirm>
          </a-space>

          <a-table :data="dlq" :loading="dlqLoading" :pagination="{ pageSize: 8 }" size="small" row-key="id">
            <a-table-column title="ID" data-index="id" :width="170" :ellipsis="true" :tooltip="true" />
            <a-table-column title="Agent" data-index="agent_id" :width="170" :ellipsis="true" :tooltip="true" />
            <a-table-column title="Target" data-index="target" :width="160" :ellipsis="true" :tooltip="true" />
            <a-table-column title="错误" data-index="error" :ellipsis="true" :tooltip="true" />
            <a-table-column title="重试次数" data-index="retry_count" :width="90" />
            <a-table-column title="创建时间" data-index="created_at" :width="190" />
            <a-table-column title="操作" :width="170">
              <template #cell="{ record }">
                <a-space>
                  <a-button
                    size="mini"
                    type="primary"
                    :loading="dlqOperatingIDs.includes(record.id)"
                    @click="replayOneDLQ(record.id)"
                  >
                    重试
                  </a-button>
                  <a-popconfirm content="确认删除该失败记录？" @ok="removeOneDLQ(record.id)">
                    <a-button size="mini" status="danger" :loading="dlqOperatingIDs.includes(record.id)">删除</a-button>
                  </a-popconfirm>
                </a-space>
              </template>
            </a-table-column>
          </a-table>
        </a-card>
      </a-tab-pane>
    </a-tabs>

    <a-spin :loading="loading" tip="加载中..." />
  </div>
</template>
