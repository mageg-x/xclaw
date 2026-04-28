<script setup lang="ts">
import { onMounted, reactive, ref } from 'vue'
import { Message, Modal } from '@arco-design/web-vue'
import {
  deleteMCPServer,
  listMCPServers,
  listMCPTools,
  testMCPServer,
  upsertMCPServer,
  type MCPServer,
  type MCPTool
} from '@/api/client'

const loading = ref(false)
const saving = ref(false)
const servers = ref<MCPServer[]>([])
const tools = ref<MCPTool[]>([])
const testResult = ref('')
const envText = ref('')

const form = reactive<MCPServer>({
  id: '',
  name: '',
  transport: 'http',
  url: '',
  command: '',
  args: [],
  enabled: true,
  timeout_sec: 20
})

async function refresh() {
  loading.value = true
  try {
    const [serverItems, toolItems] = await Promise.all([listMCPServers(), listMCPTools()])
    servers.value = serverItems
    tools.value = toolItems
  } finally {
    loading.value = false
  }
}

async function save() {
  saving.value = true
  try {
    await upsertMCPServer({
      ...form,
      args: Array.isArray(form.args)
        ? form.args
        : String(form.args || '')
            .split(',')
            .map((item) => item.trim())
            .filter(Boolean),
      env: parseEnvText(envText.value)
    })
    Message.success('MCP Server 已保存')
    resetForm()
    await refresh()
  } catch (error) {
    Message.error((error as Error).message)
  } finally {
    saving.value = false
  }
}

function edit(item: MCPServer) {
  if (item.readonly) {
    Message.info(`该 MCP Server 由 ${item.managed_by || '系统'} 自动管理，请在对应 Skill 包中修改`)
    return
  }
  form.id = item.id
  form.name = item.name
  form.transport = item.transport
  form.url = item.url || ''
  form.command = item.command || ''
  form.args = [...(item.args || [])]
  form.env = { ...(item.env || {}) }
  form.enabled = item.enabled
  form.timeout_sec = item.timeout_sec || 20
  envText.value = formatEnv(item.env)
}

function resetForm() {
  form.id = ''
  form.name = ''
  form.transport = 'http'
  form.url = ''
  form.command = ''
  form.args = []
  form.env = {}
  form.enabled = true
  form.timeout_sec = 20
  envText.value = ''
}

function parseEnvText(value: string) {
  const out: Record<string, string> = {}
  for (const raw of value.split('\n')) {
    const line = raw.trim()
    if (!line) {
      continue
    }
    const idx = line.indexOf('=')
    if (idx < 0) {
      out[line] = ''
      continue
    }
    const key = line.slice(0, idx).trim()
    if (!key) {
      continue
    }
    out[key] = line.slice(idx + 1).trim()
  }
  return Object.keys(out).length ? out : undefined
}

function formatEnv(value?: Record<string, string>) {
  return Object.entries(value || {})
    .map(([key, val]) => `${key}=${val}`)
    .join('\n')
}

function removeServer(id: string) {
  Modal.warning({
    title: '删除 MCP Server',
    content: `确认删除 ${id} 吗？`,
    onOk: async () => {
      try {
        await deleteMCPServer(id)
        Message.success('已删除')
        await refresh()
      } catch (error) {
        Message.error((error as Error).message)
      }
    }
  })
}

async function runTest(id: string) {
  try {
    const result = await testMCPServer(id)
    testResult.value = JSON.stringify(result, null, 2)
    Message.success(`测试完成，发现 ${result.tool_count} 个工具`)
    await refresh()
  } catch (error) {
    testResult.value = String((error as Error).message || error)
    Message.error((error as Error).message)
  }
}

onMounted(refresh)
</script>

<template>
  <div class="page split">
    <a-card title="注册 MCP Server" :bordered="false">
      <a-alert type="info" style="margin-bottom: 20px">
        支持 `http` 和 `stdio` 两种 transport。保存后可直接测试并将发现的工具分配给 Agent。
      </a-alert>

      <a-row :gutter="12">
        <a-col :xs="24" :sm="12">
          <div class="form-group">
            <label class="form-label">ID</label>
            <a-input v-model="form.id" placeholder="例如 github-tools" />
          </div>
        </a-col>
        <a-col :xs="24" :sm="12">
          <div class="form-group">
            <label class="form-label">名称</label>
            <a-input v-model="form.name" placeholder="例如 GitHub Tools" />
          </div>
        </a-col>
      </a-row>

      <a-row :gutter="12">
        <a-col :xs="24" :sm="8">
          <div class="form-group">
            <label class="form-label">Transport</label>
            <a-select v-model="form.transport">
              <a-option value="http">http</a-option>
              <a-option value="stdio">stdio</a-option>
            </a-select>
          </div>
        </a-col>
        <a-col :xs="24" :sm="8">
          <div class="form-group">
            <label class="form-label">Timeout</label>
            <a-input-number v-model="form.timeout_sec" :min="5" :max="120" style="width: 100%" />
          </div>
        </a-col>
        <a-col :xs="24" :sm="8">
          <div class="form-group">
            <label class="form-label">启用</label>
            <a-switch v-model="form.enabled" />
          </div>
        </a-col>
      </a-row>

      <div v-if="form.transport === 'http'" class="form-group">
        <label class="form-label">URL</label>
        <a-input v-model="form.url" placeholder="例如 http://127.0.0.1:8000/mcp" />
      </div>

      <template v-else>
        <div class="form-group">
          <label class="form-label">命令</label>
          <a-input v-model="form.command" placeholder="例如 npx" />
        </div>
        <div class="form-group">
          <label class="form-label">参数（逗号分隔）</label>
          <a-input
            :model-value="Array.isArray(form.args) ? form.args.join(',') : ''"
            placeholder="例如 -y,@modelcontextprotocol/server-filesystem,/tmp"
            @update:model-value="
              (value) =>
                (form.args = String(value)
                  .split(',')
                  .map((item) => item.trim())
                  .filter(Boolean))
            "
          />
        </div>
        <div class="form-group">
          <label class="form-label">环境变量（每行一个 `KEY=VALUE`）</label>
          <a-textarea
            v-model="envText"
            :auto-size="{ minRows: 3, maxRows: 8 }"
            placeholder="例如&#10;GITHUB_TOKEN=...&#10;HOME=/tmp/xclaw"
          />
        </div>
      </template>

      <a-space>
        <a-button type="primary" :loading="saving" @click="save">保存</a-button>
        <a-button @click="resetForm">重置</a-button>
      </a-space>

      <div v-if="testResult" class="form-group" style="margin-top: 20px">
        <label class="form-label">最近测试结果</label>
        <pre class="mcp-result">{{ testResult }}</pre>
      </div>
    </a-card>

    <a-card title="MCP Server 列表" :bordered="false" :loading="loading">
      <a-list :data="servers" :bordered="false">
        <template #item="{ item }">
          <a-list-item>
            <div style="width: 100%">
              <div class="server-row">
                <a-space wrap>
                  <strong>{{ item.name }}</strong>
                  <a-tag color="arcoblue">{{ item.id }}</a-tag>
                  <a-tag>{{ item.transport }}</a-tag>
                  <a-tag :color="item.enabled ? 'green' : 'gray'">{{ item.enabled ? 'enabled' : 'disabled' }}</a-tag>
                  <a-tag v-if="item.readonly" color="orange">{{ item.managed_by || 'managed' }}</a-tag>
                </a-space>
                <a-space>
                  <a-button size="small" :disabled="!!item.readonly" @click="edit(item)">编辑</a-button>
                  <a-button size="small" @click="runTest(item.id)">测试</a-button>
                  <a-button size="small" status="danger" :disabled="!!item.readonly" @click="removeServer(item.id)">删除</a-button>
                </a-space>
              </div>
              <a-typography-paragraph type="secondary" style="margin: 8px 0 0">
                {{ item.transport === 'http' ? item.url : `${item.command || ''} ${(item.args || []).join(' ')}` }}
              </a-typography-paragraph>
              <a-typography-paragraph
                v-if="item.env && Object.keys(item.env).length"
                type="secondary"
                style="margin: 4px 0 0"
              >
                env: {{ Object.keys(item.env).join(', ') }}
              </a-typography-paragraph>
            </div>
          </a-list-item>
        </template>
      </a-list>
    </a-card>

    <a-card title="已发现工具" :bordered="false" :loading="loading" class="wide-card">
      <a-table :data="tools" :pagination="false" size="small">
        <template #columns>
          <a-table-column title="Tool" data-index="full_name" :width="320" />
          <a-table-column title="Server" data-index="server_name" :width="180" />
          <a-table-column title="Risk" data-index="risk" :width="100" />
          <a-table-column title="Transport" data-index="transport" :width="100" />
          <a-table-column title="Status" :width="120">
            <template #cell="{ record }">
              <a-tag :color="record.available ? 'green' : 'red'">
                {{ record.available ? 'ok' : 'error' }}
              </a-tag>
            </template>
          </a-table-column>
          <a-table-column title="Description" data-index="description" />
          <a-table-column title="Last Error">
            <template #cell="{ record }">
              <span class="tool-error">{{ record.last_error || '-' }}</span>
            </template>
          </a-table-column>
        </template>
      </a-table>
    </a-card>
  </div>
</template>

<style scoped>
.server-row {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 12px;
}

.wide-card {
  grid-column: 1 / -1;
}

.mcp-result {
  margin: 0;
  padding: 12px;
  border-radius: 12px;
  background: #0f172a;
  color: #dbeafe;
  font-size: 12px;
  white-space: pre-wrap;
  word-break: break-word;
}

.tool-error {
  font-size: 12px;
  color: #8b1e1e;
  word-break: break-word;
}
</style>
