<script setup lang="ts">
import { reactive, ref, watch } from 'vue'
import { Message } from '@arco-design/web-vue'
import { createCron, deleteCron, listCron, type CronJob } from '@/api/client'
import { useSystemStore } from '@/stores/system'

const systemStore = useSystemStore()
const jobs = ref<CronJob[]>([])
const loading = ref(false)

const form = reactive({
  agent_id: '',
  name: '定时任务',
  schedule: '*/10 * * * *',
  job_type: 'analyze',
  payload: '',
  enabled: true,
  retry_limit: 3
})

const cronPresets = [
  { label: '每 10 分钟', value: '*/10 * * * *' },
  { label: '每 30 分钟', value: '*/30 * * * *' },
  { label: '每小时整点', value: '0 * * * *' },
  { label: '每天 09:00', value: '0 9 * * *' }
]

function applyCronPreset(schedule: string) {
  form.schedule = schedule
}

watch(
  () => systemStore.agents,
  (agents) => {
    if (!form.agent_id && agents.length > 0) {
      form.agent_id = agents[0].id
    }
  },
  { immediate: true }
)

async function refresh() {
  loading.value = true
  try {
    jobs.value = await listCron(form.agent_id)
  } finally {
    loading.value = false
  }
}

async function createItem() {
  try {
    await createCron({ ...form })
    Message.success('Cron 任务已创建')
    await refresh()
  } catch (error) {
    Message.error((error as Error).message)
  }
}

async function removeItem(id: string) {
  try {
    await deleteCron(id)
    Message.success('已删除')
    await refresh()
  } catch (error) {
    Message.error((error as Error).message)
  }
}

refresh()
</script>

<template>
  <div class="page split">
    <a-card title="创建定时任务" :bordered="false">
      <a-alert type="warning" style="margin-bottom: 20px">
        这是“自动执行”功能。若你不熟悉 Cron，建议先用下方快捷模板。
      </a-alert>

      <div class="form-group">
        <label class="form-label">选择助手</label>
        <a-select v-model="form.agent_id" placeholder="选择助手" size="large">
          <a-option v-for="item in systemStore.agents" :key="item.id" :value="item.id">
            {{ item.emoji }} {{ item.name }}
          </a-option>
        </a-select>
      </div>

      <div class="form-group">
        <label class="form-label">任务名称</label>
        <a-input v-model="form.name" placeholder="例如：每小时汇总工单" size="large" />
      </div>

      <div class="form-group">
        <label class="form-label">执行周期（Cron）</label>
        <a-space wrap style="margin-bottom: 8px">
          <a-button v-for="item in cronPresets" :key="item.value" size="small" @click="applyCronPreset(item.value)">
            {{ item.label }}
          </a-button>
        </a-space>
        <a-input v-model="form.schedule" placeholder="例如：*/10 * * * *" size="large" />
      </div>

      <div class="form-group">
        <label class="form-label">任务类型</label>
        <a-select v-model="form.job_type" size="large">
          <a-option value="pull">pull（拉取）</a-option>
          <a-option value="analyze">analyze（分析）</a-option>
          <a-option value="notify">notify（通知）</a-option>
        </a-select>
      </div>

      <div class="form-group">
        <label class="form-label">任务内容（发给助手的指令）</label>
        <a-textarea v-model="form.payload" :auto-size="{ minRows: 2, maxRows: 6 }" placeholder="例如：汇总最近 1 小时异常日志并给出处理建议" />
      </div>

      <a-row :gutter="16">
        <a-col :xs="24" :sm="12">
          <div class="form-group">
            <a-checkbox v-model="form.enabled">创建后立即启用</a-checkbox>
          </div>
        </a-col>
        <a-col :xs="24" :sm="12">
          <div class="form-group">
            <label class="form-label">失败重试次数</label>
            <a-input-number v-model="form.retry_limit" :min="1" :max="5" size="large" style="width: 100%" />
          </div>
        </a-col>
      </a-row>

      <a-button type="primary" long @click="createItem" size="large">创建定时任务</a-button>
    </a-card>

    <a-card title="任务列表" :bordered="false">
      <div style="margin-bottom: 16px">
        <a-button @click="refresh" :loading="loading" type="primary">刷新</a-button>
      </div>
      <a-list :data="jobs" :bordered="false">
        <template #item="{ item }">
          <a-list-item>
            <div style="width: 100%">
              <div style="display: flex; align-items: center; justify-content: space-between; margin-bottom: 8px">
                <a-space>
                  <strong style="font-size: 15px">{{ item.name }}</strong>
                  <a-tag size="small">{{ item.schedule }}</a-tag>
                  <a-tag size="small" color="arcoblue">{{ item.job_type }}</a-tag>
                  <a-tag size="small" :color="item.enabled ? 'green' : 'gray'">{{ item.enabled ? '启用' : '关闭' }}</a-tag>
                </a-space>
                <a-button status="danger" size="small" @click="removeItem(item.id)">删除</a-button>
              </div>
              <a-typography-paragraph style="margin: 0 0 6px; color: var(--text-1)">
                {{ item.payload }}
              </a-typography-paragraph>
              <a-typography-paragraph type="secondary" style="margin: 0; font-size: 12px">
                状态：{{ item.last_status }} {{ item.last_error ? `| ${item.last_error}` : '' }}
              </a-typography-paragraph>
            </div>
          </a-list-item>
        </template>
      </a-list>
    </a-card>
  </div>
</template>
