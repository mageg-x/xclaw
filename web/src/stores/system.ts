import { defineStore } from 'pinia'
import { ref } from 'vue'
import {
  bootstrap,
  createAgent,
  deleteAgent,
  getSystemStatus,
  listAgents,
  type Agent,
  type SystemStatus,
  updateAgent
} from '@/api/client'

export const useSystemStore = defineStore(
  'system',
  () => {
    const status = ref<SystemStatus | null>(null)
    const agents = ref<Agent[]>([])
    const loading = ref(false)

    async function refreshStatus() {
      status.value = await getSystemStatus()
    }

    async function refreshAgents() {
      try {
        agents.value = await listAgents()
      } catch {
        agents.value = []
      }
    }

    async function bootstrapSystem(payload: {
      master_password: string
      provider?: string
      default_model?: string
      api_key?: string
    }) {
      await bootstrap(payload)
      await refreshStatus()
    }

    async function createAgentItem(payload: Partial<Agent> & { tools: string[] }) {
      await createAgent(payload)
      await refreshAgents()
      await refreshStatus()
    }

    async function updateAgentItem(id: string, payload: Partial<Agent> & { tools: string[] }) {
      await updateAgent(id, payload)
      await refreshAgents()
    }

    async function deleteAgentItem(id: string) {
      await deleteAgent(id)
      await refreshAgents()
      await refreshStatus()
    }

    async function init() {
      loading.value = true
      try {
        await Promise.all([refreshStatus(), refreshAgents()])
      } finally {
        loading.value = false
      }
    }

    return {
      status,
      agents,
      loading,
      init,
      refreshStatus,
      refreshAgents,
      bootstrapSystem,
      createAgentItem,
      updateAgentItem,
      deleteAgentItem
    }
  },
  {
    persist: {
      pick: []
    }
  }
)
