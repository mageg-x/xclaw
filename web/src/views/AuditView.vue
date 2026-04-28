<script setup lang="ts">
import { computed, ref } from 'vue'
import { listAudit, type AuditLog } from '@/api/client'

const rows = ref<AuditLog[]>([])
const loading = ref(false)
const keyword = ref('')
const maxRows = ref(300)

const filteredRows = computed(() => {
  const q = keyword.value.trim().toLowerCase()
  if (!q) {
    return rows.value
  }
  return rows.value.filter((item) => {
    return [item.agent_id, item.session_id, item.category, item.action, item.detail]
      .join(' ')
      .toLowerCase()
      .includes(q)
  })
})

async function refresh() {
  loading.value = true
  try {
    rows.value = await listAudit(maxRows.value)
  } finally {
    loading.value = false
  }
}

refresh()
</script>

<template>
  <div class="page">
    <div class="page-header">
      <div>
        <h2>操作审计</h2>
        <p>查看系统关键操作记录，排查“谁在什么时候做了什么”</p>
      </div>
    </div>

    <a-alert type="info" style="margin-bottom: 20px">
      这里记录系统关键操作。你可以按关键词快速过滤，排查“谁在什么时候做了什么”。
    </a-alert>

    <a-card :bordered="false" style="margin-bottom: 20px">
      <a-row :gutter="16" align="center">
        <a-col :xs="24" :sm="12" :md="10">
          <a-input v-model="keyword" placeholder="按 agent / 动作 / 详情关键词过滤" size="large" allow-clear>
            <template #prefix>
              <icon-search />
            </template>
          </a-input>
        </a-col>
        <a-col :xs="24" :sm="8" :md="8">
          <a-space>
            <span style="color: var(--text-2); font-size: 13px">加载条数</span>
            <a-input-number v-model="maxRows" :min="50" :max="2000" :step="50" size="large" />
          </a-space>
        </a-col>
        <a-col :xs="24" :sm="4" :md="6" style="text-align: right">
          <a-button @click="refresh" :loading="loading" type="primary" size="large">刷新数据</a-button>
        </a-col>
      </a-row>
    </a-card>

    <a-table :data="filteredRows" :pagination="{ pageSize: 20 }" row-key="id" stripe>
      <a-table-column title="时间" data-index="created_at" />
      <a-table-column title="助手" data-index="agent_id" />
      <a-table-column title="会话" data-index="session_id" />
      <a-table-column title="分类" data-index="category">
        <template #cell="{ record }">
          <a-tag size="small" color="arcoblue">{{ record.category }}</a-tag>
        </template>
      </a-table-column>
      <a-table-column title="动作" data-index="action">
        <template #cell="{ record }">
          <a-tag size="small" color="green">{{ record.action }}</a-tag>
        </template>
      </a-table-column>
      <a-table-column title="详情" data-index="detail" :ellipsis="true" :tooltip="true" />
    </a-table>
  </div>
</template>
