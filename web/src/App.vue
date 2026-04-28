<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { Message } from '@arco-design/web-vue'
import {
  IconDashboard,
  IconRobot,
  IconMessage,
  IconClockCircle,
  IconSafe,
  IconApps,
  IconSettings,
  IconBulb,
  IconShareInternal,
  IconCode
} from '@arco-design/web-vue/es/icon'
import { useRoute } from 'vue-router'
import { login } from '@/api/client'
import { useSystemStore } from '@/stores/system'

const store = useSystemStore()
const route = useRoute()
const password = ref('')
const authLoading = ref(false)
const token = ref(localStorage.getItem('xclaw_token') || '')
let drainPollTimer: ReturnType<typeof setInterval> | null = null
const hasSeenDraining = ref(false)

const requireLogin = computed(() => Boolean(store.status?.bootstrapped) && !token.value)
const activeMenu = computed(() => menus.find((item) => item.path === route.path))
const serverURL = computed(() => {
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
  if (!serverURL.value || !browserURL.value) {
    return false
  }
  const serverSchema = serverURL.value.startsWith('https://') ? 'https' : 'http'
  const browserSchema = browserURL.value.startsWith('https://') ? 'https' : 'http'
  return serverSchema !== browserSchema
})
const globalDrainSummary = computed(() => {
  if (!store.status?.draining) {
    return ''
  }
  const requests = store.status.active_requests ?? 0
  const sse = store.status.active_sse ?? 0
  const ws = store.status.active_ws ?? 0
  return `服务切换中：活跃请求 ${requests}，SSE ${sse}，WebSocket ${ws}`
})

onMounted(async () => {
  await store.init()
})

function startDrainPolling() {
  if (drainPollTimer) return
  drainPollTimer = setInterval(() => {
    if (typeof document !== 'undefined' && document.hidden) {
      return
    }
    void store.refreshStatus()
  }, 2000)
}

function stopDrainPolling() {
  if (!drainPollTimer) return
  clearInterval(drainPollTimer)
  drainPollTimer = null
}

watch(
  () => store.status?.draining,
  (draining, prevDraining) => {
    if (draining) {
      hasSeenDraining.value = true
      startDrainPolling()
      return
    }
    stopDrainPolling()
    if ((prevDraining || hasSeenDraining.value) && store.status) {
      Message.success('服务已恢复，当前页面功能可继续使用')
      hasSeenDraining.value = false
    }
  },
  { immediate: true }
)

onBeforeUnmount(() => {
  stopDrainPolling()
})

async function doLogin() {
  authLoading.value = true
  try {
    const data = await login(password.value)
    token.value = data.token
    Message.success('登录成功')
  } catch (error) {
    Message.error((error as Error).message)
  } finally {
    authLoading.value = false
  }
}

const menus = [
  { path: '/system', label: '系统总览', desc: '查看运行状态和关键指标', icon: IconDashboard },
  { path: '/onboarding', label: '初始化向导', desc: '首次配置密码和默认模型', icon: IconBulb },
  { path: '/agents', label: '助手管理', desc: '创建或删除助手', icon: IconRobot },
  { path: '/chat', label: '聊天助手', desc: '和助手对话执行任务', icon: IconMessage },
  { path: '/gateway', label: '社交通道', desc: '配置 Telegram/企业微信/Slack 等接入', icon: IconShareInternal },
  { path: '/mcp', label: 'MCP 工具', desc: '注册外部 MCP Server 与工具', icon: IconCode },
  { path: '/cron', label: '定时任务', desc: '让助手按计划自动执行', icon: IconClockCircle },
  { path: '/audit', label: '操作审计', desc: '查看关键操作记录', icon: IconSafe },
  { path: '/capabilities', label: '能力中心', desc: '技能、知识库、路由等高级能力', icon: IconApps }
]
</script>

<template>
  <a-layout class="layout">
    <a-layout-header class="header">
      <div class="brand">
        <div class="brand-icon">X</div>
        <div class="brand-text">
          <h1>XClaw Console</h1>
          <span class="brand-subtitle">Single Binary Agent Runtime</span>
        </div>
      </div>
      <a-space class="header-status" size="small">
        <a-tag color="green" v-if="serverURL">{{ serverURL }}</a-tag>
        <a-tag color="arcoblue">Agent: {{ store.agents.length }}</a-tag>
        <a-tag :color="store.status?.bootstrapped ? 'green' : 'orange'">
          {{ store.status?.bootstrapped ? '已初始化' : '未初始化' }}
        </a-tag>
      </a-space>
    </a-layout-header>

    <div class="main-layout">
      <aside class="sider">
        <div class="sider-scroll">
          <a-menu :selected-keys="[$route.path]">
            <a-menu-item v-for="item in menus" :key="item.path" @click="$router.push(item.path)">
              <template #icon>
                <component :is="item.icon" />
              </template>
              {{ item.label }}
            </a-menu-item>
          </a-menu>
          <div class="sider-hint">
            <span>{{ activeMenu?.label || '控制台' }}</span>
            <small>{{ activeMenu?.desc || '管理你的 XClaw 运行环境' }}</small>
          </div>
        </div>
      </aside>

      <main class="content">
        <div class="content-shell">
          <a-alert v-if="store.status?.draining" type="warning" style="margin-bottom: 16px; border-radius: 12px">
            {{ globalDrainSummary }}
          </a-alert>
          <a-alert v-if="schemaMismatch" type="warning" style="margin-bottom: 16px; border-radius: 12px">
            当前浏览器访问协议和后端配置协议不一致，建议到系统总览页检查 <code>server.tls</code> 配置。
          </a-alert>
          <a-modal :visible="requireLogin" :footer="false" :closable="false" :mask-closable="false" width="420px">
            <div style="text-align: center; padding: 8px 0 16px">
              <div class="brand-icon" style="margin: 0 auto 12px; width: 48px; height: 48px; font-size: 24px">X</div>
              <h3 style="margin: 0 0 4px; font-size: 18px; font-weight: 700">管理员登录</h3>
              <p style="margin: 0; color: var(--text-2); font-size: 13px">请输入初始化时设置的管理员密码</p>
            </div>
            <a-space direction="vertical" fill>
              <a-input-password v-model="password" placeholder="输入初始化时设置的密码" size="large" />
              <a-button type="primary" long :loading="authLoading" size="large" @click="doLogin">登录</a-button>
            </a-space>
          </a-modal>
          <router-view />
        </div>
      </main>
    </div>
  </a-layout>
</template>
