<script setup lang="ts">
import { reactive, ref } from 'vue'
import { Message } from '@arco-design/web-vue'
import { useSystemStore } from '@/stores/system'

const store = useSystemStore()
const loading = ref(false)
const form = reactive({
  master_password: '',
  provider: 'openai',
  default_model: 'gpt-4o-mini',
  api_key: ''
})
const providerOptions = ['openai', 'anthropic', 'deepseek', 'local']
const modelExamples: Record<string, string[]> = {
  openai: ['gpt-4o-mini', 'gpt-4.1-mini', 'gpt-4.1'],
  anthropic: ['claude-3-5-sonnet-latest', 'claude-3-7-sonnet-latest'],
  deepseek: ['deepseek-chat', 'deepseek-reasoner'],
  local: ['local-reasoner']
}

function applyModelExample(model: string) {
  form.default_model = model
}

async function submit() {
  loading.value = true
  try {
    await store.bootstrapSystem({
      master_password: form.master_password.trim(),
      provider: form.provider.trim(),
      default_model: form.default_model.trim(),
      api_key: form.api_key.trim()
    })
    Message.success('初始化完成')
  } catch (error) {
    Message.error((error as Error).message)
  } finally {
    loading.value = false
  }
}
</script>

<template>
  <div class="page" style="max-width: 640px; margin: 0 auto">
    <div class="page-header" style="justify-content: center; text-align: center; border-bottom: none">
      <div>
        <div class="brand-icon" style="margin: 0 auto 16px; width: 56px; height: 56px; font-size: 28px">X</div>
        <h2 style="font-size: 22px; margin-bottom: 4px">首次运行向导</h2>
        <p>只需 3 步即可启动你的 XClaw 控制台</p>
      </div>
    </div>

    <a-alert type="warning" style="margin-bottom: 24px">
      只要填 3 项就能启动：管理员密码、模型供应商、默认模型。API Key 可选，后续也能再配置。
    </a-alert>

    <a-card :bordered="false" style="margin-bottom: 20px">
      <div class="form-group">
        <label class="form-label">1) 管理员密码</label>
        <a-input-password v-model="form.master_password" placeholder="至少 8 位，后续登录控制台会用到" allow-clear size="large" />
      </div>

      <div class="form-group">
        <label class="form-label">2) 模型供应商</label>
        <a-select v-model="form.provider" placeholder="选择默认供应商" size="large">
          <a-option v-for="item in providerOptions" :key="item" :value="item">{{ item }}</a-option>
        </a-select>
      </div>

      <div class="form-group">
        <label class="form-label">3) 默认模型</label>
        <a-space wrap style="margin-bottom: 8px">
          <a-button
            v-for="item in modelExamples[form.provider] || []"
            :key="item"
            size="small"
            @click="applyModelExample(item)"
          >
            {{ item }}
          </a-button>
        </a-space>
        <a-input v-model="form.default_model" placeholder="例如 gpt-4o-mini" allow-clear size="large" />
      </div>

      <div class="form-group">
        <label class="form-label">4) API Key（可选）</label>
        <a-input-password v-model="form.api_key" placeholder="可选：默认 API Key（不填可后续配置）" allow-clear size="large" />
      </div>

      <a-button type="primary" long :loading="loading" @click="submit" size="large">完成初始化</a-button>
    </a-card>

    <div style="text-align: center; color: var(--text-2); font-size: 13px">
      当前状态：
      <a-tag :color="store.status?.bootstrapped ? 'green' : 'orange'">
        {{ store.status?.bootstrapped ? '已初始化' : '未初始化' }}
      </a-tag>
    </div>
  </div>
</template>
