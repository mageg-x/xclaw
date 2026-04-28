import { createRouter, createWebHistory } from 'vue-router'

import SystemView from '@/views/SystemView.vue'
import OnboardingView from '@/views/OnboardingView.vue'
import AgentsView from '@/views/AgentsView.vue'
import ChatView from '@/views/ChatView.vue'
import CronView from '@/views/CronView.vue'
import AuditView from '@/views/AuditView.vue'
import CapabilitiesView from '@/views/CapabilitiesView.vue'
import GatewayView from '@/views/GatewayView.vue'
import MCPView from '@/views/MCPView.vue'

const router = createRouter({
  history: createWebHistory(),
  routes: [
    { path: '/', redirect: '/system' },
    { path: '/system', component: SystemView },
    { path: '/onboarding', component: OnboardingView },
    { path: '/agents', component: AgentsView },
    { path: '/chat', component: ChatView },
    { path: '/gateway', component: GatewayView },
    { path: '/mcp', component: MCPView },
    { path: '/cron', component: CronView },
    { path: '/audit', component: AuditView },
    { path: '/capabilities', component: CapabilitiesView }
  ]
})

export default router
