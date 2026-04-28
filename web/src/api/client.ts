import axios from 'axios'

const client = axios.create({
  baseURL: '/api',
  timeout: 30000
})

client.interceptors.request.use((req) => {
  const token = localStorage.getItem('xclaw_token')
  if (token) {
    req.headers.Authorization = `Bearer ${token}`
  }
  return req
})

export type SystemStatus = {
  bootstrapped: boolean
  agents_count: number
  version?: string
  draining?: boolean
  active_requests?: number
  active_sse?: number
  active_ws?: number
  running_sessions?: number
  recovering_sessions?: number
  server: { host: string; port: number; tls: boolean }
  sandbox: { mode: string; workspace_access: string; scope: string }
  vector?: VectorStatus
}

export type Agent = {
  id: string
  name: string
  emoji: string
  description: string
  system_instruction: string
  model_provider: string
  model_name: string
  workspace_path: string
  tools: string[]
  created_at: string
  updated_at: string
}

export type Session = {
  id: string
  agent_id: string
  title: string
  is_main: boolean
  status: string
  created_at: string
  updated_at: string
}

export type Message = {
  id: string
  session_id: string
  role: string
  content: string
  metadata: string
  created_at: string
}

export type ChatAttachmentInput = {
  id?: string
  kind?: 'image' | 'file' | 'audio'
  name?: string
  url?: string
  mime?: string
  size?: number
  duration_sec?: number
  transcript?: string
}

export type ChatPollOptionInput = {
  id?: string
  label: string
  votes?: number
}

export type ChatPollInput = {
  question: string
  options: ChatPollOptionInput[]
}

export type SendMessagePayload = {
  content: string
  auto_approve: boolean
  reply_to_id?: string
  attachments?: ChatAttachmentInput[]
  poll?: ChatPollInput
  metadata?: Record<string, unknown>
}

export type CronJob = {
  id: string
  agent_id: string
  name: string
  schedule: string
  job_type: string
  payload: string
  enabled: boolean
  retry_limit: number
  last_status: string
  last_error: string
  updated_at: string
}

export type AuditLog = {
  id: number
  agent_id: string
  session_id: string
  category: string
  action: string
  detail: string
  created_at: string
}

export type VectorStatus = {
  enabled: boolean
  hnsw_supported: boolean
  hnsw_support_note: string
  dimension: number
  engine: string
  active_index_strategy: string
}

export type VectorMemoryHit = {
  row_id: number
  agent_id: string
  session_id: string
  content: string
  distance: number
  created_at: string
}

export type SkillItem = {
  name: string
  version: string
  description?: string
  source: string
  skill_url?: string
  resolved_from?: string
  integrity?: string
  installed_at?: string
}

export type LoadedSkill = {
  name: string
  version: string
  description: string
  level: 'system' | 'user' | 'project'
  path: string
  allowed_tools?: string[]
  author?: string
  tags?: string[]
  trigger_hints?: string[]
}

export type SkillDetail = {
  skill: LoadedSkill
  full_prompt: string
}

export type MCPTool = {
  server_id: string
  server_name: string
  name: string
  full_name: string
  description: string
  risk: string
  transport: string
  last_error?: string
  last_sync_at?: string
  available: boolean
}

export type MCPServer = {
  id: string
  name: string
  transport: string
  url?: string
  command?: string
  args?: string[]
  env?: Record<string, string>
  enabled: boolean
  timeout_sec?: number
  managed_by?: string
  readonly?: boolean
}

export type InstallSkillResult = {
  item: SkillItem
  registered_mcp_servers?: MCPServer[]
}

export type UninstallSkillResult = {
  ok: boolean
  removed_mcp_servers?: MCPServer[]
  removed_mcp_tools?: string[]
}

export type MultimodalFile = {
  id: string
  name: string
  stored_name: string
  path: string
  mime: string
  size: number
  uploaded_at: string
  kind?: string
}

export type A2APeer = {
  name: string
  url: string
  source?: string
}

export type ApprovalRule = {
  id: string
  name: string
  enabled: boolean
  tools?: string[]
  risks?: string[]
  path_prefixes?: string[]
  command_patterns?: string[]
  impact_min?: number
  strategy: 'single' | 'all' | 'any'
  approvers?: string[]
  expires_in_sec: number
  reason?: string
}

export type ApprovalRequest = {
  id: string
  rule_id: string
  tool: string
  risk: string
  params: Record<string, string>
  reason: string
  strategy: string
  approvers?: string[]
  approved_by?: string[]
  rejected_by?: string[]
  status: string
  created_at: string
  expires_at: string
}

export type KnowledgeMount = {
  id: string
  name: string
  type: string
  source: string
  token?: string
  status: string
  last_error?: string
  indexed_chunks: number
  last_indexed_at: string
  updated_at: string
}

export type GatewayConfig = {
  default_target: string
  fallback_targets: string[]
  quiet_hours_start: string
  quiet_hours_end: string
}

export type GatewayProviderConfig = {
  name: string
  protocol: string
  enabled: boolean
  settings: Record<string, string>
}

export type GatewayProviderHealth = {
  status: string
  detail: string
  updated_at: string
  latency_ms: number
  error_rate: number
  metrics: Record<string, unknown>
}

export type GatewayProvidersResponse = {
  configs: GatewayProviderConfig[]
  health: Record<string, GatewayProviderHealth>
}

export type GatewayBinding = {
  id: string
  agent_id: string
  platform: string
  chat_id: string
  thread_id: string
  sender_id: string
  display_name: string
  enabled: boolean
  metadata: Record<string, string>
  updated_at: string
}

export type GatewayRouteMatch = {
  platform: string
  chat_id: string
  thread_id: string
  sender_id: string
  event_type: string
  content_prefix: string
  regex: string
  mention: string
  metadata: Record<string, string>
}

export type GatewayRouteAction = {
  target_agent: string
  target: string
  target_session: string
  strip_prefix: boolean
  create_session: boolean
  priority: string
}

export type GatewayRouteRule = {
  name: string
  priority: number
  match: GatewayRouteMatch
  action: GatewayRouteAction
  enabled: boolean
}

export type GatewayOutboundEvent = {
  message_id: string
  target: string
  platform: string
  chat_id: string
  thread_id: string
  reply_to_id: string
  text_markdown: string
  actions: Array<{ label: string; value: string; url: string }>
  attachments: Array<{ type: string; url: string; file_name: string; mime: string; size: number }>
  stream: boolean
  phase: string
  idempotency_key: string
  priority: string
  ttl_seconds: number
}

export type GatewayDLQItem = {
  id: string
  agent_id: string
  target: string
  event: GatewayOutboundEvent
  error: string
  retry_count: number
  created_at: string
}

export async function getSystemStatus() {
  const { data } = await client.get<SystemStatus>('/system/status')
  return data
}

export async function bootstrap(payload: {
  master_password: string
  provider?: string
  default_model?: string
  api_key?: string
}) {
  await client.post('/system/bootstrap', payload)
}

export async function login(password: string) {
  const { data } = await client.post<{ token: string; expires_in_sec: number }>('/auth/login', { password })
  localStorage.setItem('xclaw_token', data.token)
  return data
}

export async function logout() {
  await client.post('/auth/logout')
  localStorage.removeItem('xclaw_token')
}

export async function listAgents() {
  const { data } = await client.get<Agent[]>('/agents')
  return data
}

export async function createAgent(payload: Partial<Agent> & { tools: string[] }) {
  const { data } = await client.post<Agent>('/agents', payload)
  return data
}

export async function updateAgent(id: string, payload: Partial<Agent> & { tools: string[] }) {
  const { data } = await client.put<Agent>(`/agents/${id}`, payload)
  return data
}

export async function deleteAgent(id: string) {
  await client.delete(`/agents/${id}`)
}

export async function listMCPTools() {
  const { data } = await client.get<MCPTool[]>('/mcp/tools')
  return data
}

export async function listMCPServers() {
  const { data } = await client.get<MCPServer[]>('/mcp/servers')
  return data
}

export async function upsertMCPServer(payload: MCPServer) {
  const { data } = await client.post<MCPServer>('/mcp/servers', payload)
  return data
}

export async function deleteMCPServer(id: string) {
  await client.delete('/mcp/servers', { params: { id } })
}

export async function testMCPServer(id: string) {
  const { data } = await client.post('/mcp/servers/test', { id })
  return data as {
    id: string
    name: string
    transport: string
    tool_count: number
    tools: MCPTool[]
  }
}

export async function listSessions(agentID: string) {
  const { data } = await client.get<Session[]>('/sessions', { params: { agent_id: agentID } })
  return data
}

export async function createSession(payload: { agent_id: string; title: string; is_main: boolean }) {
  const { data } = await client.post<Session>('/sessions', payload)
  return data
}

export async function listMessages(sessionID: string) {
  const { data } = await client.get<Message[]>(`/sessions/${sessionID}/messages`)
  return data
}

export async function sendMessage(sessionID: string, payload: SendMessagePayload) {
  const { data } = await client.post<Message>(`/sessions/${sessionID}/messages`, payload)
  return data
}

export async function listCron(agentID = '') {
  const { data } = await client.get<CronJob[]>('/cron', { params: { agent_id: agentID } })
  return data
}

export async function createCron(payload: {
  agent_id: string
  name: string
  schedule: string
  job_type: string
  payload: string
  enabled: boolean
  retry_limit: number
}) {
  const { data } = await client.post<CronJob>('/cron', payload)
  return data
}

export async function deleteCron(id: string) {
  await client.delete(`/cron/${id}`)
}

export async function listAudit(limit = 200) {
  const { data } = await client.get<AuditLog[]>('/audit', { params: { limit } })
  return data
}

export async function getSystemMetrics() {
  const { data } = await client.get<Record<string, unknown>>('/system/metrics')
  return data
}

export async function getTokenStats(agentID = '') {
  const { data } = await client.get<Record<string, unknown>>('/system/token-stats', {
    params: agentID ? { agent_id: agentID } : undefined
  })
  return data
}

export async function getUpdateCheck() {
  const { data } = await client.get<Record<string, unknown>>('/system/update-check')
  return data
}

export async function installSystemUpdate(channel = 'stable') {
  const { data } = await client.post<Record<string, unknown>>('/system/update-install', { channel })
  return data
}

export async function restartSystem() {
  const { data } = await client.post<Record<string, unknown>>('/system/restart')
  return data
}

export async function getVectorStatus() {
  const { data } = await client.get<VectorStatus>('/system/vector-status')
  return data
}

export async function searchVectorMemory(payload: { agent_id?: string; query: string; limit?: number }) {
  const { data } = await client.post<VectorMemoryHit[]>('/memory/vector/search', payload)
  return data
}

export async function listSkillCatalog(query = '') {
  const { data } = await client.get<SkillItem[]>('/skills/catalog', {
    params: query.trim() ? { q: query.trim() } : undefined
  })
  return data
}

export async function getSkillMarketURL() {
  const { data } = await client.get<{ url: string }>('/skills/market-url')
  return data.url
}

export async function listInstalledSkills() {
  const { data } = await client.get<SkillItem[]>('/skills/installed')
  return data
}

export async function installSkill(payload: {
  name?: string
  version?: string
  source?: string
  source_url?: string
  source_path?: string
  checksum?: string
}) {
  const { data } = await client.post<InstallSkillResult>('/skills/install', payload)
  return data
}

export async function uninstallSkill(payload: { name: string }) {
  const { data } = await client.post<UninstallSkillResult>('/skills/uninstall', payload)
  return data
}

export async function listLoadedSkills() {
  const { data } = await client.get<LoadedSkill[]>('/skills/list')
  return data
}

export async function getSkillDetail(name: string) {
  const { data } = await client.get<SkillDetail>('/skills/detail', {
    params: { name }
  })
  return data
}

export async function reloadSkills() {
  const { data } = await client.post<{ ok: boolean; count: number }>('/skills/reload')
  return data
}

export async function uploadMultimodal(file: File) {
  const form = new FormData()
  form.append('file', file)
  const { data } = await client.post<MultimodalFile>('/multimodal/upload', form, {
    headers: { 'Content-Type': 'multipart/form-data' }
  })
  return data
}

export async function listMultimodalFiles() {
  const { data } = await client.get<MultimodalFile[]>('/multimodal/files')
  return data
}

export async function listA2APeers() {
  const { data } = await client.get<A2APeer[]>('/a2a/peers')
  return data
}

export async function registerA2APeer(payload: A2APeer) {
  const { data } = await client.post<Record<string, unknown>>('/a2a/peers/register', payload)
  return data
}

export async function dispatchA2A(payload: {
  peer_url: string
  from: string
  to: string
  task: string
  inputs?: Record<string, string>
}) {
  const { data } = await client.post<Record<string, unknown>>('/a2a/dispatch', payload)
  return data
}

export async function saveCredential(payload: { provider: string; secret: string; master_password: string }) {
  await client.post('/credentials', payload)
}

export async function revealCredential(payload: { provider: string; master_password: string }) {
  const { data } = await client.post<{ secret: string }>('/credentials/reveal', payload)
  return data.secret
}

export async function listApprovalRules() {
  const { data } = await client.get<ApprovalRule[]>('/approvals/rules')
  return data
}

export async function saveApprovalRules(rules: ApprovalRule[]) {
  const { data } = await client.put<ApprovalRule[]>('/approvals/rules', rules)
  return data
}

export async function listApprovalRequests() {
  const { data } = await client.get<ApprovalRequest[]>('/approvals/requests')
  return data
}

export async function approveRequest(id: string, actor = 'admin') {
  const { data } = await client.post<ApprovalRequest>('/approvals/approve', { id, actor })
  return data
}

export async function rejectRequest(id: string, actor = 'admin') {
  const { data } = await client.post<ApprovalRequest>('/approvals/reject', { id, actor })
  return data
}

export async function listKnowledgeMounts() {
  const { data } = await client.get<KnowledgeMount[]>('/knowledge/mounts')
  return data
}

export async function upsertKnowledgeMount(payload: Partial<KnowledgeMount> & { name: string; type: string; source: string }) {
  const { data } = await client.post<KnowledgeMount>('/knowledge/mounts', payload)
  return data
}

export async function reindexKnowledgeMount(id: string) {
  const { data } = await client.post<KnowledgeMount>(`/knowledge/reindex?id=${encodeURIComponent(id)}`)
  return data
}

export async function searchKnowledge(payload: { query: string; limit?: number; mount_id?: string }) {
  const { data } = await client.post<VectorMemoryHit[]>('/knowledge/search', payload)
  return data
}

export async function analyzeMultimodal(payload: { id: string; prompt: string; model?: string; api_key?: string }) {
  const { data } = await client.post<{ summary: string; mode: string }>('/multimodal/analyze', payload)
  return data
}

export async function renderMultimodal(payload: { kind: 'mermaid' | 'echarts' | 'html' | 'image'; title?: string; prompt: string }) {
  const { data } = await client.post<{ kind: string; title: string; content: any }>('/multimodal/render', payload)
  return data
}

export async function speechToText(file: File, apiKey = '', model = 'whisper-1') {
  const form = new FormData()
  form.append('file', file)
  if (apiKey) {
    form.append('api_key', apiKey)
  }
  form.append('model', model)
  const { data } = await client.post<{ text: string }>('/audio/stt', form, {
    headers: { 'Content-Type': 'multipart/form-data' }
  })
  return data.text
}

export async function textToSpeech(payload: { text: string; voice?: string; model?: string; api_key?: string }) {
  const { data } = await client.post<{ mime: string; audio_base64: string; bytes: number }>('/audio/tts', payload)
  return data
}

export async function getModelRouting() {
  const { data } = await client.get('/system/model-routing')
  return data as {
    enabled: boolean
    low_complexity_max: number
    medium_complexity_max: number
    hard_keywords: string[]
  }
}

export async function setModelRouting(payload: {
  enabled: boolean
  low_complexity_max: number
  medium_complexity_max: number
  hard_keywords: string[]
}) {
  const { data } = await client.put('/system/model-routing', payload)
  return data as {
    enabled: boolean
    low_complexity_max: number
    medium_complexity_max: number
    hard_keywords: string[]
  }
}

export async function getGatewayConfig() {
  const { data } = await client.get<GatewayConfig>('/gateway/config')
  return data
}

export async function updateGatewayConfig(payload: GatewayConfig) {
  const { data } = await client.put<GatewayConfig>('/gateway/config', payload)
  return data
}

export async function listGatewayProviders() {
  const { data } = await client.get<GatewayProvidersResponse>('/gateway/providers')
  return data
}

export async function upsertGatewayProvider(payload: GatewayProviderConfig) {
  await client.put('/gateway/providers', payload)
}

export async function listGatewayBindings(agentID = '') {
  const { data } = await client.get<GatewayBinding[]>('/gateway/bindings', {
    params: agentID.trim() ? { agent_id: agentID.trim() } : undefined
  })
  return data
}

export async function upsertGatewayBinding(
  payload: Partial<GatewayBinding> & {
    agent_id: string
    platform: string
    chat_id: string
  }
) {
  const { data } = await client.post<GatewayBinding>('/gateway/bindings', payload)
  return data
}

export async function deleteGatewayBinding(id: string) {
  await client.delete(`/gateway/bindings/${encodeURIComponent(id)}`)
}

export async function listGatewayRoutes() {
  const { data } = await client.get<GatewayRouteRule[]>('/gateway/routes')
  return data
}

export async function upsertGatewayRoute(payload: Partial<GatewayRouteRule> & { name: string }) {
  await client.post('/gateway/routes', payload)
}

export async function deleteGatewayRoute(name: string) {
  await client.delete(`/gateway/routes/${encodeURIComponent(name)}`)
}

export async function listGatewayDLQ() {
  const { data } = await client.get<GatewayDLQItem[]>('/gateway/dlq')
  return data
}

export async function replayGatewayDLQ(id: string) {
  const { data } = await client.post<{ ok: boolean; replayed: string }>(`/gateway/dlq/${encodeURIComponent(id)}`)
  return data
}

export async function deleteGatewayDLQ(id: string) {
  await client.delete(`/gateway/dlq/${encodeURIComponent(id)}`)
}

export async function retryAllGatewayDLQ() {
  const { data } = await client.post<{ retried: string[]; failed: string[] }>('/gateway/dlq/batch-retry')
  return data
}

export async function purgeGatewayDLQ() {
  const { data } = await client.delete<{ purged: string[]; failed: string[] }>('/gateway/dlq/purge')
  return data
}
