<script setup lang="ts">
import { onMounted, reactive, ref } from 'vue'
import { Message, Modal } from '@arco-design/web-vue'
import { listMCPTools, type MCPTool } from '@/api/client'
import { useSystemStore } from '@/stores/system'

const store = useSystemStore()
const creating = ref(false)
const discoveredTools = ref<MCPTool[]>([])

const form = reactive({
  name: '',
  emoji: '🤖',
  description: '',
  model_provider: 'local',
  model_name: 'local-reasoner',
  tools: 'list_dir,read_file,search_text'
})

async function create() {
  creating.value = true
  try {
    await store.createAgentItem({
      name: form.name,
      emoji: form.emoji,
      description: form.description,
      model_provider: form.model_provider,
      model_name: form.model_name,
      tools: form.tools
        .split(',')
        .map((x) => x.trim())
        .filter(Boolean)
    })
    Message.success('Agent 已创建')
    form.name = ''
    form.description = ''
  } catch (error) {
    Message.error((error as Error).message)
  } finally {
    creating.value = false
  }
}

function removeAgent(id: string) {
  Modal.warning({
    title: '确认删除',
    content: '删除后会移除数据库中的该 Agent 记录，继续吗？',
    onOk: async () => {
      try {
        await store.deleteAgentItem(id)
        Message.success('已删除')
      } catch (error) {
        Message.error((error as Error).message)
      }
    }
  })
}

onMounted(async () => {
  try {
    discoveredTools.value = await listMCPTools()
  } catch {
    discoveredTools.value = []
  }
})
</script>

<template>
  <div class="page split">
    <a-card title="创建助手" :bordered="false">
      <a-alert type="info" style="margin-bottom: 20px">
        先填“名称 + 职责描述”就能用，其他项可直接用默认值。
      </a-alert>

      <div class="form-group">
        <label class="form-label">助手名称</label>
        <a-input v-model="form.name" placeholder="助手名称，例如：客服助手 / 研发助手" size="large" />
      </div>

      <div class="form-group">
        <label class="form-label">职责描述</label>
        <a-textarea
          v-model="form.description"
          :auto-size="{ minRows: 3, maxRows: 6 }"
          placeholder="职责描述，例如：负责回答产品问题，必要时查文档后回复。"
        />
      </div>

      <a-row :gutter="12">
        <a-col :xs="24" :sm="8">
          <div class="form-group">
            <label class="form-label">头像符号</label>
            <a-input v-model="form.emoji" placeholder="例如 🤖" />
          </div>
        </a-col>
        <a-col :xs="24" :sm="8">
          <div class="form-group">
            <label class="form-label">模型供应商</label>
            <a-input v-model="form.model_provider" placeholder="默认 local" />
          </div>
        </a-col>
        <a-col :xs="24" :sm="8">
          <div class="form-group">
            <label class="form-label">模型档位</label>
            <a-input v-model="form.model_name" placeholder="默认 local-reasoner" />
          </div>
        </a-col>
      </a-row>

      <div class="form-group">
        <label class="form-label">可用工具（逗号分隔）</label>
        <a-input v-model="form.tools" placeholder="list_dir,read_file,search_text" />
        <a-typography-paragraph type="secondary" style="margin: 8px 0 0; font-size: 12px">
          外部 MCP 工具使用格式：`mcp:&lt;server&gt;:&lt;tool&gt;`
        </a-typography-paragraph>
        <a-space wrap style="margin-top: 10px" v-if="discoveredTools.length">
          <a-tag
            v-for="tool in discoveredTools"
            :key="tool.full_name"
            size="small"
            color="arcoblue"
          >
            {{ tool.full_name }}
          </a-tag>
        </a-space>
      </div>

      <a-button type="primary" long :loading="creating" @click="create" size="large">创建助手</a-button>
    </a-card>

    <a-card title="助手列表" :bordered="false">
      <a-list :data="store.agents" :bordered="false">
        <template #item="{ item }">
          <a-list-item>
            <div style="width: 100%">
              <div style="display: flex; align-items: center; justify-content: space-between; margin-bottom: 8px">
                <a-space>
                  <span style="font-size: 20px">{{ item.emoji }}</span>
                  <strong style="font-size: 15px">{{ item.name }}</strong>
                  <a-tag color="arcoblue">{{ item.model_provider }}/{{ item.model_name }}</a-tag>
                </a-space>
                <a-button status="danger" size="small" @click="removeAgent(item.id)">删除</a-button>
              </div>
              <a-typography-paragraph style="margin: 0 0 8px; color: var(--text-1)">
                {{ item.description }}
              </a-typography-paragraph>
              <a-typography-paragraph type="secondary" style="margin: 0 0 8px; font-size: 12px">
                {{ item.id }}
              </a-typography-paragraph>
              <a-space>
                <a-tag v-for="tool in item.tools" :key="tool" color="green" size="small">{{ tool }}</a-tag>
              </a-space>
            </div>
          </a-list-item>
        </template>
      </a-list>
    </a-card>
  </div>
</template>
