<script setup lang="ts">
import { computed, onMounted, reactive, ref, watch } from 'vue'
import { Message } from '@arco-design/web-vue'
import mermaid from 'mermaid'
import hljs from 'highlight.js/lib/core'
import json from 'highlight.js/lib/languages/json'
import 'highlight.js/styles/github-dark.css'
hljs.registerLanguage('json', json)
import {
  analyzeMultimodal,
  approveRequest,
  dispatchA2A,
  getSkillDetail,
  getSkillMarketURL,
  getModelRouting,
  installSkill,
  listA2APeers,
  listApprovalRequests,
  listApprovalRules,
  listInstalledSkills,
  listKnowledgeMounts,
  listMCPTools,
  listLoadedSkills,
  listMultimodalFiles,
  listSkillCatalog,
  listMCPServers,
  reindexKnowledgeMount,
  registerA2APeer,
  rejectRequest,
  reloadSkills,
  renderMultimodal,
  revealCredential,
  saveApprovalRules,
  saveCredential,
  searchKnowledge,
  searchVectorMemory,
  setModelRouting,
  speechToText,
  textToSpeech,
  uninstallSkill,
  uploadMultimodal,
  upsertKnowledgeMount,
  type A2APeer,
  type ApprovalRequest,
  type ApprovalRule,
  type KnowledgeMount,
  type LoadedSkill,
  type MultimodalFile,
  type SkillItem,
  type MCPServer,
  type MCPTool,
  type VectorMemoryHit
} from '@/api/client'

mermaid.initialize({ startOnLoad: false, securityLevel: 'loose' })

const skillsCatalog = ref<SkillItem[]>([])
const skillsInstalled = ref<SkillItem[]>([])
const loadedSkills = ref<LoadedSkill[]>([])
const multimodalFiles = ref<MultimodalFile[]>([])
const vectorHits = ref<VectorMemoryHit[]>([])
const peers = ref<A2APeer[]>([])
const revealedSecret = ref('')

const skillDetailVisible = ref(false)
const skillDetailName = ref('')
const skillDetailPrompt = ref('')
const skillDetailLoading = ref(false)

const approvalRules = ref<ApprovalRule[]>([])
const approvalRequests = ref<ApprovalRequest[]>([])
const approvalRulesJSON = ref('[]')

const knowledgeMounts = ref<KnowledgeMount[]>([])
const knowledgeHits = ref<VectorMemoryHit[]>([])

const multimodalSummary = ref('')
const multimodalMode = ref('')
const renderOutput = ref<{ kind: string; title: string; content: any } | null>(null)
const mermaidSVG = ref('')

const sttText = ref('')
const ttsAudioSrc = ref('')

const routing = reactive({
  enabled: true,
  low_complexity_max: 8,
  medium_complexity_max: 18,
  hard_keywords_text: '架构,安全,优化,debug,root cause,distributed'
})

const skillSearch = ref('')
const skillCatalogLoading = ref(false)
const skillMarketURL = ref('')
const skillInstallLoading = ref(false)
const skillChangeReport = ref<{
  action: 'install' | 'uninstall'
  itemName: string
  servers: MCPServer[]
  tools: MCPTool[]
} | null>(null)
const skillInstallForm = reactive({
  mode: 'path' as 'path' | 'url',
  source_path: '',
  source_url: '',
  checksum: '',
  name: '',
  version: ''
})
const installedSkillSet = computed(() => {
  const set = new Set<string>()
  for (const item of skillsInstalled.value) {
    set.add(item.name)
  }
  return set
})
const builtinSkills = computed(() => skillsCatalog.value.filter((item) => item.source === 'builtin'))
const marketSkills = computed(() => skillsCatalog.value.filter((item) => item.source !== 'builtin'))
const installedMarketSkills = computed(() => skillsInstalled.value.filter((item) => item.source !== 'builtin'))

const jsonHighlighted = computed(() => {
  try {
    return hljs.highlight(approvalRulesJSON.value, { language: 'json' }).value
  } catch {
    return approvalRulesJSON.value
      .replace(/&/g, '&amp;')
      .replace(/</g, '&lt;')
      .replace(/>/g, '&gt;')
  }
})

const jsonValid = computed(() => {
  try {
    JSON.parse(approvalRulesJSON.value)
    return true
  } catch {
    return false
  }
})

function formatJSON() {
  try {
    const parsed = JSON.parse(approvalRulesJSON.value)
    approvalRulesJSON.value = JSON.stringify(parsed, null, 2)
    Message.success('JSON 已格式化')
  } catch {
    Message.error('JSON 格式不合法，无法格式化')
  }
}

const jsonEditorRef = ref<HTMLElement | null>(null)
const jsonEditableRef = ref<HTMLElement | null>(null)

function syncJsonScroll(event: Event) {
  const el = event.target as HTMLElement
  const container = el.parentElement
  if (!container) return
  const pre = container.querySelector('.json-editor-highlight') as HTMLElement
  if (pre) {
    pre.scrollTop = el.scrollTop
    pre.scrollLeft = el.scrollLeft
  }
}

function onJsonInput(event: Event) {
  const el = event.target as HTMLElement
  approvalRulesJSON.value = el.innerText
}

function onJsonKeydown(event: KeyboardEvent) {
  if (event.key === 'Tab') {
    event.preventDefault()
    document.execCommand('insertText', false, '  ')
  }
}

const vectorForm = reactive({
  agent_id: '',
  query: '',
  limit: 5
})

const peerForm = reactive({
  name: '',
  url: ''
})

const dispatchForm = reactive({
  peer_url: '',
  from: 'xclaw-local',
  to: 'remote-agent',
  task: '',
  inputs_raw: ''
})

const credentialSaveForm = reactive({
  provider: '',
  secret: '',
  master_password: ''
})

const credentialRevealForm = reactive({
  provider: '',
  master_password: ''
})

const knowledgeForm = reactive({
  name: '',
  type: 'local',
  source: '',
  token: ''
})

const knowledgeSourceHint = computed(() => {
  switch (knowledgeForm.type) {
    case 'local':
      return '示例：/data/docs 或 D:\\docs。用于读取本机目录文档。'
    case 'git':
      return '示例：https://github.com/org/repo.git。系统会拉取仓库并建立索引。'
    case 'web':
      return '示例：https://a.com/doc1, https://b.com/doc2。支持逗号或换行多个 URL。'
    case 'notion':
      return '示例：Notion 页面/块 ID（通常是 32 位 ID）。需要填写 Notion Token。'
    default:
      return '请根据类型填写来源。'
  }
})

const knowledgeSourcePlaceholder = computed(() => {
  switch (knowledgeForm.type) {
    case 'local':
      return '本地目录路径，例如 /data/docs'
    case 'git':
      return 'Git 仓库 URL，例如 https://github.com/org/repo.git'
    case 'web':
      return '网页 URL（可多个），例如 https://a.com/doc, https://b.com/doc'
    case 'notion':
      return 'Notion 页面/块 ID'
    default:
      return '来源'
  }
})

function applyKnowledgeExample() {
  switch (knowledgeForm.type) {
    case 'local':
      knowledgeForm.source = '/data/docs'
      break
    case 'git':
      knowledgeForm.source = 'https://github.com/org/repo.git'
      break
    case 'web':
      knowledgeForm.source = 'https://example.com/doc-1, https://example.com/doc-2'
      break
    case 'notion':
      knowledgeForm.source = 'xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx'
      break
    default:
      knowledgeForm.source = ''
      break
  }
}

const knowledgeSearchForm = reactive({
  query: '',
  limit: 6,
  mount_id: ''
})

const multimodalAnalyzeForm = reactive({
  id: '',
  prompt: '请提取关键内容，按要点输出。',
  model: ''
})

const renderForm = reactive({
  kind: 'mermaid' as 'mermaid' | 'echarts' | 'html' | 'image',
  title: '自动生成图表',
  prompt: ''
})

const ttsForm = reactive({
  text: '',
  voice: 'alloy',
  model: 'gpt-4o-mini-tts'
})

async function refreshAll() {
  await refreshSkillCatalog()
  skillMarketURL.value = await getSkillMarketURL()
  skillsInstalled.value = await listInstalledSkills()
  try {
    loadedSkills.value = await listLoadedSkills()
  } catch {
    loadedSkills.value = []
  }
  multimodalFiles.value = await listMultimodalFiles()
  peers.value = await listA2APeers()
  approvalRules.value = await listApprovalRules()
  approvalRulesJSON.value = JSON.stringify(approvalRules.value, null, 2)
  approvalRequests.value = await listApprovalRequests()
  knowledgeMounts.value = await listKnowledgeMounts()
  const route = await getModelRouting()
  routing.enabled = !!route.enabled
  routing.low_complexity_max = route.low_complexity_max
  routing.medium_complexity_max = route.medium_complexity_max
  routing.hard_keywords_text = (route.hard_keywords || []).join(',')
}

async function refreshSkillCatalog() {
  skillCatalogLoading.value = true
  try {
    skillsCatalog.value = await listSkillCatalog(skillSearch.value)
  } finally {
    skillCatalogLoading.value = false
  }
}

async function doInstallSkill(item: SkillItem) {
  if (item.source === 'builtin') {
    Message.info('内置技能始终可用，无需安装')
    return
  }
  const name = item.name.trim()
  const version = (item.version || '').trim()
  const source = (item.source || skillMarketURL.value || 'market').trim()
  if (!name) {
    Message.error('技能名不能为空')
    return
  }
  try {
    skillInstallLoading.value = true
    const result = await installSkill({
      name,
      version,
      source
    })
    await updateSkillChangeReport('install', result.item.name, result.registered_mcp_servers || [])
    Message.success('技能安装成功')
    skillsInstalled.value = await listInstalledSkills()
    loadedSkills.value = await listLoadedSkills()
  } catch (error) {
    Message.error((error as Error).message)
  } finally {
    skillInstallLoading.value = false
  }
}

async function doInstallSkillFromSource() {
  const sourcePath = skillInstallForm.source_path.trim()
  const sourceURL = skillInstallForm.source_url.trim()
  if (skillInstallForm.mode === 'path' && !sourcePath) {
    Message.error('本地路径不能为空')
    return
  }
  if (skillInstallForm.mode === 'url' && !sourceURL) {
    Message.error('URL 不能为空')
    return
  }
  try {
    skillInstallLoading.value = true
    const result = await installSkill({
      name: skillInstallForm.name.trim() || undefined,
      version: skillInstallForm.version.trim() || undefined,
      source: skillInstallForm.mode === 'path' ? 'local' : 'url',
      source_path: skillInstallForm.mode === 'path' ? sourcePath : undefined,
      source_url: skillInstallForm.mode === 'url' ? sourceURL : undefined,
      checksum: skillInstallForm.checksum.trim() || undefined
    })
    await updateSkillChangeReport('install', result.item.name, result.registered_mcp_servers || [])
    Message.success('技能安装成功')
    skillInstallForm.source_path = ''
    skillInstallForm.source_url = ''
    skillInstallForm.checksum = ''
    skillInstallForm.name = ''
    skillInstallForm.version = ''
    await refreshSkillCatalog()
    skillsInstalled.value = await listInstalledSkills()
    loadedSkills.value = await listLoadedSkills()
  } catch (error) {
    Message.error((error as Error).message)
  } finally {
    skillInstallLoading.value = false
  }
}

async function updateSkillChangeReport(action: 'install' | 'uninstall', itemName: string, initialServers: MCPServer[], initialTools?: string[]) {
  const serverIDs = new Set(initialServers.map((item) => item.id))
  let servers = initialServers
  if (action === 'install' && serverIDs.size === 0) {
    const serverItems = await listMCPServers()
    servers = serverItems.filter((item) => item.managed_by === `skill:${itemName}`)
    for (const item of servers) {
      serverIDs.add(item.id)
    }
  }
  let tools: MCPTool[] = []
  if (action === 'install' && serverIDs.size > 0) {
    const toolItems = await listMCPTools()
    tools = toolItems.filter((item) => serverIDs.has(item.server_id))
  } else if (action === 'uninstall' && Array.isArray(initialTools)) {
    tools = initialTools.map((fullName) => ({
      server_id: '',
      server_name: '',
      name: fullName.split(':').pop() || fullName,
      full_name: fullName,
      description: '',
      risk: '',
      transport: '',
      available: false
    }))
  }
  skillChangeReport.value = {
    action,
    itemName,
    servers,
    tools
  }
}

async function doUninstallSkill(name: string) {
  try {
    const result = await uninstallSkill({ name })
    await updateSkillChangeReport('uninstall', name, result.removed_mcp_servers || [], result.removed_mcp_tools || [])
    Message.success(`已卸载技能 ${name}`)
    skillsInstalled.value = await listInstalledSkills()
    try {
      loadedSkills.value = await listLoadedSkills()
    } catch { /* ignore */ }
  } catch (error) {
    Message.error((error as Error).message)
  }
}

function formatInstalledAt(value?: string) {
  if (!value) {
    return '-'
  }
  const date = new Date(value)
  if (Number.isNaN(date.getTime())) {
    return value
  }
  return date.toLocaleString()
}

async function doReloadSkills() {
  try {
    const result = await reloadSkills()
    loadedSkills.value = await listLoadedSkills()
    Message.success(`技能已重新加载，共 ${result.count} 个`)
  } catch (error) {
    Message.error((error as Error).message)
  }
}

async function doShowSkillDetail(name: string) {
  skillDetailName.value = name
  skillDetailPrompt.value = ''
  skillDetailVisible.value = true
  skillDetailLoading.value = true
  try {
    const detail = await getSkillDetail(name)
    skillDetailPrompt.value = detail.full_prompt || '(无完整指令)'
  } catch {
    skillDetailPrompt.value = '(加载失败)'
  } finally {
    skillDetailLoading.value = false
  }
}

async function onPickFile(event: Event) {
  const target = event.target as HTMLInputElement
  const file = target.files?.[0]
  if (!file) {
    return
  }
  try {
    await uploadMultimodal(file)
    Message.success('文件上传成功')
    multimodalFiles.value = await listMultimodalFiles()
    if (!multimodalAnalyzeForm.id && multimodalFiles.value.length > 0) {
      multimodalAnalyzeForm.id = multimodalFiles.value[multimodalFiles.value.length - 1].id
    }
  } catch (error) {
    Message.error((error as Error).message)
  } finally {
    target.value = ''
  }
}

async function doVectorSearch() {
  try {
    vectorHits.value = await searchVectorMemory({
      agent_id: vectorForm.agent_id.trim(),
      query: vectorForm.query.trim(),
      limit: vectorForm.limit
    })
  } catch (error) {
    Message.error((error as Error).message)
  }
}

async function doRegisterPeer() {
  try {
    await registerA2APeer({
      name: peerForm.name.trim(),
      url: peerForm.url.trim()
    })
    Message.success('A2A 节点已注册')
    peers.value = await listA2APeers()
  } catch (error) {
    Message.error((error as Error).message)
  }
}

async function doDispatch() {
  try {
    let inputs: Record<string, string> = {}
    if (dispatchForm.inputs_raw.trim()) {
      inputs = JSON.parse(dispatchForm.inputs_raw)
    }
    await dispatchA2A({
      peer_url: dispatchForm.peer_url.trim(),
      from: dispatchForm.from.trim(),
      to: dispatchForm.to.trim(),
      task: dispatchForm.task.trim(),
      inputs
    })
    Message.success('A2A 任务已发送')
  } catch (error) {
    Message.error((error as Error).message)
  }
}

async function doSaveCredential() {
  try {
    await saveCredential({
      provider: credentialSaveForm.provider.trim(),
      secret: credentialSaveForm.secret.trim(),
      master_password: credentialSaveForm.master_password.trim()
    })
    Message.success('凭证保存成功')
  } catch (error) {
    Message.error((error as Error).message)
  }
}

async function doRevealCredential() {
  try {
    revealedSecret.value = await revealCredential({
      provider: credentialRevealForm.provider.trim(),
      master_password: credentialRevealForm.master_password.trim()
    })
  } catch (error) {
    Message.error((error as Error).message)
  }
}

async function doSaveApprovalRules() {
  try {
    const parsed = JSON.parse(approvalRulesJSON.value) as ApprovalRule[]
    approvalRules.value = await saveApprovalRules(parsed)
    approvalRulesJSON.value = JSON.stringify(approvalRules.value, null, 2)
    Message.success('审批规则已保存')
  } catch (error) {
    Message.error((error as Error).message)
  }
}

async function doApproveRequest(id: string) {
  try {
    await approveRequest(id)
    approvalRequests.value = await listApprovalRequests()
  } catch (error) {
    Message.error((error as Error).message)
  }
}

async function doRejectRequest(id: string) {
  try {
    await rejectRequest(id)
    approvalRequests.value = await listApprovalRequests()
  } catch (error) {
    Message.error((error as Error).message)
  }
}

async function doAddKnowledgeMount() {
  try {
    await upsertKnowledgeMount({
      name: knowledgeForm.name.trim(),
      type: knowledgeForm.type.trim(),
      source: knowledgeForm.source.trim(),
      token: knowledgeForm.token.trim()
    })
    Message.success('知识库挂载并索引成功')
    knowledgeMounts.value = await listKnowledgeMounts()
  } catch (error) {
    Message.error((error as Error).message)
  }
}

async function doReindexMount(id: string) {
  try {
    await reindexKnowledgeMount(id)
    Message.success('已重新索引')
    knowledgeMounts.value = await listKnowledgeMounts()
  } catch (error) {
    Message.error((error as Error).message)
  }
}

async function doKnowledgeSearch() {
  try {
    knowledgeHits.value = await searchKnowledge({
      query: knowledgeSearchForm.query.trim(),
      limit: knowledgeSearchForm.limit,
      mount_id: knowledgeSearchForm.mount_id.trim()
    })
  } catch (error) {
    Message.error((error as Error).message)
  }
}

async function doMultimodalAnalyze() {
  try {
    const data = await analyzeMultimodal({
      id: multimodalAnalyzeForm.id.trim(),
      prompt: multimodalAnalyzeForm.prompt.trim(),
      model: multimodalAnalyzeForm.model.trim()
    })
    multimodalSummary.value = data.summary
    multimodalMode.value = data.mode
  } catch (error) {
    Message.error((error as Error).message)
  }
}

async function doRenderOutput() {
  try {
    renderOutput.value = await renderMultimodal({
      kind: renderForm.kind,
      title: renderForm.title.trim(),
      prompt: renderForm.prompt.trim()
    })
  } catch (error) {
    Message.error((error as Error).message)
  }
}

async function onAudioPick(event: Event) {
  const target = event.target as HTMLInputElement
  const file = target.files?.[0]
  if (!file) {
    return
  }
  try {
    sttText.value = await speechToText(file)
    Message.success('语音转文字成功')
  } catch (error) {
    Message.error((error as Error).message)
  } finally {
    target.value = ''
  }
}

async function doTTS() {
  try {
    const data = await textToSpeech({
      text: ttsForm.text.trim(),
      voice: ttsForm.voice.trim(),
      model: ttsForm.model.trim()
    })
    ttsAudioSrc.value = `data:${data.mime};base64,${data.audio_base64}`
    Message.success(`TTS 已生成 (${data.bytes} bytes)`)
  } catch (error) {
    Message.error((error as Error).message)
  }
}

async function doSaveRouting() {
  try {
    await setModelRouting({
      enabled: routing.enabled,
      low_complexity_max: routing.low_complexity_max,
      medium_complexity_max: routing.medium_complexity_max,
      hard_keywords: routing.hard_keywords_text
        .split(',')
        .map((v) => v.trim())
        .filter((v) => v)
    })
    Message.success('模型路由规则已保存')
  } catch (error) {
    Message.error((error as Error).message)
  }
}

watch(renderOutput, async (next) => {
  if (!next || next.kind !== 'mermaid') {
    mermaidSVG.value = ''
    return
  }
  try {
    const id = `m-${Date.now()}`
    const rendered = await mermaid.render(id, String(next.content || ''))
    mermaidSVG.value = rendered.svg
  } catch {
    mermaidSVG.value = ''
  }
})

watch(approvalRulesJSON, (val) => {
  const el = jsonEditableRef.value
  if (!el) return
  if (el.innerText !== val) {
    el.innerText = val
  }
})

watch(jsonEditableRef, (el) => {
  if (el) {
    el.innerText = approvalRulesJSON.value
  }
})

onMounted(() => {
  refreshAll()
})
</script>

<template>
  <div class="page">
    <div class="page-header">
      <div>
        <h2>能力中心</h2>
        <p>技能、知识库、模型路由、审批等高级能力配置</p>
      </div>
    </div>

    <a-tabs>
      <a-tab-pane key="skills" title="技能中心">
        <a-alert type="info" style="margin-bottom: 20px">
          <template #icon><icon-info-circle /></template>
          技能系统遵循 <strong>Agent Skills 开放标准</strong>，以 <code>SKILL.md</code>（YAML 元数据 + Markdown 指令）为核心入口，兼容 OpenClaw、ClawHub 等生态（28,000+ 社区技能）。
          <br />
          加载优先级：项目级 > 用户级 > 系统级，支持多层级覆盖与定制。
          <br />
          当前市场目录地址：{{ skillMarketURL || '未配置' }}
        </a-alert>

        <a-card title="已加载技能" :bordered="false" style="margin-bottom: 20px">
          <template #extra>
            <a-space>
              <a-tag color="arcoblue">共 {{ loadedSkills.length }} 个</a-tag>
              <a-button size="small" @click="doReloadSkills">
                <template #icon><icon-refresh /></template>
                重新加载
              </a-button>
            </a-space>
          </template>
          <a-typography-text type="secondary">
            当前运行时已加载的所有技能（含系统级、用户级、项目级），Agent 执行时自动注入摘要，按需加载完整指令。
          </a-typography-text>

          <div class="builtin-skills-grid" style="margin-top: 16px">
            <div class="builtin-skills-grid-header">
              <span class="col-name">技能名称</span>
              <span class="col-version">版本</span>
              <span class="col-level">层级</span>
              <span class="col-desc">描述</span>
              <span class="col-action">操作</span>
            </div>
            <div v-for="item in loadedSkills" :key="item.name" class="builtin-skill-item">
              <span class="builtin-skill-name">{{ item.name }}</span>
              <a-tag size="small">{{ item.version }}</a-tag>
              <a-tag size="small" :color="item.level === 'project' ? 'orangered' : item.level === 'user' ? 'purple' : 'green'">
                {{ item.level === 'project' ? '项目级' : item.level === 'user' ? '用户级' : '系统级' }}
              </a-tag>
              <span class="builtin-skill-desc">{{ item.description || '-' }}</span>
              <a-button size="mini" type="text" @click="doShowSkillDetail(item.name)">查看指令</a-button>
            </div>
            <div v-if="loadedSkills.length === 0" style="padding: 20px; text-align: center; color: var(--color-text-3)">
              暂无已加载技能
            </div>
          </div>
        </a-card>

        <a-card title="市场技能（可安装）" :bordered="false" style="margin-bottom: 20px">
          <template #extra>
            <a-tag color="arcoblue">Agent Skills 标准</a-tag>
          </template>
          <a-typography-text type="secondary">
            搜索并安装符合 Agent Skills 标准的社区技能。安装后通过 <code>SKILL.md</code> 自动注册，Agent 按需渐进加载。
          </a-typography-text>
          <a-space style="margin-top: 12px; margin-bottom: 16px">
            <a-input v-model="skillSearch" placeholder="输入关键词，例如 code / review / monitor" @press-enter="refreshSkillCatalog" />
            <a-button :loading="skillCatalogLoading" type="primary" @click="refreshSkillCatalog">搜索</a-button>
          </a-space>

          <a-list :data="marketSkills" :bordered="false" :loading="skillCatalogLoading">
            <template #empty>
              <a-empty description="没搜到匹配的市场技能，试试换关键词。" />
            </template>
            <template #item="{ item }">
              <a-list-item>
                <div style="width: 100%">
                  <div style="display: flex; align-items: center; justify-content: space-between; margin-bottom: 4px">
                    <a-space>
                      <strong>{{ item.name }}</strong>
                      <a-tag size="small">{{ item.version || 'latest' }}</a-tag>
                      <a-tag size="small" color="arcoblue">{{ item.source || 'market' }}</a-tag>
                    </a-space>
                    <a-button
                      v-if="!installedSkillSet.has(item.name)"
                      size="small"
                      type="primary"
                      @click="doInstallSkill(item)"
                    >
                      安装
                    </a-button>
                    <a-button v-else size="small" status="danger" @click="doUninstallSkill(item.name)">卸载</a-button>
                  </div>
                  <a-typography-text type="secondary" style="font-size: 13px">{{ item.description || '-' }}</a-typography-text>
                </div>
              </a-list-item>
            </template>
          </a-list>
        </a-card>

        <a-card title="显式安装 Skill 包" :bordered="false" style="margin-bottom: 20px">
          <a-typography-text type="secondary">
            可直接从本地路径、压缩包或远程 URL 安装 Skill。安装时会写入锁文件，并自动扫描包内 <code>tools/*.json|yaml|yml</code> 注册 MCP Server。
          </a-typography-text>
          <a-row :gutter="16" style="margin-top: 16px">
            <a-col :xs="24" :sm="8">
              <div class="form-group">
                <label class="form-label">安装来源</label>
                <a-radio-group v-model="skillInstallForm.mode" type="button">
                  <a-radio value="path">本地路径</a-radio>
                  <a-radio value="url">远程 URL</a-radio>
                </a-radio-group>
              </div>
            </a-col>
            <a-col :xs="24" :sm="8">
              <div class="form-group">
                <label class="form-label">技能名（可选）</label>
                <a-input v-model="skillInstallForm.name" placeholder="覆盖安装名，不填则取包内 manifest" />
              </div>
            </a-col>
            <a-col :xs="24" :sm="8">
              <div class="form-group">
                <label class="form-label">版本（可选）</label>
                <a-input v-model="skillInstallForm.version" placeholder="默认使用包内版本" />
              </div>
            </a-col>
          </a-row>
          <div class="form-group">
            <label class="form-label">{{ skillInstallForm.mode === 'path' ? '本地路径' : '远程 URL' }}</label>
            <a-input
              v-if="skillInstallForm.mode === 'path'"
              v-model="skillInstallForm.source_path"
              placeholder="例如 /opt/skills/github-pack 或 /tmp/github-pack.zip"
            />
            <a-input
              v-else
              v-model="skillInstallForm.source_url"
              placeholder="例如 https://example.com/skills/github-pack/skill.json"
            />
          </div>
          <div class="form-group">
            <label class="form-label">SHA256 校验和（可选）</label>
            <a-input v-model="skillInstallForm.checksum" placeholder="支持 sha256:xxxx 或纯 hex" />
          </div>
          <a-space>
            <a-button type="primary" :loading="skillInstallLoading" @click="doInstallSkillFromSource">安装 Skill 包</a-button>
            <a-button @click="skillInstallForm.checksum = ''">清空校验和</a-button>
          </a-space>
          <div v-if="skillChangeReport" style="margin-top: 16px; padding: 14px 16px; border: 1px solid var(--color-border-2); border-radius: 12px; background: var(--color-fill-1)">
            <div style="font-weight: 600; margin-bottom: 8px">
              最近{{ skillChangeReport.action === 'install' ? '安装' : '卸载' }}结果：{{ skillChangeReport.itemName }}
            </div>
            <div style="font-size: 13px; color: var(--color-text-2); margin-bottom: 8px">
              {{ skillChangeReport.action === 'install' ? '关联' : '移除' }} MCP Server {{ skillChangeReport.servers.length }} 个，
              {{ skillChangeReport.action === 'install' ? '发现' : '移除' }}工具 {{ skillChangeReport.tools.length }} 个
            </div>
            <div v-if="skillChangeReport.servers.length" style="margin-bottom: 8px">
              <a-space wrap>
                <a-tag v-for="server in skillChangeReport.servers" :key="server.id" color="arcoblue">
                  {{ server.id }}
                </a-tag>
              </a-space>
            </div>
            <div v-if="skillChangeReport.tools.length">
              <a-space wrap>
                <a-tag v-for="tool in skillChangeReport.tools" :key="tool.full_name">
                  {{ tool.full_name }}
                </a-tag>
              </a-space>
            </div>
          </div>
        </a-card>

        <a-card title="内置技能（系统级）" :bordered="false" style="margin-bottom: 20px">
          <template #extra>
            <a-tag color="green">始终可用</a-tag>
          </template>
          <a-typography-text type="secondary">
            系统级技能随运行时提供，优先级最低，可被用户级或项目级同名技能覆盖。遵循 <code>SKILL.md</code> 标准格式。
          </a-typography-text>

          <div class="builtin-skills-grid" style="margin-top: 16px">
            <div class="builtin-skills-grid-header">
              <span class="col-name">技能名称</span>
              <span class="col-version">版本</span>
              <span class="col-level">层级</span>
              <span class="col-desc">描述</span>
            </div>
            <div v-for="item in builtinSkills" :key="item.name" class="builtin-skill-item">
              <span class="builtin-skill-name">{{ item.name }}</span>
              <a-tag size="small">{{ item.version || 'latest' }}</a-tag>
              <a-tag size="small" color="green">系统级</a-tag>
              <span class="builtin-skill-desc">{{ item.description || '-' }}</span>
            </div>
          </div>
        </a-card>

        <a-card title="已安装的市场技能" :bordered="false">
          <template #extra>
            <a-tag color="purple">用户级</a-tag>
          </template>
          <a-typography-text type="secondary">
            已安装的社区技能，以用户级加载，优先级高于系统级内置技能。
          </a-typography-text>
          <a-list :data="installedMarketSkills" :bordered="false" style="margin-top: 12px">
            <template #empty>
              <a-empty description="暂无已安装市场技能" />
            </template>
            <template #item="{ item }">
              <a-list-item>
                <div style="display: flex; align-items: flex-start; justify-content: space-between; width: 100%; gap: 16px">
                  <div style="min-width: 0">
                    <a-space wrap>
                      <strong>{{ item.name }}</strong>
                      <a-tag size="small">{{ item.version }}</a-tag>
                      <a-tag size="small" color="purple">用户级</a-tag>
                      <a-tag size="small">{{ item.source }}</a-tag>
                    </a-space>
                    <div style="margin-top: 6px; color: var(--color-text-2); font-size: 12px">
                      安装时间：{{ formatInstalledAt(item.installed_at) }}
                    </div>
                    <div v-if="item.resolved_from" style="margin-top: 4px; color: var(--color-text-2); font-size: 12px; word-break: break-all">
                      来源：{{ item.resolved_from }}
                    </div>
                    <div v-if="item.integrity" style="margin-top: 4px; color: var(--color-text-2); font-size: 12px; word-break: break-all">
                      完整性：{{ item.integrity }}
                    </div>
                  </div>
                  <a-space>
                    <a-button size="mini" type="text" @click="doShowSkillDetail(item.name)">查看指令</a-button>
                    <a-button size="small" status="danger" @click="doUninstallSkill(item.name)">卸载</a-button>
                  </a-space>
                </div>
              </a-list-item>
            </template>
          </a-list>
        </a-card>

        <a-card title="SKILL.md 标准格式参考" :bordered="false" style="margin-top: 20px">
          <a-typography-text type="secondary">
            每个技能目录以 <code>SKILL.md</code> 为入口，遵循 Agent Skills 开放标准。标准目录结构如下：
          </a-typography-text>
          <div class="skill-standard-ref" style="margin-top: 16px">
            <div class="skill-standard-block">
              <div class="skill-standard-title">目录结构</div>
              <pre class="skill-standard-code">my-skill/
├── SKILL.md          # 入口：YAML 元数据 + Markdown 指令
├── scripts/          # 可选：辅助脚本
├── references/       # 可选：参考资料
└── assets/           # 可选：静态资源</pre>
            </div>
            <div class="skill-standard-block">
              <div class="skill-standard-title">SKILL.md 示例</div>
              <pre class="skill-standard-code">---
name: code-review
description: 代码审查技能，自动检测代码质量问题
version: 1.0.0
allowed-tools:
  - file_read
  - shell_exec
---

## 指令

请对目标代码进行规范审查，输出问题列表和改进建议。</pre>
            </div>
            <div class="skill-standard-block">
              <div class="skill-standard-title">加载优先级</div>
              <div class="skill-priority-list">
                <div class="skill-priority-item">
                  <a-tag color="orangered" size="small">项目级</a-tag>
                  <span>.xclaw/skills/ — 项目级技能覆盖目录</span>
                </div>
                <div class="skill-priority-item">
                  <a-tag color="purple" size="small">用户级</a-tag>
                  <span>xclaw 根目录下的 skills/ — 用户与内置技能目录</span>
                </div>
                <div class="skill-priority-item">
                  <a-tag color="green" size="small">系统级</a-tag>
                  <span>内置技能 — 最低优先级，可被覆盖</span>
                </div>
              </div>
            </div>
          </div>
        </a-card>
      </a-tab-pane>

      <a-tab-pane key="memory" title="向量记忆">
        <a-card :bordered="false">
          <a-row :gutter="16">
            <a-col :xs="24" :sm="8">
              <div class="form-group">
                <label class="form-label">Agent ID（可选）</label>
                <a-input v-model="vectorForm.agent_id" placeholder="Agent ID" size="large" />
              </div>
            </a-col>
            <a-col :xs="24" :sm="12">
              <div class="form-group">
                <label class="form-label">语义查询内容</label>
                <a-input v-model="vectorForm.query" placeholder="输入查询内容" size="large" />
              </div>
            </a-col>
            <a-col :xs="24" :sm="4">
              <div class="form-group">
                <label class="form-label">返回数量</label>
                <a-input-number v-model="vectorForm.limit" :min="1" :max="20" size="large" style="width: 100%" />
              </div>
            </a-col>
          </a-row>
          <a-button type="primary" @click="doVectorSearch" size="large">向量检索</a-button>

          <a-list :data="vectorHits" :bordered="false" style="margin-top: 20px">
            <template #item="{ item }">
              <a-list-item>
                <div style="width: 100%">
                  <a-space style="margin-bottom: 4px">
                    <a-tag size="small" color="arcoblue">distance={{ item.distance.toFixed(4) }}</a-tag>
                    <a-typography-text type="secondary" style="font-size: 12px">{{ item.agent_id }} / {{ item.session_id }}</a-typography-text>
                  </a-space>
                  <a-typography-text style="font-size: 13px">{{ item.content }}</a-typography-text>
                </div>
              </a-list-item>
            </template>
          </a-list>
        </a-card>
      </a-tab-pane>

      <a-tab-pane key="knowledge" title="知识库挂载">
        <a-alert type="info" style="margin-bottom: 20px">
          把外部资料接入系统供 Agent 检索。流程是：填写来源 -> 建立索引 -> 对话时自动召回。
        </a-alert>

        <a-card title="新增知识库" :bordered="false" style="margin-bottom: 20px">
          <a-row :gutter="16">
            <a-col :xs="24" :sm="12">
              <div class="form-group">
                <label class="form-label">挂载名称</label>
                <a-input v-model="knowledgeForm.name" placeholder="例如：产品文档库 / 运维手册" size="large" />
              </div>
            </a-col>
            <a-col :xs="24" :sm="12">
              <div class="form-group">
                <label class="form-label">资料类型</label>
                <a-select v-model="knowledgeForm.type" placeholder="选择资料类型" size="large">
                  <a-option value="local">本地目录（local）</a-option>
                  <a-option value="git">Git 仓库（git）</a-option>
                  <a-option value="web">网页链接（web）</a-option>
                  <a-option value="notion">Notion（notion）</a-option>
                </a-select>
              </div>
            </a-col>
          </a-row>

          <div class="form-group">
            <label class="form-label">资料来源</label>
            <a-input v-model="knowledgeForm.source" :placeholder="knowledgeSourcePlaceholder" size="large" />
            <div class="form-hint">{{ knowledgeSourceHint }}</div>
            <a-button size="small" @click="applyKnowledgeExample" style="margin-top: 4px">填入示例</a-button>
          </div>

          <div class="form-group">
            <label class="form-label">Notion Token（仅 notion 类型需要）</label>
            <a-input-password v-model="knowledgeForm.token" placeholder="例如 secret_xxx" size="large" />
          </div>

          <a-button type="primary" @click="doAddKnowledgeMount" size="large">新增并索引</a-button>
        </a-card>

        <a-card title="已挂载知识库" :bordered="false" style="margin-bottom: 20px">
          <a-list :data="knowledgeMounts" :bordered="false">
            <template #item="{ item }">
              <a-list-item>
                <div style="width: 100%">
                  <div style="display: flex; align-items: center; justify-content: space-between; margin-bottom: 6px">
                    <a-space>
                      <strong>{{ item.name }}</strong>
                      <a-tag size="small">{{ item.type }}</a-tag>
                      <a-tag size="small" color="green">chunks={{ item.indexed_chunks }}</a-tag>
                      <a-tag size="small">{{ item.status }}</a-tag>
                    </a-space>
                    <a-button size="small" @click="doReindexMount(item.id)">重建索引</a-button>
                  </div>
                  <a-typography-text type="secondary" style="font-size: 12px">{{ item.source }}</a-typography-text>
                  <a-typography-text v-if="item.last_error" type="danger" style="font-size: 12px; display: block; margin-top: 4px">
                    {{ item.last_error }}
                  </a-typography-text>
                </div>
              </a-list-item>
            </template>
          </a-list>
        </a-card>

        <a-card title="知识库查询" :bordered="false">
          <a-row :gutter="16">
            <a-col :xs="24" :sm="8">
              <div class="form-group">
                <label class="form-label">Mount ID（可选过滤）</label>
                <a-input v-model="knowledgeSearchForm.mount_id" placeholder="可选：mount id 精确过滤" size="large" />
              </div>
            </a-col>
            <a-col :xs="24" :sm="12">
              <div class="form-group">
                <label class="form-label">查询内容</label>
                <a-input v-model="knowledgeSearchForm.query" placeholder="知识库查询内容" size="large" />
              </div>
            </a-col>
            <a-col :xs="24" :sm="4">
              <div class="form-group">
                <label class="form-label">返回数量</label>
                <a-input-number v-model="knowledgeSearchForm.limit" :min="1" :max="20" size="large" style="width: 100%" />
              </div>
            </a-col>
          </a-row>
          <a-button type="primary" @click="doKnowledgeSearch" size="large">查询知识库</a-button>

          <a-list :data="knowledgeHits" :bordered="false" style="margin-top: 20px">
            <template #item="{ item }">
              <a-list-item>
                <div style="width: 100%">
                  <a-tag size="small" color="arcoblue" style="margin-bottom: 4px">{{ item.agent_id }}</a-tag>
                  <a-typography-text style="font-size: 13px; display: block">{{ item.content }}</a-typography-text>
                </div>
              </a-list-item>
            </template>
          </a-list>
        </a-card>
      </a-tab-pane>

      <a-tab-pane key="multimodal" title="多模态">
        <a-card title="文件上传" :bordered="false" style="margin-bottom: 20px">
          <a-alert type="info" style="margin-bottom: 16px">
            当前支持图片、PDF、Word、Excel、PowerPoint，以及代码/文档压缩包（`.zip`、`.tar.gz`、`.tgz`）上传与解析。
          </a-alert>
          <div class="form-group">
            <label class="form-label">上传文件</label>
            <input type="file" @change="onPickFile" style="padding: 8px 0" />
          </div>
          <a-list :data="multimodalFiles" :bordered="false">
            <template #item="{ item }">
              <a-list-item>
                <div style="width: 100%">
                  <strong>{{ item.name }}</strong>
                  <a-typography-text type="secondary" style="font-size: 12px; display: block">
                    {{ item.id }} | {{ item.mime }} | {{ item.size }} bytes
                  </a-typography-text>
                </div>
              </a-list-item>
            </template>
          </a-list>
        </a-card>

        <a-card title="文件分析" :bordered="false" style="margin-bottom: 20px">
          <a-row :gutter="16">
            <a-col :xs="24" :sm="8">
              <div class="form-group">
                <label class="form-label">文件 ID</label>
                <a-input v-model="multimodalAnalyzeForm.id" placeholder="选择文件 ID 进行分析" size="large" />
              </div>
            </a-col>
            <a-col :xs="24" :sm="16">
              <div class="form-group">
                <label class="form-label">视觉模型（可选）</label>
                <a-input v-model="multimodalAnalyzeForm.model" placeholder="例如 gpt-4o-mini" size="large" />
              </div>
            </a-col>
          </a-row>
          <div class="form-group">
            <label class="form-label">分析提示词</label>
            <a-textarea v-model="multimodalAnalyzeForm.prompt" :auto-size="{ minRows: 2, maxRows: 5 }" />
          </div>
          <a-button type="primary" @click="doMultimodalAnalyze" size="large">分析文件</a-button>

          <a-alert v-if="multimodalSummary" type="success" style="margin-top: 16px">模式：{{ multimodalMode }}</a-alert>
          <a-textarea v-if="multimodalSummary" :model-value="multimodalSummary" :auto-size="{ minRows: 4, maxRows: 10 }" readonly style="margin-top: 12px" />
        </a-card>

        <a-card title="渲染输出" :bordered="false">
          <a-row :gutter="16">
            <a-col :xs="24" :sm="8">
              <div class="form-group">
                <label class="form-label">渲染类型</label>
                <a-select v-model="renderForm.kind" size="large">
                  <a-option value="mermaid">Mermaid</a-option>
                  <a-option value="echarts">ECharts</a-option>
                  <a-option value="html">HTML</a-option>
                  <a-option value="image">Image</a-option>
                </a-select>
              </div>
            </a-col>
            <a-col :xs="24" :sm="16">
              <div class="form-group">
                <label class="form-label">标题</label>
                <a-input v-model="renderForm.title" placeholder="标题" size="large" />
              </div>
            </a-col>
          </a-row>
          <div class="form-group">
            <label class="form-label">渲染输入</label>
            <a-textarea v-model="renderForm.prompt" :auto-size="{ minRows: 2, maxRows: 5 }" />
          </div>
          <a-button type="primary" @click="doRenderOutput" size="large">生成多模态输出</a-button>

          <div v-if="renderOutput?.kind === 'mermaid' && mermaidSVG" v-html="mermaidSVG" class="mermaid-preview" style="margin-top: 16px" />
          <a-textarea
            v-else-if="renderOutput?.kind === 'echarts'"
            :model-value="JSON.stringify(renderOutput.content, null, 2)"
            :auto-size="{ minRows: 4, maxRows: 12 }"
            readonly
            style="margin-top: 16px"
          />
          <iframe
            v-else-if="renderOutput?.kind === 'html'"
            :srcdoc="String(renderOutput.content || '')"
            style="width: 100%; min-height: 240px; border: 1px solid var(--line-0); border-radius: 12px; margin-top: 16px"
          />
          <div v-else-if="renderOutput?.kind === 'image'" style="margin-top: 16px">
            <img
              v-if="renderOutput.content?.url"
              :src="String(renderOutput.content?.url || '')"
              :alt="renderOutput.title || 'Generated image'"
              style="display: block; max-width: 100%; border-radius: 16px; border: 1px solid var(--line-0)"
            />
            <a-alert v-if="renderOutput.content?.revised_prompt" type="info" style="margin-top: 12px">
              最终提示词：{{ renderOutput.content?.revised_prompt }}
            </a-alert>
            <a-link v-if="renderOutput.content?.url" :href="String(renderOutput.content?.url || '')" download="xclaw-generated-image.png">
              下载图片
            </a-link>
          </div>
        </a-card>
      </a-tab-pane>

      <a-tab-pane key="voice" title="语音 STT/TTS">
        <a-alert type="warning" style="margin-bottom: 20px">需要配置 OPENAI_API_KEY 或在请求中带 api_key。</a-alert>

        <a-card title="语音转文字（STT）" :bordered="false" style="margin-bottom: 20px">
          <div class="form-group">
            <label class="form-label">上传音频文件</label>
            <input type="file" accept="audio/*" @change="onAudioPick" style="padding: 8px 0" />
          </div>
          <div class="form-group">
            <label class="form-label">转写结果</label>
            <a-textarea v-model="sttText" :auto-size="{ minRows: 3, maxRows: 8 }" placeholder="转写结果将显示在这里" />
          </div>
        </a-card>

        <a-card title="文字转语音（TTS）" :bordered="false">
          <div class="form-group">
            <label class="form-label">输入文本</label>
            <a-textarea v-model="ttsForm.text" :auto-size="{ minRows: 3, maxRows: 8 }" placeholder="输入要合成语音的文本" />
          </div>
          <a-row :gutter="16">
            <a-col :xs="24" :sm="12">
              <div class="form-group">
                <label class="form-label">Voice</label>
                <a-input v-model="ttsForm.voice" placeholder="例如 alloy" size="large" />
              </div>
            </a-col>
            <a-col :xs="24" :sm="12">
              <div class="form-group">
                <label class="form-label">Model</label>
                <a-input v-model="ttsForm.model" placeholder="例如 gpt-4o-mini-tts" size="large" />
              </div>
            </a-col>
          </a-row>
          <a-button type="primary" @click="doTTS" size="large">生成语音</a-button>
          <audio v-if="ttsAudioSrc" :src="ttsAudioSrc" controls style="width: 100%; margin-top: 16px" />
        </a-card>
      </a-tab-pane>

      <a-tab-pane key="approval" title="审批中心">
        <a-alert type="info" style="margin-bottom: 20px">
          控制高风险操作是否要人工确认。普通用户建议先用默认规则；需要精细控制时再编辑 JSON。
        </a-alert>

        <a-card title="审批规则（高级 JSON）" :bordered="false" style="margin-bottom: 20px">
          <div class="json-editor-toolbar">
            <div class="json-editor-toolbar-left">
              <span class="json-editor-label">规则 JSON</span>
              <a-tag v-if="jsonValid" size="small" color="green">格式合法</a-tag>
              <a-tag v-else size="small" color="red">格式错误</a-tag>
            </div>
            <a-space>
              <a-button size="small" @click="formatJSON">
                <template #icon>✨</template>
                格式化
              </a-button>
            </a-space>
          </div>

          <div class="json-editor" :class="{ 'json-editor--invalid': !jsonValid }">
            <div class="json-editor-container" ref="jsonEditorRef">
              <pre class="json-editor-highlight"><code v-html="jsonHighlighted"></code></pre>
              <div
                class="json-editor-contenteditable"
                ref="jsonEditableRef"
                contenteditable="true"
                spellcheck="false"
                @input="onJsonInput"
                @scroll="syncJsonScroll"
                @keydown="onJsonKeydown"
              ></div>
            </div>
          </div>

          <div style="margin-top: 16px">
            <a-button type="primary" @click="doSaveApprovalRules" size="large">保存规则</a-button>
          </div>
        </a-card>

        <a-card title="待处理请求" :bordered="false">
          <a-list :data="approvalRequests" :bordered="false">
            <template #item="{ item }">
              <a-list-item>
                <div style="width: 100%">
                  <div style="display: flex; align-items: center; justify-content: space-between; margin-bottom: 6px">
                    <a-space>
                      <strong>{{ item.id }}</strong>
                      <a-tag size="small" :color="item.status === 'pending' ? 'orange' : 'green'">{{ item.status }}</a-tag>
                      <a-tag size="small" color="arcoblue">{{ item.tool }}</a-tag>
                    </a-space>
                    <a-space>
                      <a-button size="small" type="primary" @click="doApproveRequest(item.id)">通过</a-button>
                      <a-button size="small" status="danger" @click="doRejectRequest(item.id)">拒绝</a-button>
                    </a-space>
                  </div>
                  <a-typography-text style="font-size: 13px">{{ item.reason }}</a-typography-text>
                  <a-typography-text type="secondary" style="font-size: 12px; display: block; margin-top: 4px">
                    strategy={{ item.strategy }} | expires={{ item.expires_at }}
                  </a-typography-text>
                </div>
              </a-list-item>
            </template>
          </a-list>
        </a-card>
      </a-tab-pane>

      <a-tab-pane key="routing" title="模型路由">
        <a-alert type="info" style="margin-bottom: 20px">
          系统会先评估问题复杂度，再自动选模型档位。分档规则：低复杂度 -> mini，中复杂度 -> standard，高复杂度 -> advanced。
        </a-alert>

        <a-card :bordered="false">
          <div class="form-group">
            <a-space>
              <a-typography-text style="font-weight: 500">启用自动路由</a-typography-text>
              <a-switch v-model="routing.enabled" />
            </a-space>
          </div>

          <a-row :gutter="16">
            <a-col :xs="24" :sm="12">
              <div class="form-group">
                <label class="form-label">低复杂度阈值（推荐 8）</label>
                <a-input-number v-model="routing.low_complexity_max" :min="1" :max="50" size="large" style="width: 100%" />
                <div class="form-hint">评分小于等于这个值，走 mini（更省钱、更快）</div>
              </div>
            </a-col>
            <a-col :xs="24" :sm="12">
              <div class="form-group">
                <label class="form-label">中复杂度阈值（推荐 18）</label>
                <a-input-number v-model="routing.medium_complexity_max" :min="2" :max="80" size="large" style="width: 100%" />
                <div class="form-hint">评分在 (低阈值, 中阈值] 走 standard；超过中阈值走 advanced</div>
              </div>
            </a-col>
          </a-row>

          <a-alert type="normal" style="margin-bottom: 16px">
            示例：复杂度评分 6 -> mini；评分 12 -> standard；评分 25 -> advanced。
          </a-alert>

          <div class="form-group">
            <label class="form-label">高难关键词（逗号分隔）</label>
            <a-textarea
              v-model="routing.hard_keywords_text"
              :auto-size="{ minRows: 2, maxRows: 6 }"
              placeholder="例如：架构,重构,并发,安全,优化,多步骤,多代理,算法,benchmark,debug,incident,root cause,distributed"
            />
            <div class="form-hint">问题里命中这些词会加分，更容易走高档模型</div>
          </div>

          <a-button type="primary" @click="doSaveRouting" size="large">保存路由配置</a-button>
        </a-card>
      </a-tab-pane>

      <a-tab-pane key="a2a" title="A2A 协作">
        <a-alert type="info" style="margin-bottom: 20px">把本实例和其它实例连接起来，实现跨实例转发任务。</a-alert>

        <a-card title="注册节点" :bordered="false" style="margin-bottom: 20px">
          <a-row :gutter="16">
            <a-col :xs="24" :sm="12">
              <div class="form-group">
                <label class="form-label">节点名称</label>
                <a-input v-model="peerForm.name" placeholder="例如 上海节点 / 测试环境" size="large" />
              </div>
            </a-col>
            <a-col :xs="24" :sm="12">
              <div class="form-group">
                <label class="form-label">节点地址</label>
                <a-input v-model="peerForm.url" placeholder="例如 http://127.0.0.1:5311" size="large" />
              </div>
            </a-col>
          </a-row>
          <a-button type="primary" @click="doRegisterPeer" size="large">注册节点</a-button>

          <a-list :data="peers" :bordered="false" style="margin-top: 20px">
            <template #item="{ item }">
              <a-list-item>
                <div style="display: flex; align-items: center; justify-content: space-between; width: 100%">
                  <a-space>
                    <strong>{{ item.name }}</strong>
                    <a-typography-text type="secondary">{{ item.url }}</a-typography-text>
                  </a-space>
                  <a-tag size="small">{{ item.source || 'manual' }}</a-tag>
                </div>
              </a-list-item>
            </template>
          </a-list>
        </a-card>

        <a-card title="派发任务" :bordered="false">
          <a-row :gutter="16">
            <a-col :xs="24" :sm="12">
              <div class="form-group">
                <label class="form-label">目标节点地址</label>
                <a-input v-model="dispatchForm.peer_url" placeholder="和上面注册的 URL 一致" size="large" />
              </div>
            </a-col>
            <a-col :xs="24" :sm="6">
              <div class="form-group">
                <label class="form-label">发送方标识</label>
                <a-input v-model="dispatchForm.from" placeholder="xclaw-local" size="large" />
              </div>
            </a-col>
            <a-col :xs="24" :sm="6">
              <div class="form-group">
                <label class="form-label">接收方标识</label>
                <a-input v-model="dispatchForm.to" placeholder="remote-agent" size="large" />
              </div>
            </a-col>
          </a-row>
          <div class="form-group">
            <label class="form-label">任务描述</label>
            <a-textarea v-model="dispatchForm.task" :auto-size="{ minRows: 2, maxRows: 5 }" placeholder="要对方执行的任务描述" />
          </div>
          <div class="form-group">
            <label class="form-label">可选参数（JSON）</label>
            <a-textarea v-model="dispatchForm.inputs_raw" :auto-size="{ minRows: 2, maxRows: 5 }" placeholder='{"agent_id":"agent-xxx"}' />
          </div>
          <a-button type="primary" @click="doDispatch" size="large">发送 A2A 任务</a-button>
        </a-card>
      </a-tab-pane>

      <a-tab-pane key="credential" title="凭证管理">
        <a-alert type="info" style="margin-bottom: 20px">保存第三方服务密钥（例如 OpenAI Key）。只有管理员密码正确时才能查看明文。</a-alert>

        <a-card title="保存凭证" :bordered="false" style="margin-bottom: 20px">
          <a-row :gutter="16">
            <a-col :xs="24" :sm="12">
              <div class="form-group">
                <label class="form-label">服务商名称</label>
                <a-input v-model="credentialSaveForm.provider" placeholder="例如 openai / anthropic / deepseek" size="large" />
              </div>
            </a-col>
            <a-col :xs="24" :sm="12">
              <div class="form-group">
                <label class="form-label">管理员密码</label>
                <a-input-password v-model="credentialSaveForm.master_password" placeholder="用于加密保存" size="large" />
              </div>
            </a-col>
          </a-row>
          <div class="form-group">
            <label class="form-label">API Key 或 Secret</label>
            <a-input-password v-model="credentialSaveForm.secret" placeholder="API Key 或 Secret" size="large" />
          </div>
          <a-button type="primary" @click="doSaveCredential" size="large">保存凭证</a-button>
        </a-card>

        <a-card title="查看凭证" :bordered="false">
          <a-row :gutter="16">
            <a-col :xs="24" :sm="12">
              <div class="form-group">
                <label class="form-label">服务商名称</label>
                <a-input v-model="credentialRevealForm.provider" placeholder="要查看的服务商名称" size="large" />
              </div>
            </a-col>
            <a-col :xs="24" :sm="12">
              <div class="form-group">
                <label class="form-label">管理员密码</label>
                <a-input-password v-model="credentialRevealForm.master_password" placeholder="用于解密" size="large" />
              </div>
            </a-col>
          </a-row>
          <a-button @click="doRevealCredential" size="large" style="margin-bottom: 16px">解密查看</a-button>
          <a-textarea :model-value="revealedSecret" :auto-size="{ minRows: 2, maxRows: 6 }" readonly placeholder="解密后的凭证将显示在这里" />
        </a-card>
      </a-tab-pane>
    </a-tabs>

    <a-drawer
      :visible="skillDetailVisible"
      :width="560"
      :title="'技能指令: ' + skillDetailName"
      @cancel="skillDetailVisible = false"
      unmountOnClose
    >
      <a-spin :loading="skillDetailLoading" style="width: 100%">
        <pre style="white-space: pre-wrap; word-break: break-word; font-size: 13px; line-height: 1.7; background: #1e1e1e; color: #d4d4d4; padding: 16px; border-radius: 8px; margin: 0">{{ skillDetailPrompt }}</pre>
      </a-spin>
    </a-drawer>
  </div>
</template>

<style scoped>
.mermaid-preview {
  border: 1px solid var(--line-0);
  border-radius: 12px;
  padding: 16px;
  overflow: auto;
  background: var(--bg-1);
}

.builtin-skills-grid {
  display: flex;
  flex-direction: column;
  gap: 0;
}

.builtin-skills-grid-header {
  display: grid;
  grid-template-columns: 200px 80px 72px 1fr 80px;
  align-items: center;
  padding: 10px 16px;
  background: var(--bg-1);
  border-bottom: 2px solid var(--line-1);
  font-size: 12px;
  font-weight: 600;
  color: var(--text-2);
  text-transform: uppercase;
  letter-spacing: 0.05em;
}

.builtin-skill-item {
  display: grid;
  grid-template-columns: 200px 80px 72px 1fr 80px;
  align-items: center;
  padding: 14px 16px;
  border-bottom: 1px solid var(--line-0);
  transition: background 0.15s;
}

.builtin-skill-item:last-child {
  border-bottom: none;
}

.builtin-skill-item:hover {
  background: var(--accent-soft);
}

.builtin-skill-name {
  font-weight: 600;
  font-size: 14px;
  color: var(--text-0);
  font-family: 'JetBrains Mono', 'Fira Code', 'Consolas', monospace;
}

.builtin-skill-desc {
  grid-column: 4;
  font-size: 13px;
  color: var(--text-2);
}

.skill-standard-ref {
  display: grid;
  grid-template-columns: repeat(3, 1fr);
  gap: 16px;
}

.skill-standard-block {
  border: 1px solid var(--line-0);
  border-radius: var(--radius-md);
  overflow: hidden;
}

.skill-standard-title {
  padding: 10px 16px;
  background: var(--bg-1);
  font-weight: 600;
  font-size: 13px;
  color: var(--text-1);
  border-bottom: 1px solid var(--line-0);
}

.skill-standard-code {
  margin: 0;
  padding: 14px 16px;
  background: #1e1e1e;
  color: #d4d4d4;
  font-family: 'JetBrains Mono', 'Fira Code', 'Consolas', monospace;
  font-size: 12px;
  line-height: 1.6;
  overflow-x: auto;
}

.skill-priority-list {
  padding: 12px 16px;
  display: flex;
  flex-direction: column;
  gap: 10px;
}

.skill-priority-item {
  display: flex;
  align-items: center;
  gap: 10px;
  font-size: 13px;
  color: var(--text-1);
}

@media (max-width: 900px) {
  .skill-standard-ref {
    grid-template-columns: 1fr;
  }
}

.json-editor-toolbar {
  display: flex;
  align-items: center;
  justify-content: space-between;
  margin-bottom: 12px;
}

.json-editor-toolbar-left {
  display: flex;
  align-items: center;
  gap: 10px;
}

.json-editor-label {
  font-weight: 500;
  font-size: 14px;
  color: var(--text-0);
}

.json-editor {
  position: relative;
  border: 1px solid var(--line-1);
  border-radius: var(--radius-md);
  background: #1e1e1e;
  overflow: hidden;
  transition: border-color 0.2s;
}

.json-editor--invalid {
  border-color: #ef4444;
}

.json-editor:hover {
  border-color: #6366f1;
}

.json-editor-container {
  position: relative;
  min-height: 320px;
  max-height: 480px;
}

.json-editor-textarea {
  position: absolute;
  top: 0;
  left: 0;
  width: 100%;
  height: 100%;
  min-height: 320px;
  padding: 16px 20px;
  border: none;
  outline: none;
  resize: none;
  background: transparent;
  color: transparent;
  caret-color: #f8fafc;
  font-family: 'JetBrains Mono', 'Fira Code', 'Consolas', monospace;
  font-size: 13px;
  line-height: 1.7;
  white-space: pre;
  overflow: auto;
  z-index: 2;
}

.json-editor-contenteditable {
  position: absolute;
  top: 0;
  left: 0;
  width: 100%;
  height: 100%;
  min-height: 320px;
  padding: 16px 20px;
  border: none;
  outline: none;
  background: transparent;
  color: transparent;
  caret-color: #f8fafc;
  font-family: 'JetBrains Mono', 'Fira Code', 'Consolas', monospace;
  font-size: 13px;
  line-height: 1.7;
  white-space: pre;
  overflow: auto;
  z-index: 2;
  word-break: break-all;
}

.json-editor-contenteditable:focus {
  outline: none;
}

.json-editor-highlight {
  position: absolute;
  top: 0;
  left: 0;
  width: 100%;
  height: 100%;
  margin: 0;
  padding: 16px 20px;
  font-family: 'JetBrains Mono', 'Fira Code', 'Consolas', monospace;
  font-size: 13px;
  line-height: 1.7;
  white-space: pre;
  overflow: auto;
  background: transparent;
  pointer-events: none;
}

.json-editor-highlight code {
  font-family: inherit;
  font-size: inherit;
  line-height: inherit;
  background: transparent !important;
}
</style>
