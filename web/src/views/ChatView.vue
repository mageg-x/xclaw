<script setup lang="ts">
import { computed, nextTick, onBeforeUnmount, onMounted, reactive, ref, watch } from 'vue'
import { Message } from '@arco-design/web-vue'
import {
  Attachments as TAttachments,
  ChatActionbar as TChatActionbar,
  Chatbot as TChatbot,
  ChatContent as TChatContent,
  ChatList as TChatList,
  ChatLoading as TChatLoading,
  ChatMarkdown as TChatMarkdown,
  ChatMessage as TChatMessage,
  ChatSender as TChatSender,
  ChatThinking as TChatThinking
} from '@tdesign-vue-next/chat'
import '@tdesign-vue-next/chat/es/style/index.css'
import type { AIMessage, AttachmentItem, ChatMessagesData, ChatRequestParams, UserMessage } from 'tdesign-web-components/lib/chat-engine'
import { sendMessage as sendChatMessage, speechToText, textToSpeech, uploadMultimodal, type Message as ChatMessage } from '@/api/client'
import { useChatStore } from '@/stores/chat'
import { useSystemStore } from '@/stores/system'

type MessageRole = 'user' | 'assistant' | 'system'
type MessageVote = '' | 'good' | 'bad'
type AttachmentKind = 'image' | 'file' | 'audio'
type AssistantPhase = 'idle' | 'thinking' | 'executing' | 'typing'

type UIAttachment = {
  id: string
  kind: AttachmentKind
  name: string
  size: number
  mime: string
  url: string
  durationSec?: number
}

type UIPollOption = {
  id: string
  label: string
  votes: number
}

type UIPoll = {
  question: string
  options: UIPollOption[]
  votedOptionID?: string
}

type UIThinking = {
  title: string
  lines: string[]
  collapsed?: boolean
  status?: 'pending' | 'complete' | 'stop' | 'error'
}

type MessageDecoration = {
  replyToID?: string
  attachments?: UIAttachment[]
  poll?: UIPoll
  vote?: MessageVote
  thinking?: UIThinking
}

type TimelineItem = {
  id: string
  role: MessageRole
  name: string
  datetime: string
  content: string
  pending?: boolean
  streamState?: AssistantPhase
  decoration: MessageDecoration
}

const REQUEST_CANCELLED = '__xclaw_request_cancelled__'

const systemStore = useSystemStore()
const chatStore = useChatStore()

const selectedAgentID = ref('')
const composer = ref('')
const sendNeedConfirm = ref(false)
const sending = ref(false)
const speakingMessageID = ref('')
const replyToMessageID = ref('')
const serviceDraining = ref(false)
const drainNotice = ref('')
const hasSeenDraining = ref(false)
const reconnectEpoch = ref(0)
const passiveStreamConnected = ref(false)
const lastPassiveEventID = ref('')
const activeRecoveredUserMessageID = ref('')
const passiveTransport = ref<'idle' | 'sse' | 'ws'>('idle')

const showEmojiPanel = ref(false)
const emojiList = ['😀', '😁', '😄', '😎', '🤝', '👍', '👎', '🔥', '✅', '❌', '🎯', '📌', '💡']
const senderActions = [
  { name: 'uploadImage', uploadProps: { accept: 'image/*', multiple: true } },
  { name: 'uploadAttachment', uploadProps: { multiple: true } },
  'send'
] as const
const senderTextareaProps = {
  maxlength: 12000,
  autosize: { minRows: 3, maxRows: 8 }
} as const
const chatbotInjectCSS = {
  ChatSender: ':host{display:none !important;}'
} as const

const draftAttachments = ref<UIAttachment[]>([])
const draftFileMap = reactive<Record<string, File>>({})

const pollEnabled = ref(false)
const pollQuestion = ref('')
const pollOptionsText = ref('赞成\n反对')

const canRecordAudio = computed(
  () => typeof window !== 'undefined' && typeof MediaRecorder !== 'undefined' && !!navigator.mediaDevices?.getUserMedia
)
const isRecording = ref(false)
const recordingSeconds = ref(0)
let recorder: MediaRecorder | null = null
let recorderStream: MediaStream | null = null
let recorderChunks: Blob[] = []
let recorderTimer: ReturnType<typeof setInterval> | null = null
let drainPollTimer: ReturnType<typeof setInterval> | null = null
let sessionPollTimer: ReturnType<typeof setInterval> | null = null
let sessionStreamAbort: AbortController | null = null
let sessionSocket: WebSocket | null = null

const objectURLs = new Set<string>()
const decorationsBySession = reactive<Record<string, Record<string, MessageDecoration>>>({})
const chatbotRef = ref<{
  setMessages?: (messages: ChatMessagesData[], mode?: 'replace' | 'prepend' | 'append') => void
  abortChat?: () => Promise<void>
  scrollList?: (params?: { behavior?: 'auto' | 'smooth'; to?: 'top' | 'bottom' }) => void
  registerMergeStrategy?: (type: string, handler: (chunk: any, existing?: any) => any) => void
  sendUserMessage?: (params: { prompt?: string; attachments?: AttachmentItem[] }) => Promise<void>
} | null>(null)
const chatbotMessages = ref<ChatMessagesData[]>([])
const streamState = ref<AssistantPhase>('idle')
const streamStatusText = ref('')
const selectedAgent = computed(() => systemStore.agents.find((item) => item.id === selectedAgentID.value) || null)
const currentSessionStatus = computed(() => String(chatStore.currentSession?.status || '').trim().toLowerCase())
const currentSessionRecovering = computed(() => currentSessionStatus.value === 'recovering')
const currentSessionRunning = computed(() => currentSessionStatus.value === 'running' || currentSessionRecovering.value)
const chatbotKey = computed(() => `${selectedAgentID.value || 'none'}:${chatStore.selectedSessionID || 'new'}:${reconnectEpoch.value}`)
const chatbotDefaultMessages = computed(() => buildEngineMessagesFromStore(chatStore.messages))
const pendingRecoveredMessageIDs = computed(() => collectPendingReplyMessageIDs(chatStore.messages))
const recoveringSession = computed(() => !serviceDraining.value && currentSessionRunning.value && !sending.value)
const assistantStageText = computed(() => {
  if (streamState.value === 'idle' && recoveringSession.value) {
    if (currentSessionRecovering.value) {
      if (passiveTransport.value === 'ws') return '正在通过 WebSocket 恢复未完成消息'
      return passiveStreamConnected.value ? '正在恢复未完成消息' : '正在等待恢复任务重新接管'
    }
    if (passiveTransport.value === 'ws') return '正在通过 WebSocket 处理会话消息'
    return passiveStreamConnected.value ? '正在处理会话消息' : '正在等待会话事件连接'
  }
  if (streamStatusText.value.trim()) return streamStatusText.value.trim()
  if (streamState.value === 'executing') return '正在执行工具'
  if (streamState.value === 'thinking') return '正在思考'
  if (streamState.value === 'typing') return '正在生成'
  return ''
})
const senderLoading = computed(() => sending.value || streamState.value !== 'idle' || recoveringSession.value)
const draftAttachmentItems = computed(() =>
  draftAttachments.value.map((attachment) => ({
    key: attachment.id,
    name: attachment.name,
    url: attachment.url,
    size: attachment.size,
    type: attachment.mime,
    raw: draftFileMap[attachment.id],
    status: 'success' as const,
    fileType: mapAttachmentFileType(attachment),
    description:
      attachment.kind === 'audio'
        ? `音频 · ${formatDuration(attachment.durationSec)}`
        : `${attachment.kind === 'image' ? '图片' : '文件'} · ${formatSize(attachment.size)}`
  }))
)
const canSend = computed(() => {
  if (!selectedAgentID.value || sending.value || streamState.value !== 'idle' || serviceDraining.value || recoveringSession.value) return false
  return !!composer.value.trim() || draftAttachments.value.length > 0 || (pollEnabled.value && pollQuestion.value.trim())
})

let finalizingStream = false

function isRecoveredPendingUserMessage(messageID: string) {
  if (activeRecoveredUserMessageID.value.trim()) {
    return activeRecoveredUserMessageID.value === messageID
  }
  return pendingRecoveredMessageIDs.value.includes(messageID)
}

function toAttachmentItems(attachments: UIAttachment[]): AttachmentItem[] {
  return attachments.map((attachment) => ({
    fileType: mapAttachmentFileType(attachment),
    size: attachment.size,
    name: attachment.name,
    url: attachment.url,
    metadata: {
      kind: attachment.kind,
      mime: attachment.mime,
      durationSec: attachment.durationSec
    }
  }))
}

function buildEngineMessageDecoration(msg: ChatMessage) {
  const parsed = parseMessageDecorationFromMetadata(msg)
  return {
    ...parsed
  }
}

function buildEngineMessagesFromStore(messages: ChatMessage[]): ChatMessagesData[] {
  return messages.map((msg) => {
    const role = normalizeRole(msg.role)
    const decoration = buildEngineMessageDecoration(msg)
    const ext = {
      decoration,
      createdAt: msg.created_at
    }

    if (role === 'user') {
      const content: UserMessage['content'] = []
      if (msg.content?.trim()) {
        content.push({
          type: 'text',
          data: msg.content
        })
      }
      if (decoration.attachments?.length) {
        content.push({
          type: 'attachment',
          data: toAttachmentItems(decoration.attachments)
        })
      }
      return {
        id: msg.id,
        role: 'user',
        datetime: formatDatetime(msg.created_at),
        content,
        status: 'complete',
        ext
      } satisfies UserMessage
    }

    if (role === 'assistant') {
      const content: AIMessage['content'] = []
      if (decoration.thinking) {
        content.push({
          type: 'thinking',
          data: {
            title: decoration.thinking.title,
            text: decoration.thinking.lines.join('\n')
          },
          status: decoration.thinking.status || 'complete'
        })
      }
      if (msg.content?.trim()) {
        content.push({
          type: 'markdown',
          data: msg.content
        })
      }
      return {
        id: msg.id,
        role: 'assistant',
        datetime: formatDatetime(msg.created_at),
        content,
        status: 'complete',
        comment: decoration.vote || '',
        ext
      } satisfies AIMessage
    }

    return {
      id: msg.id,
      role: 'system',
      datetime: formatDatetime(msg.created_at),
      content: [
        {
          type: 'text',
          data: msg.content || ''
        }
      ],
      status: 'complete',
      ext
    } as ChatMessagesData
  })
}

function engineMessageDecoration(message: ChatMessagesData): MessageDecoration {
  const base = ((message.ext as { decoration?: MessageDecoration } | undefined)?.decoration || {}) as MessageDecoration
  const override = currentSessionDecorations()?.[message.id] || {}
  const next: MessageDecoration = {
    ...base,
    ...override
  }

  if (message.role === 'assistant') {
    const thinking = (message.content || []).find((item) => item.type === 'thinking')
    if (thinking?.type === 'thinking') {
      next.thinking = {
        title: String(thinking.data?.title || base.thinking?.title || '思考过程'),
        lines: String(thinking.data?.text || '')
          .split('\n')
          .map((item) => item.trim())
          .filter(Boolean),
        collapsed: base.thinking?.collapsed ?? false,
        status: thinking.status || base.thinking?.status || 'complete'
      }
    }
  }

  if (message.role === 'user') {
    const attachmentContent = message.content.find((item) => item.type === 'attachment')
    if (attachmentContent?.type === 'attachment' && !next.attachments?.length) {
      next.attachments = attachmentContent.data.map((item, index) => ({
        id: `${message.id}_att_${index}`,
        kind: item.fileType === 'image' ? 'image' : item.fileType === 'audio' ? 'audio' : 'file',
        name: item.name || `attachment-${index + 1}`,
        size: item.size || 0,
        mime: String(item.metadata?.mime || ''),
        url: item.url || '',
        durationSec: Number(item.metadata?.durationSec) || undefined
      }))
    }
  }

  return next
}

function engineMessageText(message: ChatMessagesData) {
  return (message.content || [])
    .filter((item) => item.type === 'text' || item.type === 'markdown')
    .map((item) => String(item.data || '').trim())
    .filter(Boolean)
    .join('\n\n')
}

const timeline = computed<TimelineItem[]>(() => {
  const sourceMessages = chatbotMessages.value.length ? chatbotMessages.value : chatbotDefaultMessages.value
  const hasPendingAssistant = sourceMessages.some(
    (message) => message.role === 'assistant' && (message.status === 'pending' || message.status === 'streaming')
  )
  return sourceMessages.map((message) => ({
    id: message.id,
    role: normalizeRole(message.role),
    name: roleName(normalizeRole(message.role)),
    datetime: String(message.datetime || formatDatetime((message.ext as { createdAt?: string } | undefined)?.createdAt || '')),
    content: engineMessageText(message),
    pending:
      message.status === 'pending' ||
      message.status === 'streaming' ||
      (!hasPendingAssistant &&
        recoveringSession.value &&
        normalizeRole(message.role) === 'user' &&
        isRecoveredPendingUserMessage(message.id)),
    streamState:
      message.status === 'pending' || message.status === 'streaming'
        ? streamState.value
        : !hasPendingAssistant &&
            recoveringSession.value &&
            normalizeRole(message.role) === 'user' &&
            isRecoveredPendingUserMessage(message.id)
          ? 'thinking'
          : undefined,
    decoration: engineMessageDecoration(message)
  }))
})

const timelineMap = computed<Record<string, TimelineItem>>(() => {
  const out: Record<string, TimelineItem> = {}
  for (const item of timeline.value) {
    out[item.id] = item
  }
  return out
})

const replyTarget = computed(() => {
  if (!replyToMessageID.value) return null
  return timelineMap.value[replyToMessageID.value] || null
})

const chatbotMessageProps = computed(() => {
  return (message: ChatMessagesData) => {
    const role = normalizeRole(message.role)
    return {
      actions: false,
      name: roleName(role),
      datetime: String(message.datetime || ''),
      placement: role === 'user' ? 'right' : 'left',
      variant: message.status === 'pending' || message.status === 'streaming' ? 'text' : 'outline',
      chatContentProps: {
        thinking: {
          layout: message.status === 'pending' || message.status === 'streaming' ? 'block' : 'border',
          collapsed: false
        }
      }
    }
  }
})

const chatbotSenderProps = computed(() => ({
  disabled: true,
  actions: [],
  value: '',
  placeholder: ''
}))

const chatbotServiceConfig = computed(() => ({
  endpoint: chatStore.selectedSessionID ? `/api/sessions/${chatStore.selectedSessionID}/events` : '/api/sessions/pending/events',
  stream: true,
  timeout: 0,
  onRequest: async (params: ChatRequestParams) => {
    sending.value = true
    try {
      const sessionID = await ensureSession()
      let poll: UIPoll | null = null

      try {
        poll = parsePollDraft()
      } catch (error) {
        throw new Error((error as Error).message)
      }

      if (sendNeedConfirm.value && !window.confirm('确认发送当前消息吗？')) {
        throw new Error(REQUEST_CANCELLED)
      }

      stopPassiveSessionStream()
      stopPassiveSessionSocket()
      stopSessionPolling()
      streamState.value = 'thinking'
      streamStatusText.value = draftAttachments.value.length ? '正在上传附件' : '正在提交消息'

      const uploadedFiles: Array<{ attachment: UIAttachment; uploaded: { id: string; name?: string; mime?: string; size?: number }; transcript?: string }> = []
      const sttLines: string[] = []

      for (const attachment of draftAttachments.value) {
        const file = draftFileMap[attachment.id]
        if (!file) continue

        const uploaded = await uploadMultimodal(file)
        let transcript = ''

        if (attachment.kind === 'audio') {
          try {
            streamStatusText.value = '正在转写语音'
            const text = await speechToText(file)
            if (text.trim()) {
              transcript = text.trim()
              sttLines.push(transcript)
            }
          } catch {
            transcript = '[语音转写失败，请手动补充]'
            sttLines.push(transcript)
          }
        }

        uploadedFiles.push({
          attachment,
          uploaded,
          transcript
        })
      }

      const outboundText = composeOutboundText({
        text: String(params.prompt || ''),
        replyTo: replyTarget.value,
        sttLines
      })

      if (!outboundText.trim() && uploadedFiles.length === 0 && !poll) {
        throw new Error('请输入内容或添加附件后再发送')
      }

      const localAttachments = structuredClone(draftAttachments.value)
      patchLatestChatbotUserMessage(outboundText, localAttachments, poll, replyToMessageID.value)
      patchLatestChatbotAssistantMessage(buildPendingThinking('thinking', '正在思考中...'))

      const attachmentInputs = uploadedFiles.map((item) => ({
        id: item.uploaded.id,
        kind: item.attachment.kind,
        name: item.uploaded.name || item.attachment.name,
        mime: item.uploaded.mime || item.attachment.mime,
        size: item.uploaded.size || item.attachment.size,
        url: item.attachment.url,
        duration_sec: item.attachment.durationSec,
        transcript: item.transcript || undefined
      }))

      const created = await sendChatMessage(sessionID, {
        content: outboundText,
        auto_approve: chatStore.autoApprove,
        reply_to_id: replyToMessageID.value || undefined,
        attachments: attachmentInputs,
        poll: poll
          ? {
              question: poll.question,
              options: poll.options.map((option) => ({
                id: option.id,
                label: option.label,
                votes: option.votes
              }))
            }
          : undefined
      })

      updateMessageDecoration(created.id, {
        replyToID: replyToMessageID.value || undefined,
        attachments: localAttachments.length ? structuredClone(localAttachments) : undefined,
        poll: poll || undefined
      })

      composer.value = ''
      cancelReply()
      clearDraftAttachments()
      clearPollDraft()
      showEmojiPanel.value = false
      streamStatusText.value = '等待助手响应'

      return {
        method: 'GET',
        headers: authHeaders()
      }
    } catch (error) {
      resetLocalStreamState()
      syncChatbotFromStore()
      throw error
    } finally {
      sending.value = false
    }
  },
  onMessage: (chunk: { data?: unknown }) => {
    return handleEngineEventPayload(parseSSEPayload(chunk), 'chatbot')
  },
  onComplete: (isAborted: boolean) => {
    sending.value = false
    if (!isAborted && streamState.value !== 'idle') {
      void finalizeChatbotStream()
      return
    }
    if (!finalizingStream) {
      resetLocalStreamState()
    }
  },
  onAbort: async () => {
    sending.value = false
    if (!finalizingStream) {
      resetLocalStreamState()
    }
  },
  onError: (error: Error | Response) => {
    sending.value = false
    resetLocalStreamState()
    void chatStore.refreshMessages().finally(() => {
      syncChatbotFromStore()
    })

    if (error instanceof Response && error.status === 503) {
      handleServiceDraining('服务切换中，请等待新进程接管')
      return
    }

    const message = extractErrorMessage(error)
    if (message !== REQUEST_CANCELLED) {
      Message.error(message)
    }
  }
}))

watch(
  () => systemStore.agents,
  async (agents) => {
    if (!agents.length) {
      selectedAgentID.value = ''
      return
    }
    if (!selectedAgentID.value || !agents.some((item) => item.id === selectedAgentID.value)) {
      selectedAgentID.value = agents[0].id
      await loadSessions()
    }
  },
  { immediate: true }
)

watch(
  () => systemStore.status?.draining,
  async (draining, prevDraining) => {
    if (draining) {
      serviceDraining.value = true
      hasSeenDraining.value = true
      if (!drainNotice.value.trim()) {
        drainNotice.value = '服务切换中，请稍后重连'
      }
      startDrainPolling()
      return
    }
    if (prevDraining || serviceDraining.value || hasSeenDraining.value) {
      await handleServiceRecovered(false)
      return
    }
    serviceDraining.value = false
    drainNotice.value = ''
    stopDrainPolling()
  },
  { immediate: true }
)

watch(
  () => chatStore.selectedSessionID,
  () => {
    replyToMessageID.value = ''
    lastPassiveEventID.value = ''
    activeRecoveredUserMessageID.value = ''
    stopPassiveSessionStream()
    stopPassiveSessionSocket()
    stopSessionPolling()
    resetLocalStreamState()
    chatbotMessages.value = chatbotDefaultMessages.value
  }
)

watch(
  () => chatStore.messages,
  () => {
    if (streamState.value === 'idle' && !finalizingStream) {
      chatbotMessages.value = chatbotDefaultMessages.value
    }
  },
  { deep: true, immediate: true }
)

watch(
  () => [chatStore.selectedSessionID, currentSessionStatus.value, serviceDraining.value, sending.value, streamState.value],
  () => {
    if (shouldUsePassiveSessionStream()) {
      startSessionPolling()
      void ensurePassiveSessionStream()
      return
    }
    stopPassiveSessionStream()
    stopPassiveSessionSocket()
    if (!currentSessionRunning.value || serviceDraining.value) {
      stopSessionPolling()
    }
  },
  { immediate: true }
)

function normalizeRole(role: string): MessageRole {
  if (role === 'assistant' || role === 'user' || role === 'system') {
    return role
  }
  return 'system'
}

function roleName(role: MessageRole) {
  if (role === 'assistant') return selectedAgent.value?.name || '助手'
  if (role === 'user') return '你'
  return '系统'
}

function sessionStatusLabel(status: string) {
  switch (String(status || '').trim().toLowerCase()) {
    case 'recovering':
      return '恢复中'
    case 'running':
      return '执行中'
    case 'idle':
      return '空闲'
    default:
      return status || 'unknown'
  }
}

function applySessionStatus(sessionID: string, status: string) {
  const normalizedID = String(sessionID || '').trim()
  const normalizedStatus = String(status || '').trim()
  if (!normalizedID || !normalizedStatus) return
  const target = chatStore.sessions.find((item) => item.id === normalizedID)
  if (!target) return
  target.status = normalizedStatus
}

function normalizeAttachmentKind(value: unknown): AttachmentKind {
  const text = String(value || '').trim().toLowerCase()
  if (text === 'image' || text === 'audio' || text === 'file') {
    return text
  }
  return 'file'
}

function normalizePoll(raw: unknown, messageID: string): UIPoll | undefined {
  if (!raw || typeof raw !== 'object') return undefined
  const source = raw as Record<string, unknown>
  const question = String(source.question || '').trim()
  const rawOptions = Array.isArray(source.options) ? source.options : []
  if (!question || rawOptions.length === 0) return undefined

  const options: UIPollOption[] = []
  for (let i = 0; i < rawOptions.length; i += 1) {
    const item = rawOptions[i]
    if (!item || typeof item !== 'object') continue
    const obj = item as Record<string, unknown>
    const label = String(obj.label || '').trim()
    if (!label) continue
    const id = String(obj.id || `${messageID}_poll_${i}`)
    const voteRaw = Number(obj.votes)
    options.push({
      id,
      label,
      votes: Number.isFinite(voteRaw) && voteRaw > 0 ? Math.floor(voteRaw) : 0
    })
  }
  if (options.length === 0) return undefined

  const votedOptionID = String(source.votedOptionID || source.voted_option_id || '').trim()
  return {
    question,
    options,
    votedOptionID: votedOptionID || undefined
  }
}

function normalizeStringList(raw: unknown) {
  if (!Array.isArray(raw)) return []
  return raw.map((item) => String(item || '').trim()).filter(Boolean)
}

function normalizeThinking(raw: unknown): UIThinking | undefined {
  if (!raw || typeof raw !== 'object') return undefined
  const source = raw as Record<string, unknown>
  const lines: string[] = []
  const mode = String(source.mode || '').trim()
  const reactSteps = Number(source.react_steps)
  const observations = normalizeStringList(source.observations)

  if (mode) {
    lines.push(`执行模式：${mode}`)
  }
  if (Number.isFinite(reactSteps) && reactSteps > 0) {
    lines.push(`推理步数：${Math.floor(reactSteps)}`)
  }
  if (observations.length) {
    lines.push('观测摘要：')
    for (const item of observations) {
      lines.push(`- ${item}`)
    }
  }

  if (lines.length === 0) return undefined

  return {
    title: observations.length > 0 ? '执行与推理' : '推理摘要',
    lines,
    collapsed: observations.length > 2 || lines.join('').length > 96,
    status: 'complete'
  }
}

function collectPendingReplyMessageIDs(messages: ChatMessage[]) {
  const pending: string[] = []
  for (const msg of messages) {
    const role = normalizeRole(msg.role)
    if (role === 'user') {
      pending.push(msg.id)
      continue
    }
    if (role === 'assistant' && pending.length > 0) {
      pending.shift()
    }
  }
  return pending
}

function buildPendingThinking(state: AssistantPhase, message: string): UIThinking | undefined {
  if (state !== 'thinking' && state !== 'executing') return undefined
  const title = state === 'executing' ? '工具执行中' : '正在思考'
  const lines = [message || (state === 'executing' ? '正在执行工具并整理结果...' : '正在分析问题并规划回复...')]
  return {
    title,
    lines,
    collapsed: false,
    status: 'pending'
  }
}

function toThinkingContent(thinking: UIThinking) {
  return {
    type: 'thinking',
    data: {
      title: thinking.title,
      text: thinking.lines.join('\n')
    },
    status: thinking.status || 'pending'
  }
}

function normalizeAttachments(raw: unknown, messageID: string): UIAttachment[] {
  if (!Array.isArray(raw)) return []
  const out: UIAttachment[] = []
  for (let i = 0; i < raw.length; i += 1) {
    const item = raw[i]
    if (!item || typeof item !== 'object') continue
    const source = item as Record<string, unknown>
    const sizeRaw = Number(source.size)
    const durationRaw = Number(source.duration_sec ?? source.durationSec)
    const kind = normalizeAttachmentKind(source.kind)
    const name = String(source.name || source.file_name || `${kind}-${i + 1}`).trim()
    out.push({
      id: String(source.id || `${messageID}_att_${i}`),
      kind,
      name,
      size: Number.isFinite(sizeRaw) && sizeRaw > 0 ? sizeRaw : 0,
      mime: String(source.mime || ''),
      url: String(source.url || '').trim(),
      durationSec: Number.isFinite(durationRaw) && durationRaw > 0 ? durationRaw : undefined
    })
  }
  return out
}

function parseMessageDecorationFromMetadata(msg: ChatMessage): MessageDecoration {
  if (!msg.metadata) return {}
  try {
    const parsed = JSON.parse(msg.metadata) as Record<string, unknown>
    if (!parsed || typeof parsed !== 'object') return {}
    const out: MessageDecoration = {}
    if (typeof parsed.reply_to_id === 'string') {
      out.replyToID = parsed.reply_to_id
    }
    const attachments = normalizeAttachments(parsed.attachments, msg.id)
    if (attachments.length > 0) {
      out.attachments = attachments
    }
    const poll = normalizePoll(parsed.poll, msg.id)
    if (poll) {
      out.poll = poll
    }
    const thinking = normalizeThinking(parsed)
    if (thinking) {
      out.thinking = thinking
    }
    const vote = String(parsed.vote || parsed.feedback || '').trim()
    if (vote === 'good' || vote === 'bad') {
      out.vote = vote
    }
    return out
  } catch {
    return {}
  }
}

function currentSessionDecorations() {
  const sessionID = chatStore.selectedSessionID
  if (!sessionID) return null
  if (!decorationsBySession[sessionID]) {
    decorationsBySession[sessionID] = {}
  }
  return decorationsBySession[sessionID]
}

function updateMessageDecoration(messageID: string, patch: Partial<MessageDecoration>) {
  const bucket = currentSessionDecorations()
  if (!bucket || !messageID) return
  bucket[messageID] = {
    ...(bucket[messageID] || {}),
    ...patch
  }
}

function upsertMessageAttachment(messageID: string, attachment: UIAttachment) {
  const bucket = currentSessionDecorations()
  if (!bucket || !messageID) return
  const current = bucket[messageID] || {}
  const attachments = [...(current.attachments || [])]
  const idx = attachments.findIndex((item) => item.id === attachment.id)
  if (idx >= 0) {
    attachments[idx] = attachment
  } else {
    attachments.push(attachment)
  }
  bucket[messageID] = {
    ...current,
    attachments
  }
}

function formatDatetime(value: string) {
  if (!value) return '--'
  const date = new Date(value)
  if (Number.isNaN(date.getTime())) return value
  return date.toLocaleString()
}

function formatSize(size: number) {
  if (!Number.isFinite(size) || size <= 0) return '--'
  const units = ['B', 'KB', 'MB', 'GB']
  let cur = size
  let idx = 0
  while (cur >= 1024 && idx < units.length - 1) {
    cur /= 1024
    idx += 1
  }
  return `${cur.toFixed(cur >= 100 ? 0 : 1)} ${units[idx]}`
}

function formatDuration(sec?: number) {
  if (!sec || sec <= 0) return '00:00'
  const total = Math.round(sec)
  const m = String(Math.floor(total / 60)).padStart(2, '0')
  const s = String(total % 60).padStart(2, '0')
  return `${m}:${s}`
}

function mapAttachmentFileType(attachment: UIAttachment) {
  if (attachment.kind === 'image') return 'image'
  if (attachment.kind === 'audio') return 'audio'
  const mime = attachment.mime.toLowerCase()
  if (mime.includes('pdf')) return 'pdf'
  if (mime.includes('presentation') || mime.includes('powerpoint')) return 'ppt'
  if (mime.includes('word') || mime.includes('document')) return 'doc'
  return 'txt'
}

function avatarText(item: TimelineItem) {
  if (item.role === 'assistant') return selectedAgent.value?.emoji || 'AI'
  if (item.role === 'user') return '你'
  return '系'
}

function pendingStageText(item: TimelineItem) {
  if (!item.pending) return ''
  if (item.streamState === 'executing') return '正在执行工具'
  if (item.streamState === 'thinking') return '正在思考'
  if (item.streamState === 'typing') return '正在生成'
  return '处理中'
}

function thinkingStatus(item: TimelineItem) {
  return item.decoration.thinking?.status || (item.pending ? 'pending' : 'complete')
}

function shouldShowThinking(item: TimelineItem) {
  return item.role === 'assistant' && !!item.decoration.thinking
}

function showActionbar(item: TimelineItem) {
  return !item.pending && !!item.content.trim()
}

function actionbarItems(item: TimelineItem) {
  if (item.role === 'assistant') {
    return ['copy', 'good', 'bad', 'replay'] as const
  }
  return ['copy', 'replay'] as const
}

function makeID(prefix = 'id') {
  const random = Math.random().toString(36).slice(2, 10)
  return `${prefix}_${Date.now()}_${random}`
}

function appendEmoji(emoji: string) {
  composer.value += emoji
  showEmojiPanel.value = false
}

async function getAudioDuration(url: string): Promise<number> {
  return await new Promise((resolve) => {
    const audio = new Audio()
    audio.preload = 'metadata'
    audio.src = url
    audio.onloadedmetadata = () => {
      resolve(Number.isFinite(audio.duration) ? audio.duration : 0)
    }
    audio.onerror = () => resolve(0)
  })
}

async function addDraftFiles(files: File[]) {
  for (const file of files) {
    const kind: AttachmentKind = file.type.startsWith('image/')
      ? 'image'
      : file.type.startsWith('audio/')
        ? 'audio'
        : 'file'
    const id = makeID('att')
    const url = URL.createObjectURL(file)
    objectURLs.add(url)

    const attachment: UIAttachment = {
      id,
      kind,
      name: file.name,
      size: file.size,
      mime: file.type || 'application/octet-stream',
      url
    }

    if (kind === 'audio') {
      attachment.durationSec = await getAudioDuration(url)
    }

    draftAttachments.value.push(attachment)
    draftFileMap[id] = file
  }
}

function removeDraftAttachment(id: string) {
  const idx = draftAttachments.value.findIndex((item) => item.id === id)
  if (idx < 0) return
  const removed = draftAttachments.value[idx]
  if (removed?.url && objectURLs.has(removed.url)) {
    URL.revokeObjectURL(removed.url)
    objectURLs.delete(removed.url)
  }
  draftAttachments.value.splice(idx, 1)
  delete draftFileMap[id]
}

function clearDraftAttachments() {
  const ids = draftAttachments.value.map((item) => item.id)
  for (const id of ids) {
    removeDraftAttachment(id)
  }
}

async function handleSenderSend(value: string) {
  if (!canSend.value) return
  await chatbotRef.value?.sendUserMessage?.({
    prompt: String(value || composer.value || ''),
    attachments: toAttachmentItems(draftAttachments.value)
  })
}

async function handleSenderStop() {
  await chatbotRef.value?.abortChat?.()
}

function fileSignature(file: File) {
  return `${file.name}:${file.size}:${file.lastModified}`
}

function hasDraftFile(file: File) {
  return Object.values(draftFileMap).some((item) => fileSignature(item) === fileSignature(file))
}

async function handleChatbotFileSelect(event: CustomEvent<Array<{ raw?: File }>>) {
  const items = Array.isArray(event?.detail) ? event.detail : []
  const files = items.map((item) => item.raw).filter((item): item is File => item instanceof File)
  const nextFiles = files.filter((file) => !hasDraftFile(file))
  if (!nextFiles.length) return
  await addDraftFiles(nextFiles)
}

function handleChatbotFileRemove(event: CustomEvent<Array<{ key?: string }>>) {
  const items = Array.isArray(event?.detail) ? event.detail : []
  const remaining = new Set(items.map((item) => String(item.key || '').trim()).filter(Boolean))
  for (const attachment of [...draftAttachments.value]) {
    if (!remaining.has(attachment.id)) {
      removeDraftAttachment(attachment.id)
    }
  }
}

function setChatbotMessages(messages: ChatMessagesData[]) {
  chatbotMessages.value = messages
  chatbotRef.value?.setMessages?.(messages, 'replace')
}

function syncChatbotFromStore() {
  setChatbotMessages(buildEngineMessagesFromStore(chatStore.messages))
}

function patchChatbotMessages(mutator: (messages: ChatMessagesData[]) => void) {
  const sourceMessages = chatbotMessages.value.length ? chatbotMessages.value : chatbotDefaultMessages.value
  const nextMessages = structuredClone(sourceMessages) as ChatMessagesData[]
  mutator(nextMessages)
  setChatbotMessages(nextMessages)
}

function patchLatestChatbotUserMessage(content: string, attachments: UIAttachment[], poll: UIPoll | null, replyToID: string) {
  patchChatbotMessages((messages) => {
    const target = [...messages].reverse().find((item) => item.role === 'user') as UserMessage | undefined
    if (!target) return
    const nextContent: UserMessage['content'] = []
    if (content.trim()) {
      nextContent.push({
        type: 'text',
        data: content
      })
    }
    if (attachments.length) {
      nextContent.push({
        type: 'attachment',
        data: toAttachmentItems(attachments)
      })
    }
    target.content = nextContent
    target.ext = {
      ...(target.ext || {}),
      decoration: {
        ...(((target.ext as { decoration?: MessageDecoration } | undefined)?.decoration || {}) as MessageDecoration),
        replyToID: replyToID || undefined,
        attachments: attachments.length ? structuredClone(attachments) : undefined,
        poll: poll || undefined
      }
    }
  })
}

function patchLatestChatbotAssistantMessage(thinking?: UIThinking) {
  patchChatbotMessages((messages) => {
    const target = [...messages].reverse().find((item) => item.role === 'assistant') as AIMessage | undefined
    if (!target) return
    target.ext = {
      ...(target.ext || {}),
      decoration: {
        ...(((target.ext as { decoration?: MessageDecoration } | undefined)?.decoration || {}) as MessageDecoration),
        thinking
      }
    }
  })
}

function ensureStreamingAssistantMessage(thinking?: UIThinking) {
  patchChatbotMessages((messages) => {
    let target = [...messages].reverse().find(
      (item) => item.role === 'assistant' && (item.status === 'pending' || item.status === 'streaming')
    ) as AIMessage | undefined

    if (!target) {
      target = {
        id: makeID('assistant_stream'),
        role: 'assistant',
        datetime: formatDatetime(new Date().toISOString()),
        content: [],
        status: 'pending',
        ext: {
          decoration: {}
        }
      } satisfies AIMessage
      messages.push(target)
    }

    const nextContent = (target.content || []).filter((item) => item.type !== 'thinking' && item.type !== 'markdown')
    if (thinking) {
      nextContent.unshift(toThinkingContent(thinking))
    }
    target.content = nextContent
    target.status = 'pending'
    target.ext = {
      ...(target.ext || {}),
      decoration: {
        ...(((target.ext as { decoration?: MessageDecoration } | undefined)?.decoration || {}) as MessageDecoration),
        thinking
      }
    }
  })
}

function appendStreamingAssistantChunk(text: string) {
  if (!text) return
  patchChatbotMessages((messages) => {
    let target = [...messages].reverse().find(
      (item) => item.role === 'assistant' && (item.status === 'pending' || item.status === 'streaming')
    ) as AIMessage | undefined

    if (!target) {
      target = {
        id: makeID('assistant_stream'),
        role: 'assistant',
        datetime: formatDatetime(new Date().toISOString()),
        content: [],
        status: 'streaming',
        ext: {
          decoration: {}
        }
      } satisfies AIMessage
      messages.push(target)
    }

    const content = [...(target.content || [])]
    const markdownIndex = content.findIndex((item) => item.type === 'markdown')
    if (markdownIndex >= 0) {
      const current = content[markdownIndex]
      content[markdownIndex] = {
        ...current,
        data: `${String(current.data || '')}${text}`,
        status: 'streaming'
      }
    } else {
      content.push({
        type: 'markdown',
        data: text,
        status: 'streaming'
      })
    }
    target.content = content
    target.status = 'streaming'
  })
}

function parseSSEPayload(chunk: { data?: unknown }) {
  const raw = chunk?.data
  if (typeof raw !== 'string') return raw
  try {
    return JSON.parse(raw) as { type?: string; data?: Record<string, unknown> | string }
  } catch {
    return raw
  }
}

function parseSSEFrame(raw: string) {
  const lines = raw.split(/\r?\n/)
  let event = 'message'
  let id = ''
  const data: string[] = []
  for (const line of lines) {
    if (!line || line.startsWith(':')) continue
    if (line.startsWith('id:')) {
      id = line.slice(3).trim()
      continue
    }
    if (line.startsWith('event:')) {
      event = line.slice(6).trim() || 'message'
      continue
    }
    if (line.startsWith('data:')) {
      data.push(line.slice(5).trimStart())
    }
  }
  if (!data.length) return null
  return {
    event,
    id,
    data: data.join('\n')
  }
}

function authHeaders() {
  const token = localStorage.getItem('xclaw_token')
  return token ? { Authorization: `Bearer ${token}` } : {}
}

async function refreshDrainStatus() {
  try {
    await systemStore.refreshStatus()
    if (!systemStore.status?.draining) {
      await handleServiceRecovered(true)
    }
  } catch {
    // ignore transient failures while service is restarting
  }
}

async function refreshCurrentSessionProgress() {
  if (!selectedAgentID.value || !chatStore.selectedSessionID || serviceDraining.value) return
  try {
    await chatStore.refreshSessions(selectedAgentID.value)
    if (!currentSessionRunning.value) {
      stopSessionPolling()
      if (!sending.value && streamState.value === 'idle') {
        syncChatbotFromStore()
      }
    }
  } catch {
    // ignore transient refresh failures while runtime is settling
  }
}

function startSessionPolling() {
  if (sessionPollTimer || !currentSessionRunning.value || serviceDraining.value) return
  sessionPollTimer = setInterval(() => {
    void refreshCurrentSessionProgress()
  }, 2000)
}

function stopSessionPolling() {
  if (!sessionPollTimer) return
  clearInterval(sessionPollTimer)
  sessionPollTimer = null
}

function startDrainPolling() {
  if (drainPollTimer) return
  drainPollTimer = setInterval(() => {
    void refreshDrainStatus()
  }, 2000)
}

function stopDrainPolling() {
  if (!drainPollTimer) return
  clearInterval(drainPollTimer)
  drainPollTimer = null
}

function handleServiceDraining(message = '服务切换中，请稍后重连') {
  serviceDraining.value = true
  hasSeenDraining.value = true
  drainNotice.value = message
  stopPassiveSessionStream()
  stopPassiveSessionSocket()
  stopSessionPolling()
  resetLocalStreamState()
  void chatStore.refreshMessages().finally(() => {
    syncChatbotFromStore()
  })
  void refreshDrainStatus()
  startDrainPolling()
}

async function handleServiceRecovered(notify: boolean) {
  const wasDraining = serviceDraining.value || hasSeenDraining.value
  serviceDraining.value = false
  drainNotice.value = ''
  stopDrainPolling()
  await Promise.allSettled([
    systemStore.refreshStatus(),
    selectedAgentID.value ? chatStore.refreshSessions(selectedAgentID.value) : Promise.resolve(),
    chatStore.selectedSessionID ? chatStore.refreshMessages() : Promise.resolve()
  ])
  syncChatbotFromStore()
  reconnectEpoch.value += 1
  await nextTick()
  if (currentSessionRunning.value) {
    startSessionPolling()
    void ensurePassiveSessionStream()
  }
  void scrollChatToBottom()
  if (notify && wasDraining) {
    // recovery success is surfaced globally from App.vue to avoid duplicate popups
  }
  hasSeenDraining.value = false
}

function setReply(messageID: string) {
  replyToMessageID.value = messageID
}

function cancelReply() {
  replyToMessageID.value = ''
}

function parsePollDraft() {
  if (!pollEnabled.value) return null
  const question = pollQuestion.value.trim()
  if (!question) {
    throw new Error('投票问题不能为空')
  }
  const options = pollOptionsText.value
    .split('\n')
    .map((item) => item.trim())
    .filter(Boolean)
  if (options.length < 2) {
    throw new Error('投票至少需要 2 个选项')
  }
  return {
    question,
    options: options.map((label) => ({ id: makeID('opt'), label, votes: 0 }))
  } as UIPoll
}

function clearPollDraft() {
  pollEnabled.value = false
  pollQuestion.value = ''
  pollOptionsText.value = '赞成\n反对'
}

function quoteSnippet(item: TimelineItem) {
  const text = item.content.trim()
  if (text) return text.slice(0, 180)
  if (item.decoration.attachments?.length) {
    return `附件 ${item.decoration.attachments.length} 个`
  }
  if (item.decoration.poll) {
    return `投票：${item.decoration.poll.question}`
  }
  return '空消息'
}

function buildQuotePreviewMessages(item: TimelineItem | null): ChatMessagesData[] {
  if (!item) return []
  const previewText = quoteSnippet(item)
  return [
    {
      id: `${item.id}_quote_preview`,
      role: item.role,
      datetime: item.datetime,
      status: 'complete',
      content: [
        {
          type: item.role === 'assistant' ? 'markdown' : 'text',
          data: previewText
        }
      ]
    } as ChatMessagesData
  ]
}

function composeOutboundText(params: {
  text: string
  replyTo?: TimelineItem | null
  sttLines: string[]
}) {
  const lines: string[] = []

  if (params.replyTo) {
    lines.push(`> 引用 ${params.replyTo.name}：${quoteSnippet(params.replyTo)}`)
  }

  if (params.text.trim()) {
    lines.push(params.text.trim())
  }

  if (params.sttLines.length) {
    lines.push('', '### 语音转写', ...params.sttLines.map((item) => `- ${item}`))
  }

  return lines.join('\n').trim()
}

async function startRecording() {
  if (!canRecordAudio.value) {
    Message.warning('当前浏览器不支持录音')
    return
  }
  if (isRecording.value) return

  try {
    recorderStream = await navigator.mediaDevices.getUserMedia({ audio: true })
    recorderChunks = []
    recorder = new MediaRecorder(recorderStream)
    recorder.ondataavailable = (event) => {
      if (event.data.size > 0) {
        recorderChunks.push(event.data)
      }
    }
    recorder.onstop = async () => {
      const blobType = recorder?.mimeType || 'audio/webm'
      const blob = new Blob(recorderChunks, { type: blobType })
      const file = new File([blob], `voice-${new Date().toISOString().replace(/[:.]/g, '-')}.webm`, { type: blobType })
      await addDraftFiles([file])
      recorderChunks = []

      if (recorderStream) {
        for (const track of recorderStream.getTracks()) {
          track.stop()
        }
      }
      recorderStream = null
      recorder = null
    }

    recorder.start(200)
    isRecording.value = true
    recordingSeconds.value = 0
    recorderTimer = setInterval(() => {
      recordingSeconds.value += 1
    }, 1000)
  } catch (error) {
    Message.error((error as Error).message || '录音启动失败')
  }
}

function stopRecording() {
  if (!isRecording.value) return
  isRecording.value = false
  if (recorderTimer) {
    clearInterval(recorderTimer)
    recorderTimer = null
  }
  recorder?.stop()
}

function setMessageVote(messageID: string, vote: MessageVote) {
  const current = timelineMap.value[messageID]?.decoration.vote || ''
  const next: MessageVote = current === vote ? '' : vote
  updateMessageDecoration(messageID, { vote: next })
}

function replaySource(messageID: string) {
  const index = timeline.value.findIndex((item) => item.id === messageID)
  if (index < 0) return null
  for (let i = index; i >= 0; i -= 1) {
    const item = timeline.value[i]
    if (item.role === 'user') {
      return item
    }
  }
  return null
}

function handleActionbar(messageID: string, action: string) {
  if (action === 'good') {
    setMessageVote(messageID, 'good')
    return
  }
  if (action === 'bad') {
    setMessageVote(messageID, 'bad')
    return
  }
  if (action === 'replay') {
    const source = replaySource(messageID)
    if (!source) {
      Message.warning('未找到可重试的用户消息')
      return
    }
    composer.value = source.content
    if (source.decoration.replyToID) {
      replyToMessageID.value = source.decoration.replyToID
    }
    Message.info('已回填对应用户消息，发送即可重试')
  }
}

function votePoll(messageID: string, optionID: string) {
  const item = timelineMap.value[messageID]
  if (!item?.decoration.poll) return

  const poll = structuredClone(item.decoration.poll)
  const prev = poll.votedOptionID
  if (prev === optionID) return

  if (prev) {
    const prevOption = poll.options.find((it) => it.id === prev)
    if (prevOption && prevOption.votes > 0) {
      prevOption.votes -= 1
    }
  }

  const nextOption = poll.options.find((it) => it.id === optionID)
  if (!nextOption) return
  nextOption.votes += 1
  poll.votedOptionID = optionID

  updateMessageDecoration(messageID, { poll })
}

async function readAssistantMessage(messageID: string) {
  const item = timelineMap.value[messageID]
  if (!item || item.role !== 'assistant' || !item.content.trim()) return

  speakingMessageID.value = messageID
  try {
    const tts = await textToSpeech({ text: item.content.slice(0, 3000) })
    const binary = atob(tts.audio_base64)
    const bytes = new Uint8Array(binary.length)
    for (let i = 0; i < binary.length; i += 1) {
      bytes[i] = binary.charCodeAt(i)
    }
    const blob = new Blob([bytes], { type: tts.mime || 'audio/mpeg' })
    const url = URL.createObjectURL(blob)
    objectURLs.add(url)
    const durationSec = await getAudioDuration(url)

    upsertMessageAttachment(messageID, {
      id: `tts_${Date.now()}`,
      kind: 'audio',
      name: 'assistant-voice.mp3',
      size: blob.size,
      mime: tts.mime || 'audio/mpeg',
      url,
      durationSec
    })

    await nextTick()
    const player = document.getElementById(`audio-${messageID}`) as HTMLAudioElement | null
    player?.play().catch(() => {
      // ignore autoplay blocks
    })
  } catch (error) {
    Message.error((error as Error).message || '生成语音失败')
  } finally {
    speakingMessageID.value = ''
  }
}

async function loadSessions() {
  if (!selectedAgentID.value) return
  await chatStore.refreshSessions(selectedAgentID.value)
  if (!chatStore.selectedSessionID) {
    await createSession()
  }
}

async function createSession() {
  if (!selectedAgentID.value) return
  await chatStore.createSessionForAgent(selectedAgentID.value, `会话 ${new Date().toLocaleString()}`)
}

async function ensureSession() {
  if (chatStore.selectedSessionID) return chatStore.selectedSessionID
  await createSession()
  if (!chatStore.selectedSessionID) {
    throw new Error('会话创建失败')
  }
  return chatStore.selectedSessionID
}

function getReplyPreview(item: TimelineItem) {
  const replyID = item.decoration.replyToID
  if (!replyID) return null
  return timelineMap.value[replyID] || null
}

function extractErrorMessage(error: Error | Response) {
  if (error instanceof Response) {
    return `请求失败（${error.status}）`
  }
  return error.message || '发送失败'
}

function resetLocalStreamState() {
  streamState.value = 'idle'
  streamStatusText.value = ''
}

function handleEngineEventPayload(
  rawPayload: unknown,
  source: 'chatbot' | 'passive'
): { type: 'thinking' | 'markdown'; data: unknown; status?: string } | null {
  const payload = rawPayload as { type?: string; data?: Record<string, unknown>; session_id?: string } | string | null
  if (!payload || typeof payload === 'string') return null

  const payloadSessionID = String(payload.session_id || chatStore.selectedSessionID || '').trim()

  if (payload.type === 'session.status') {
    const data = (payload.data || {}) as { status?: string }
    applySessionStatus(payloadSessionID, String(data.status || '').trim())
    return null
  }

  if (payload.type === 'assistant.start') {
    const data = (payload.data || {}) as { user_message_id?: string; resume?: boolean }
    const resumedMessageID = String(data.user_message_id || '').trim()
    applySessionStatus(payloadSessionID, data.resume ? 'recovering' : 'running')
    if (data.resume && resumedMessageID) {
      activeRecoveredUserMessageID.value = resumedMessageID
    } else if (!data.resume) {
      activeRecoveredUserMessageID.value = ''
    }
    streamState.value = 'thinking'
    streamStatusText.value = '正在思考中...'
    const thinking = buildPendingThinking('thinking', streamStatusText.value)
    if (source === 'chatbot') {
      patchLatestChatbotAssistantMessage(thinking)
      return thinking ? toThinkingContent(thinking) : null
    }
    ensureStreamingAssistantMessage(thinking)
    return null
  }

  if (payload.type === 'presence') {
    const data = (payload.data || {}) as { state?: string; message?: string }
    const state = String(data.state || '').trim()
    const message = String(data.message || '').trim()
    if (state === 'thinking' || state === 'executing' || state === 'typing' || state === 'idle') {
      streamState.value = state
    }
    streamStatusText.value = message
    if (streamState.value === 'thinking' || streamState.value === 'executing') {
      const thinking = buildPendingThinking(streamState.value, message)
      if (source === 'chatbot') {
        patchLatestChatbotAssistantMessage(thinking)
        return thinking ? toThinkingContent(thinking) : null
      }
      ensureStreamingAssistantMessage(thinking)
    }
    return null
  }

  if (payload.type === 'assistant.delta') {
    streamState.value = 'typing'
    const text = String((payload.data as { chunk?: string } | undefined)?.chunk || '')
    if (!text) return null
    if (source === 'chatbot') {
      return {
        type: 'markdown',
        data: text,
        status: 'streaming'
      }
    }
    appendStreamingAssistantChunk(text)
    return null
  }

  if (payload.type === 'assistant.done') {
    applySessionStatus(payloadSessionID, 'idle')
    activeRecoveredUserMessageID.value = ''
    void finalizeChatbotStream()
    return null
  }

  if (payload.type === 'shutdown') {
    const data = (payload.data || {}) as { state?: string; message?: string }
    if (String(data.state || '').trim() === 'draining') {
      handleServiceDraining(String(data.message || '').trim() || undefined)
    }
    return null
  }

  if (payload.type === 'error') {
    applySessionStatus(payloadSessionID, 'idle')
    activeRecoveredUserMessageID.value = ''
    resetLocalStreamState()
    return null
  }

  return null
}

async function finalizeChatbotStream() {
  if (finalizingStream) return
  finalizingStream = true
  try {
    activeRecoveredUserMessageID.value = ''
    await chatbotRef.value?.abortChat?.()
    await chatStore.refreshMessages()
    syncChatbotFromStore()
    resetLocalStreamState()
    await nextTick()
    chatbotRef.value?.scrollList?.({ to: 'bottom', behavior: 'smooth' })
  } finally {
    finalizingStream = false
  }
}

function shouldUsePassiveSessionStream() {
  return !!chatStore.selectedSessionID && currentSessionRunning.value && !serviceDraining.value && !sending.value && streamState.value === 'idle'
}

function stopPassiveSessionStream() {
  if (!sessionStreamAbort) return
  sessionStreamAbort.abort()
  sessionStreamAbort = null
  passiveStreamConnected.value = false
  passiveTransport.value = 'idle'
}

function stopPassiveSessionSocket() {
  if (!sessionSocket) return
  const socket = sessionSocket
  sessionSocket = null
  passiveStreamConnected.value = false
  passiveTransport.value = 'idle'
  try {
    socket.close()
  } catch {
    // ignore close errors
  }
}

function buildPassiveSessionURL(sessionID: string, transport: 'sse' | 'ws') {
  const path = transport === 'ws' ? `/api/sessions/${sessionID}/ws` : `/api/sessions/${sessionID}/events`
  const url = new URL(path, window.location.origin)
  if (lastPassiveEventID.value.trim()) {
    url.searchParams.set('since_id', lastPassiveEventID.value.trim())
  }
  if (transport === 'ws') {
    url.protocol = url.protocol === 'https:' ? 'wss:' : 'ws:'
    const token = localStorage.getItem('xclaw_token')
    if (token) {
      url.searchParams.set('access_token', token)
    }
  }
  return url
}

async function ensurePassiveSessionStream() {
  if (!shouldUsePassiveSessionStream() || sessionStreamAbort || sessionSocket) return

  const sessionID = chatStore.selectedSessionID
  if (!sessionID) return

  const controller = new AbortController()
  sessionStreamAbort = controller
  passiveStreamConnected.value = false
  passiveTransport.value = 'sse'
  const url = buildPassiveSessionURL(sessionID, 'sse')

  try {
    const response = await fetch(url.toString(), {
      method: 'GET',
      headers: authHeaders(),
      signal: controller.signal
    })
    if (response.status === 503) {
      handleServiceDraining('服务切换中，请等待新进程接管')
      return
    }
    if (!response.ok || !response.body) {
      throw new Error(`订阅会话事件失败（${response.status}）`)
    }

    passiveStreamConnected.value = true
    passiveTransport.value = 'sse'
    const reader = response.body.getReader()
    const decoder = new TextDecoder()
    let buffer = ''

    for (;;) {
      const { value, done } = await reader.read()
      if (done) break
      buffer += decoder.decode(value, { stream: true })

      let boundary = buffer.indexOf('\n\n')
      while (boundary >= 0) {
        const frame = parseSSEFrame(buffer.slice(0, boundary))
        buffer = buffer.slice(boundary + 2)
        if (frame?.data) {
          if (frame.id) {
            lastPassiveEventID.value = frame.id
          }
          handleEngineEventPayload(parseSSEPayload({ data: frame.data }), 'passive')
        }
        boundary = buffer.indexOf('\n\n')
      }
    }
  } catch (error) {
    if (controller.signal.aborted) return
    if (currentSessionRunning.value && !serviceDraining.value) {
      startSessionPolling()
      ensurePassiveSessionSocket()
    }
    const message = extractErrorMessage(error as Error)
    if (!message.includes('The operation was aborted')) {
      console.warn(message)
    }
  } finally {
    if (sessionStreamAbort === controller) {
      sessionStreamAbort = null
      passiveStreamConnected.value = false
      if (passiveTransport.value === 'sse') {
        passiveTransport.value = 'idle'
      }
    }
    if (shouldUsePassiveSessionStream() && !sessionSocket) {
      setTimeout(() => {
        void ensurePassiveSessionStream()
      }, 1200)
    }
  }
}

function ensurePassiveSessionSocket() {
  if (!shouldUsePassiveSessionStream() || sessionSocket || sessionStreamAbort) return

  const sessionID = chatStore.selectedSessionID
  if (!sessionID) return

  const url = buildPassiveSessionURL(sessionID, 'ws')
  const socket = new WebSocket(url.toString())
  sessionSocket = socket
  passiveTransport.value = 'ws'

  socket.onopen = () => {
    if (sessionSocket !== socket) return
    passiveStreamConnected.value = true
    passiveTransport.value = 'ws'
  }

  socket.onmessage = (event) => {
    if (sessionSocket !== socket) return
    const payload = parseSSEPayload({ data: String(event.data || '') }) as
      | { type?: string; sequence?: number; data?: Record<string, unknown>; session_id?: string }
      | string
      | null
    if (!payload || typeof payload === 'string') return
    const sequence = Number((payload as { sequence?: number }).sequence)
    if (Number.isFinite(sequence) && sequence > 0) {
      lastPassiveEventID.value = String(Math.floor(sequence))
    }
    handleEngineEventPayload(payload, 'passive')
  }

  socket.onerror = () => {
    if (sessionSocket !== socket) return
    passiveStreamConnected.value = false
  }

  socket.onclose = () => {
    if (sessionSocket !== socket) return
    sessionSocket = null
    passiveStreamConnected.value = false
    passiveTransport.value = 'idle'
    if (shouldUsePassiveSessionStream()) {
      setTimeout(() => {
        void ensurePassiveSessionStream()
      }, 1200)
    }
  }
}

async function scrollChatToBottom() {
  await nextTick()
  chatbotRef.value?.scrollList?.({
    behavior: 'smooth',
    to: 'bottom'
  })
}

function handleChatbotMessageChange(event: CustomEvent<ChatMessagesData[]>) {
  chatbotMessages.value = Array.isArray(event.detail) ? event.detail : []
}

function registerChatbotStrategies() {
  chatbotRef.value?.registerMergeStrategy?.('markdown', (chunk: { data?: string }, existing?: { data?: string }) => ({
    ...chunk,
    data: `${existing?.data || ''}${chunk.data || ''}`
  }))
  chatbotRef.value?.registerMergeStrategy?.('thinking', (chunk: { data?: { title?: string; text?: string }; status?: string }) => ({
    ...chunk,
    data: {
      title: chunk.data?.title || '思考过程',
      text: chunk.data?.text || ''
    }
  }))
}

function handleChatbotReady() {
  registerChatbotStrategies()
  scrollChatToBottom()
}

watch(
  () => [timeline.value.length, streamState.value, streamStatusText.value, chatStore.selectedSessionID],
  () => {
    scrollChatToBottom()
  }
)

onMounted(() => {
  scrollChatToBottom()
})

onBeforeUnmount(() => {
  void chatbotRef.value?.abortChat?.()
  stopPassiveSessionStream()
  stopPassiveSessionSocket()
  stopDrainPolling()
  stopSessionPolling()
  stopRecording()
  if (recorderTimer) {
    clearInterval(recorderTimer)
    recorderTimer = null
  }
  for (const url of objectURLs) {
    URL.revokeObjectURL(url)
  }
  objectURLs.clear()
})
</script>

<template>
  <div class="page chat-page">
    <div class="page-header">
      <div>
        <h2>聊天助手</h2>
        <p>基于 TDesign Chatbot / ChatEngine / ChatList / ChatSender / ChatMessage 体系构建，支持文本、语音、附件、引用、投票、Markdown 与流式思考。</p>
      </div>
    </div>

    <a-alert v-if="serviceDraining" type="warning" style="margin-bottom: 16px">
      {{ drainNotice || '服务切换中，请等待新进程接管后再继续对话。' }}
    </a-alert>

    <a-alert v-else-if="recoveringSession" type="info" style="margin-bottom: 16px">
      {{
        currentSessionRecovering
          ? '服务已恢复，正在继续处理上次未完成的消息。当前输入暂时锁定，待本轮恢复完成后可继续对话。'
          : '当前会话仍在执行中，输入暂时锁定；等待本轮任务完成后可继续对话。'
      }}
    </a-alert>

    <a-row :gutter="16" style="margin-bottom: 16px">
      <a-col :xs="24" :md="8">
        <div class="form-group">
          <label class="form-label">选择助手</label>
          <a-select v-model="selectedAgentID" :disabled="serviceDraining" placeholder="选择助手" size="large" @change="loadSessions">
            <a-option v-for="item in systemStore.agents" :key="item.id" :value="item.id">
              {{ item.emoji }} {{ item.name }}
            </a-option>
          </a-select>
        </div>
      </a-col>
      <a-col :xs="24" :md="10">
        <div class="form-group">
          <label class="form-label">选择会话</label>
          <a-select v-model="chatStore.selectedSessionID" :disabled="serviceDraining" placeholder="选择会话" size="large" @change="chatStore.refreshMessages">
            <a-option v-for="item in chatStore.sessions" :key="item.id" :value="item.id">
              {{ item.title }}（{{ sessionStatusLabel(item.status) }}）
            </a-option>
          </a-select>
        </div>
      </a-col>
      <a-col :xs="24" :md="6">
        <div class="form-group">
          <label class="form-label">&nbsp;</label>
          <a-button type="primary" :disabled="serviceDraining" long size="large" @click="createSession">新建会话</a-button>
        </div>
      </a-col>
    </a-row>

    <a-row :gutter="16" style="margin-bottom: 12px">
      <a-col :xs="24" :md="12">
        <a-checkbox v-model="chatStore.autoApprove">自动通过高风险操作（仅高级用户）</a-checkbox>
      </a-col>
      <a-col :xs="24" :md="12">
        <a-checkbox v-model="sendNeedConfirm">发送前询问确认</a-checkbox>
      </a-col>
    </a-row>

    <div class="x-chat-shell">
      <div class="x-chat-stage" v-if="streamState !== 'idle' || recoveringSession">
        <span class="x-chat-stage-dot" />
        <span>{{ assistantStageText }}</span>
      </div>

      <div v-if="!timeline.length" class="x-empty-chat">
        <t-chat-loading animation="gradient" text="等待开始新会话" />
      </div>

      <t-chatbot
        ref="chatbotRef"
        :key="chatbotKey"
        class="x-chatbot"
        layout="both"
        :default-messages="chatbotDefaultMessages"
        :inject-css="chatbotInjectCSS"
        :list-props="{ autoScroll: true, defaultScrollTo: 'bottom' }"
        :message-props="chatbotMessageProps"
        :sender-props="chatbotSenderProps"
        :chat-service-config="chatbotServiceConfig"
        @chat-ready="handleChatbotReady"
        @message-change="handleChatbotMessageChange"
      >
        <template v-for="item in timeline" :key="`${item.id}-avatar`" v-slot:[`${item.id}-avatar`]>
          <div class="x-message-avatar" :class="`is-${item.role}`">
            {{ avatarText(item) }}
          </div>
        </template>

        <template v-for="item in timeline" :key="`${item.id}-content`" v-slot:[`${item.id}-content`]>
          <div class="x-message-content">
            <t-chat-thinking
              v-if="shouldShowThinking(item)"
              class="x-thinking-card"
              :status="thinkingStatus(item)"
              :collapsed="item.decoration.thinking?.collapsed"
              :layout="item.pending ? 'block' : 'border'"
            >
              <div class="x-thinking-body">
                <div class="x-thinking-title">{{ item.decoration.thinking?.title }}</div>
                <div v-for="line in item.decoration.thinking?.lines || []" :key="line" class="x-thinking-line">
                  {{ line }}
                </div>
              </div>
            </t-chat-thinking>

            <div v-if="getReplyPreview(item)" class="x-reply-block">
              <div class="x-reply-label">引用消息</div>
              <t-chat-list
                class="x-quote-list"
                layout="both"
                :auto-scroll="false"
                :show-scroll-button="false"
                :data="buildQuotePreviewMessages(getReplyPreview(item))"
              />
            </div>

            <template v-if="item.content.trim()">
              <t-chat-markdown v-if="item.role === 'assistant'" :content="item.content || ' '" />
              <t-chat-content v-else :role="item.role" :content="{ type: 'markdown', data: item.content || ' ' }" />
              <div v-if="item.pending" class="x-inline-loading">
                <t-chat-loading animation="moving" :text="pendingStageText(item)" />
              </div>
            </template>
            <div v-else-if="item.pending" class="x-inline-loading">
              <t-chat-loading animation="gradient" :text="pendingStageText(item)" />
            </div>

            <div v-if="item.decoration.attachments?.length" class="x-attachment-list">
              <div class="x-attachment-title">附件</div>
              <t-attachments
                v-if="item.decoration.attachments.some((att) => att.kind !== 'audio')"
                :items="
                  item.decoration.attachments
                    .filter((att) => att.kind !== 'audio')
                    .map((att) => ({
                      key: att.id,
                      name: att.name,
                      url: att.url,
                      size: att.size,
                      type: att.mime,
                      status: 'success',
                      fileType: mapAttachmentFileType(att),
                      description: `${att.kind === 'image' ? '图片' : '文件'} · ${formatSize(att.size)}`
                    }))
                "
                overflow="wrap"
                class="x-message-attachments"
              />
              <div v-for="att in item.decoration.attachments" :key="att.id" class="x-attachment-item">
                <template v-if="att.kind === 'audio' && att.url">
                  <audio :id="`audio-${item.id}`" :src="att.url" controls preload="metadata" class="x-audio-player" />
                  <div class="x-attachment-meta">{{ att.name }} · {{ formatDuration(att.durationSec) }}</div>
                </template>
              </div>
            </div>

            <div v-if="item.decoration.poll" class="x-poll-card">
              <div class="x-poll-title">🗳 {{ item.decoration.poll.question }}</div>
              <a-space direction="vertical" fill>
                <a-button
                  v-for="opt in item.decoration.poll.options"
                  :key="opt.id"
                  long
                  size="small"
                  :type="item.decoration.poll.votedOptionID === opt.id ? 'primary' : 'outline'"
                  @click="votePoll(item.id, opt.id)"
                >
                  {{ opt.label }}（{{ opt.votes }}）
                </a-button>
              </a-space>
            </div>
          </div>
        </template>

        <template v-for="item in timeline" :key="`${item.id}-actionbar`" v-slot:[`${item.id}-actionbar`]>
          <div v-if="showActionbar(item)" class="x-message-actions">
            <t-chat-actionbar
              class="x-message-actionbar"
              :content="item.content"
              :comment="item.decoration.vote || ''"
              :action-bar="actionbarItems(item)"
              @actions="(value) => handleActionbar(item.id, String(value))"
            />
            <div class="x-message-action-extras">
              <a-button size="mini" @click="setReply(item.id)">引用</a-button>
              <a-button v-if="item.role === 'assistant'" size="mini" :loading="speakingMessageID === item.id" @click="readAssistantMessage(item.id)">
                朗读
              </a-button>
            </div>
          </div>
        </template>
      </t-chatbot>

      <t-chat-sender
        v-model="composer"
        class="x-chat-sender"
        :loading="senderLoading"
        :actions="senderActions"
        :attachments-props="{ items: draftAttachmentItems, overflow: 'wrap' }"
        :textarea-props="senderTextareaProps"
        :placeholder="
          recoveringSession
            ? '正在恢复处理中，请稍候...'
            : '输入消息，支持 Markdown。可附加语音/图片/文件，也可发起投票。'
        "
        :send-btn-disabled="!canSend"
        @send="handleSenderSend"
        @stop="handleSenderStop"
        @file-select="handleChatbotFileSelect"
        @remove="handleChatbotFileRemove"
      >
        <template #inner-header>
          <div v-if="replyTarget || showEmojiPanel || pollEnabled" class="x-sender-stack">
            <div v-if="replyTarget" class="x-draft-reply">
              <div class="x-draft-reply-main">
                <strong>正在引用</strong>
                <t-chat-list
                  class="x-quote-list"
                  layout="both"
                  :auto-scroll="false"
                  :show-scroll-button="false"
                  :data="buildQuotePreviewMessages(replyTarget)"
                />
              </div>
              <div class="x-draft-reply-actions">
                <a-button size="mini" @click="cancelReply">取消</a-button>
              </div>
            </div>

            <div v-if="showEmojiPanel" class="x-emoji-panel">
              <button v-for="emoji in emojiList" :key="emoji" class="x-emoji-btn" type="button" @click="appendEmoji(emoji)">
                {{ emoji }}
              </button>
            </div>

            <div v-if="pollEnabled" class="x-poll-editor">
              <div class="form-group" style="margin-bottom: 10px">
                <label class="form-label">投票问题</label>
                <a-input v-model="pollQuestion" placeholder="例如：是否同意本周上线？" />
              </div>
              <div class="form-group" style="margin-bottom: 0">
                <label class="form-label">投票选项（每行一个）</label>
                <a-textarea v-model="pollOptionsText" :auto-size="{ minRows: 3, maxRows: 8 }" />
              </div>
            </div>
          </div>
        </template>

        <template #footer-prefix>
          <div class="x-composer-toolbar">
            <div class="x-composer-agent">
              {{ selectedAgent ? `${selectedAgent.emoji} ${selectedAgent.name}` : '未选择助手' }}
            </div>
            <a-space wrap>
              <a-button size="small" @click="showEmojiPanel = !showEmojiPanel">表情</a-button>
              <a-button size="small" @click="pollEnabled = !pollEnabled">{{ pollEnabled ? '取消投票' : '发起投票' }}</a-button>
              <a-button size="small" :type="isRecording ? 'primary' : 'outline'" @click="isRecording ? stopRecording() : startRecording()">
                {{ isRecording ? `停止录音 (${formatDuration(recordingSeconds)})` : '语音' }}
              </a-button>
            </a-space>
          </div>
        </template>
      </t-chat-sender>
    </div>
  </div>
</template>

<style scoped>
.chat-page {
  display: flex;
  flex-direction: column;
  gap: 14px;
}

.x-chat-shell {
  border-radius: 16px;
  border: 1px solid var(--line-0);
  background: linear-gradient(160deg, #f8fbff 0%, #f2f7fd 45%, #f7f8fc 100%);
  padding: 14px;
  display: flex;
  flex-direction: column;
  gap: 12px;
}

.x-chat-stage {
  display: inline-flex;
  align-items: center;
  gap: 8px;
  align-self: flex-start;
  padding: 6px 12px;
  border-radius: 999px;
  background: rgba(15, 23, 42, 0.72);
  color: #f8fafc;
  font-size: 12px;
  backdrop-filter: blur(12px);
}

.x-chat-stage-dot {
  width: 8px;
  height: 8px;
  border-radius: 999px;
  background: #22c55e;
  box-shadow: 0 0 0 6px rgba(34, 197, 94, 0.14);
}

.x-empty-chat {
  min-height: 160px;
  display: grid;
  place-items: center;
}

.x-message-avatar {
  width: 40px;
  height: 40px;
  border-radius: 14px;
  display: inline-flex;
  align-items: center;
  justify-content: center;
  font-size: 16px;
  font-weight: 700;
  color: #0f172a;
  background: linear-gradient(135deg, #dbeafe 0%, #bfdbfe 100%);
  box-shadow: inset 0 1px 0 rgba(255, 255, 255, 0.6);
}

.x-message-avatar.is-assistant {
  background: linear-gradient(135deg, #fde68a 0%, #f59e0b 100%);
}

.x-message-avatar.is-system {
  background: linear-gradient(135deg, #e2e8f0 0%, #cbd5e1 100%);
}

.x-message-content {
  display: flex;
  flex-direction: column;
  gap: 10px;
}

.x-message-actions {
  display: flex;
  align-items: flex-start;
  justify-content: space-between;
  gap: 12px;
  margin-top: 8px;
  flex-wrap: wrap;
}

.x-message-actionbar {
  flex: 1;
  min-width: 220px;
}

.x-message-action-extras {
  display: inline-flex;
  align-items: center;
  gap: 8px;
}

.x-thinking-card {
  border-radius: 14px;
}

.x-thinking-body {
  display: flex;
  flex-direction: column;
  gap: 6px;
}

.x-thinking-title {
  font-size: 12px;
  font-weight: 700;
  color: #0f172a;
}

.x-thinking-line {
  font-size: 12px;
  line-height: 1.5;
  color: #475569;
}

.x-inline-loading {
  padding-top: 2px;
}

.x-reply-block {
  max-width: min(100%, 420px);
}

.x-quote-list {
  border: 1px solid #dbeafe;
  border-radius: 12px;
  padding: 6px;
  background: #f8fbff;
}

.x-reply-label {
  font-size: 12px;
  color: #334155;
  font-weight: 600;
  margin-bottom: 4px;
}

.x-reply-text {
  font-size: 12px;
  color: #475569;
  margin-top: 3px;
  line-height: 1.45;
}

.x-attachment-list {
  border: 1px dashed #c7d2fe;
  border-radius: 12px;
  padding: 8px;
  background: #fafcff;
}

.x-attachment-title {
  font-size: 12px;
  font-weight: 600;
  color: #475569;
  margin-bottom: 8px;
}

.x-attachment-item {
  margin-bottom: 8px;
}

.x-attachment-item:last-child {
  margin-bottom: 0;
}

.x-audio-player {
  width: min(360px, 100%);
}

.x-attachment-meta {
  font-size: 12px;
  color: #64748b;
  margin-top: 4px;
}

.x-poll-card {
  border: 1px solid #fcd34d;
  background: #fffbeb;
  border-radius: 12px;
  padding: 10px;
}

.x-poll-title {
  margin-bottom: 8px;
  font-size: 13px;
  font-weight: 700;
  color: #92400e;
}

.x-sender-stack {
  display: flex;
  flex-direction: column;
  gap: 10px;
  margin-bottom: 10px;
}

.x-chat-sender {
  display: block;
}

.x-draft-reply {
  width: 100%;
}

.x-draft-reply-main {
  display: flex;
  flex-direction: column;
  gap: 3px;
  font-size: 12px;
}

.x-draft-reply-actions {
  margin-top: 8px;
}

.x-emoji-panel {
  display: flex;
  flex-wrap: wrap;
  gap: 8px;
  background: #f8fafc;
  border: 1px solid #e2e8f0;
  border-radius: 12px;
  padding: 8px;
}

.x-emoji-btn {
  border: 1px solid #e2e8f0;
  background: #fff;
  border-radius: 8px;
  font-size: 20px;
  width: 38px;
  height: 38px;
  cursor: pointer;
}

.x-emoji-btn:hover {
  background: #f1f5f9;
}

.x-poll-editor {
  border: 1px solid #fcd34d;
  background: #fffbeb;
  border-radius: 12px;
  padding: 10px;
}

.x-composer-toolbar {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 10px;
  width: 100%;
  flex-wrap: wrap;
}

.x-composer-agent {
  font-size: 12px;
  color: #475569;
  font-weight: 600;
}

:deep(.x-chatbot .t-chat-list) {
  height: 58vh;
  min-height: 420px;
  overflow-y: auto;
  padding-right: 6px;
}

:deep(.x-chatbot .t-chat-item__content) {
  max-width: min(100%, 860px);
}

:deep(.x-reply-block .t-chat__main),
:deep(.x-draft-reply .t-chat__main) {
  width: 100%;
}

:deep(.x-quote-list .t-chat-list) {
  height: auto;
  min-height: 0;
  overflow: visible;
  padding-right: 0;
}

:deep(.x-reply-block .t-chat__content),
:deep(.x-draft-reply .t-chat__content) {
  max-width: 100%;
}

:deep(.x-chat-sender .t-chat-sender) {
  border-radius: 14px;
  background: rgba(255, 255, 255, 0.84);
  box-shadow: inset 0 1px 0 rgba(255, 255, 255, 0.72);
}

:deep(.t-chat__input__footer__left) {
  flex: 1;
  min-width: 260px;
}

:deep(.t-chat__input__attachments) {
  margin-bottom: 10px;
}

@media (max-width: 900px) {
  :deep(.t-chat-list) {
    height: 52vh;
    min-height: 360px;
  }

  .x-composer-toolbar {
    align-items: flex-start;
    flex-direction: column;
  }
}
</style>
