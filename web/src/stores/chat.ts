import { defineStore } from 'pinia'
import { ref, computed } from 'vue'
import {
  type ChatAttachmentInput,
  type ChatPollInput,
  createSession,
  listMessages,
  listSessions,
  sendMessage,
  type Message,
  type Session
} from '@/api/client'

export type UserMessagePayload = {
  content: string
  reply_to_id?: string
  attachments?: ChatAttachmentInput[]
  poll?: ChatPollInput
  metadata?: Record<string, unknown>
}

export const useChatStore = defineStore(
  'chat',
  () => {
    const sessions = ref<Session[]>([])
    const messages = ref<Message[]>([])
    const selectedSessionID = ref('')
    const autoApprove = ref(false)

    const currentSession = computed(() => sessions.value.find((x) => x.id === selectedSessionID.value) || null)

    async function refreshSessions(agentID: string) {
      sessions.value = agentID ? await listSessions(agentID) : []
      if (sessions.value.length === 0) {
        selectedSessionID.value = ''
        messages.value = []
        return
      }
      if (!selectedSessionID.value || !sessions.value.some((x) => x.id === selectedSessionID.value)) {
        selectedSessionID.value = sessions.value[0].id
      }
      await refreshMessages()
    }

    async function createSessionForAgent(agentID: string, title: string) {
      const session = await createSession({
        agent_id: agentID,
        title,
        is_main: sessions.value.length === 0
      })
      await refreshSessions(agentID)
      selectedSessionID.value = session.id
      await refreshMessages()
    }

    async function refreshMessages() {
      if (!selectedSessionID.value) {
        messages.value = []
        return
      }
      messages.value = await listMessages(selectedSessionID.value)
    }

    async function pushUserMessage(payload: string | UserMessagePayload) {
      if (!selectedSessionID.value) {
        throw new Error('请先创建会话')
      }
      const request =
        typeof payload === 'string'
          ? { content: payload.trim(), auto_approve: autoApprove.value }
          : {
              content: payload.content.trim(),
              auto_approve: autoApprove.value,
              reply_to_id: payload.reply_to_id,
              attachments: payload.attachments,
              poll: payload.poll,
              metadata: payload.metadata
            }

      const hasAttachments = Array.isArray(request.attachments) && request.attachments.length > 0
      const hasPoll = !!request.poll
      if (!request.content && !hasAttachments && !hasPoll) {
        throw new Error('消息内容不能为空')
      }

      const created = await sendMessage(selectedSessionID.value, request)
      await refreshMessages()
      return created
    }

    return {
      sessions,
      messages,
      selectedSessionID,
      autoApprove,
      currentSession,
      refreshSessions,
      createSessionForAgent,
      refreshMessages,
      pushUserMessage
    }
  },
  {
    persist: {
      pick: ['selectedSessionID', 'autoApprove']
    }
  }
)
