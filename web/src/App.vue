<script setup>
import { computed, onBeforeUnmount, onMounted, reactive, ref, watch } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import {
  Calendar,
  CircleCheck,
  Connection,
  Delete,
  Download,
  Files,
  Plus,
  Promotion,
  RefreshRight,
  Search,
  User,
  FolderOpened,
  UserFilled,
  Setting,
  ArrowLeft,
  Document,
  ArrowDown,
  ArrowUp
} from '@element-plus/icons-vue'

const state = reactive({
  accounts: [],
  messages: [],
  endpoints: [],
  importText: '',
  importProtocol: 'auto',
  fetchLimit: 20,
  fetchAllMessages: true,
  selectedAccountIds: [],
  selectedMessageIds: [],
  loading: false,
  importing: false,
  fetching: false,
  fetchJobs: [],
  searchText: '',
  accountSearchText: '',
  accountsExpanded: false
})

const proxySettings = reactive({
  enabled: false,
  rawList: '',
  count: 0,
  updatedAt: ''
})

const showImportDialog = ref(false)
const showFetchDialog = ref(false)
const showEndpointsDialog = ref(false)
const showProxyDialog = ref(false)
const editingEndpoint = ref(null)
const newEndpoint = ref(null)
const currentMessage = ref(null)
const proxySaving = ref(false)

// 账号折叠显示数
const ACCOUNT_PREVIEW_LIMIT = 10

const totalAccountCount = ref(0)
const accountSearchLoading = ref(false)
let fetchJobPollTimer = null

const messageCount = computed(() => state.messages.length)
const filteredEndpoints = computed(() =>
  state.endpoints.filter((ep) => ep.requestedProtocol !== 'auto')
)
const endpointCount = computed(() => filteredEndpoints.value.length)
const selectedAccountCount = computed(() => state.selectedAccountIds.length)
const selectedMessageCount = computed(() => state.selectedMessageIds.length)
const loading = computed(() => state.loading)
const fetchJobs = computed(() => state.fetchJobs)
const fetchJobActive = computed(() => state.fetchJobs.length > 0)

function isAccountFetching(accountId) {
  return state.fetchJobs.some((job) =>
    job.currentAccountId === accountId ||
    job.targets?.some((target) => target.accountId === accountId)
  )
}

// 显示的账号列表（考虑折叠）
const displayedAccounts = computed(() => {
  if (state.accountsExpanded) {
    return state.accounts
  }
  return state.accounts.slice(0, ACCOUNT_PREVIEW_LIMIT)
})

const hasMoreAccounts = computed(() => state.accounts.length > ACCOUNT_PREVIEW_LIMIT)

const filteredMessages = computed(() => {
  const keyword = state.searchText.trim().toLowerCase()
  if (!keyword) return state.messages
  return state.messages.filter((message) =>
    [message.accountEmail, message.subject, message.from, message.preview, message.body]
      .some((item) => String(item || '').toLowerCase().includes(keyword))
  )
})

const isAllAccountsSelected = computed(() =>
  state.accounts.length > 0 && state.selectedAccountIds.length === state.accounts.length
)

const isAllMessagesSelected = computed(() =>
  filteredMessages.value.length > 0 && state.selectedMessageIds.length === filteredMessages.value.length
)

async function api(path, options = {}) {
  const response = await fetch(path, {
    headers: {
      'Content-Type': 'application/json',
      ...(options.headers || {})
    },
    ...options
  })
  if (!response.ok) {
    throw new Error((await response.text()) || `HTTP ${response.status}`)
  }
  const text = await response.text()
  return text ? JSON.parse(text) : {}
}

async function loadAccounts(keyword = '') {
  let url = '/api/accounts'
  if (keyword) {
    url += `?keyword=${encodeURIComponent(keyword)}`
  }
  const data = await api(url)
  state.accounts = data.accounts || []
  if (!keyword) {
    totalAccountCount.value = state.accounts.length
  }
  syncAccountSelection()
}

// 防抖搜索账号
let accountSearchTimeout = null
watch(() => state.accountSearchText, (val) => {
  clearTimeout(accountSearchTimeout)
  accountSearchTimeout = setTimeout(async () => {
    accountSearchLoading.value = true
    try {
      await loadAccounts(val.trim())
    } finally {
      accountSearchLoading.value = false
    }
  }, 300)
})

async function loadMessages() {
  const data = await api('/api/messages?limit=200')
  state.messages = data.messages || []
  syncMessageSelection()
}

async function loadEndpoints() {
  const data = await api('/api/endpoints')
  state.endpoints = data.endpoints || []
}

async function loadProxySettings() {
  const data = await api('/api/proxy-settings')
  const settings = data.settings || {}
  proxySettings.enabled = !!settings.enabled
  proxySettings.rawList = settings.rawList || ''
  proxySettings.count = settings.count || 0
  proxySettings.updatedAt = settings.updatedAt || ''
}

function stopFetchJobPolling() {
  if (fetchJobPollTimer) {
    clearTimeout(fetchJobPollTimer)
    fetchJobPollTimer = null
  }
}

function scheduleFetchJobPolling(delay = 1200, showFinishedMessage = false) {
  stopFetchJobPolling()
  fetchJobPollTimer = setTimeout(() => {
    pollFetchJob(showFinishedMessage).catch(() => {})
  }, delay)
}

async function loadFetchJob(jobId = '') {
  if (jobId) {
    const data = await api(`/api/fetch/${jobId}`)
    const job = data.job || null
    if (job) {
      const index = state.fetchJobs.findIndex((item) => item.id === job.id)
      if (index >= 0) {
        state.fetchJobs.splice(index, 1, job)
      } else {
        state.fetchJobs = [job, ...state.fetchJobs]
      }
    }
    return job
  }
  const data = await api('/api/fetch')
  state.fetchJobs = data.jobs || (data.job ? [data.job] : [])
  return state.fetchJobs
}

async function pollFetchJob(showFinishedMessage = false) {
  const jobs = state.fetchJobs.length ? [...state.fetchJobs] : await loadFetchJob()
  if (!jobs.length) {
    stopFetchJobPolling()
    return null
  }

  const refreshedJobs = []
  const finishedJobs = []
  for (const item of jobs) {
    const job = await loadFetchJob(item.id)
    if (!job) continue
    if (job.status === 'queued' || job.status === 'running') {
      refreshedJobs.push(job)
    } else {
      finishedJobs.push(job)
    }
  }
  state.fetchJobs = refreshedJobs

  if (refreshedJobs.length > 0) {
    scheduleFetchJobPolling(1200, showFinishedMessage)
    return refreshedJobs
  }

  stopFetchJobPolling()
  await refreshAll(false)
  if (showFinishedMessage && finishedJobs.length > 0) {
    for (const job of finishedJobs) {
      const success = job.successCount || 0
      const failed = job.failureCount || 0
      if (job.status === 'failed') {
        ElMessage.error(job.error || job.message || '收件任务失败')
      } else if (failed > 0) {
        const firstError = job.results?.find((item) => item.error)?.error
        ElMessage.warning(`${job.currentEmail || job.targets?.[0]?.email || job.id}: 成功 ${success}，失败 ${failed}${firstError ? `：${firstError}` : ''}`)
      } else {
        ElMessage.success(`${job.targets?.[0]?.email || job.id}: 收件完成，成功 ${success}`)
      }
    }
  }
  return finishedJobs
}

async function refreshAll(showMessage = false) {
  state.loading = true
  try {
    await Promise.all([loadAccounts(state.accountSearchText.trim()), loadMessages(), loadProxySettings()])
    if (showMessage) ElMessage.success('数据已刷新')
  } catch (error) {
    ElMessage.error(error.message || String(error))
  } finally {
    state.loading = false
  }
}

function toggleAccountsExpanded() {
  state.accountsExpanded = !state.accountsExpanded
}

function syncAccountSelection() {
  const ids = new Set(state.accounts.map((a) => a.id))
  state.selectedAccountIds = state.selectedAccountIds.filter((id) => ids.has(id))
}

function syncMessageSelection() {
  const ids = new Set(state.messages.map((m) => m.id))
  state.selectedMessageIds = state.selectedMessageIds.filter((id) => ids.has(id))
}

function toggleAccountSelection(accountId) {
  if (state.selectedAccountIds.includes(accountId)) {
    state.selectedAccountIds = state.selectedAccountIds.filter((id) => id !== accountId)
  } else {
    state.selectedAccountIds = [...state.selectedAccountIds, accountId]
  }
}

function toggleMessageSelection(messageId) {
  if (state.selectedMessageIds.includes(messageId)) {
    state.selectedMessageIds = state.selectedMessageIds.filter((id) => id !== messageId)
  } else {
    state.selectedMessageIds = [...state.selectedMessageIds, messageId]
  }
}

function toggleAllAccounts() {
  if (isAllAccountsSelected.value) {
    state.selectedAccountIds = []
  } else {
    state.selectedAccountIds = state.accounts.map((a) => a.id)
  }
}

function toggleAllMessages() {
  if (isAllMessagesSelected.value) {
    state.selectedMessageIds = []
  } else {
    state.selectedMessageIds = filteredMessages.value.map((m) => m.id)
  }
}

async function importAccounts() {
  if (!state.importText.trim()) {
    ElMessage.warning('请输入账号内容')
    return
  }
  state.importing = true
  try {
    const result = await api('/api/accounts/import', {
      method: 'POST',
      body: JSON.stringify({
        text: state.importText,
        protocol: state.importProtocol
      })
    })
    state.importText = ''
    showImportDialog.value = false
    ElMessage.success(`导入成功：新增 ${result.added || 0}，更新 ${result.updated || 0}`)
    await refreshAll()
  } catch (error) {
    ElMessage.error(error.message || String(error))
  } finally {
    state.importing = false
  }
}

async function removeAccount(accountId) {
  try {
    await api(`/api/accounts/${accountId}`, { method: 'DELETE' })
    ElMessage.success('账号已删除')
    await refreshAll()
  } catch (error) {
    ElMessage.error(error.message || String(error))
  }
}

async function deleteSelectedAccounts() {
  if (!state.selectedAccountIds.length) {
    ElMessage.warning('请先选择要删除的账号')
    return
  }
  try {
    await ElMessageBox.confirm(`确定删除 ${state.selectedAccountIds.length} 个账号？`, '确认删除')
    await Promise.all(state.selectedAccountIds.map((id) => api(`/api/accounts/${id}`, { method: 'DELETE' })))
    ElMessage.success(`已删除 ${state.selectedAccountIds.length} 个账号`)
    state.selectedAccountIds = []
    await refreshAll()
  } catch (e) {
    if (e !== 'cancel') ElMessage.error(e.message || String(e))
  }
}

async function deleteAllAccounts() {
  if (!state.accounts.length) {
    ElMessage.warning('没有账号可删除')
    return
  }
  try {
    await ElMessageBox.confirm(`确定删除全部 ${state.accounts.length} 个账号？`, '确认删除')
    await Promise.all(state.accounts.map((a) => api(`/api/accounts/${a.id}`, { method: 'DELETE' })))
    ElMessage.success('已删除全部账号')
    state.selectedAccountIds = []
    await refreshAll()
  } catch (e) {
    if (e !== 'cancel') ElMessage.error(e.message || String(e))
  }
}

async function fetchMail() {
  if (!state.selectedAccountIds.length) {
    ElMessage.warning('请先选择账号')
    return
  }
  try {
    const payload = {
      accountIds: state.selectedAccountIds,
      limit: Number(state.fetchLimit) || 20,
      allMessages: state.fetchAllMessages,
      markSeen: false,
      async: true
    }
    const result = await api('/api/fetch', {
      method: 'POST',
      body: JSON.stringify(payload)
    })
    if (result.job) {
      state.fetchJobs = [result.job, ...state.fetchJobs.filter((item) => item.id !== result.job.id)]
    }
    showFetchDialog.value = false
    if (result.started) {
      ElMessage.success(result.message || '后台收件已开始')
      scheduleFetchJobPolling(800, true)
    } else {
      ElMessage.warning(result.message || '部分账号已在收件中')
      if (state.fetchJobs.length > 0) {
        scheduleFetchJobPolling(800, true)
      }
    }
  } catch (error) {
    ElMessage.error(error.message || String(error))
  }
}

async function fetchOneAccount(accountId) {
  if (isAccountFetching(accountId)) {
    ElMessage.warning('该账号正在收件中')
    return
  }
  try {
    const result = await api('/api/fetch', {
      method: 'POST',
      body: JSON.stringify({
        accountIds: [accountId],
        limit: Number(state.fetchLimit) || 20,
        allMessages: state.fetchAllMessages,
        markSeen: false,
        async: true
      })
    })
    if (result.job) {
      state.fetchJobs = [result.job, ...state.fetchJobs.filter((item) => item.id !== result.job.id)]
    }
    if (result.started) {
      ElMessage.success(result.message || '后台收件已开始')
      scheduleFetchJobPolling(800, true)
    } else {
      ElMessage.warning(result.message || '该账号已在收件中')
      if (state.fetchJobs.length > 0) {
        scheduleFetchJobPolling(800, true)
      }
    }
  } catch (error) {
    ElMessage.error(error.message || String(error))
  }
}

function fetchJobStatusLabel(status) {
  switch (status) {
    case 'queued':
      return '排队中'
    case 'running':
      return '进行中'
    case 'completed':
      return '已完成'
    case 'failed':
      return '失败'
    default:
      return status || '未知'
  }
}

function fetchJobStatusType(status) {
  switch (status) {
    case 'queued':
      return 'warning'
    case 'running':
      return 'primary'
    case 'completed':
      return 'success'
    case 'failed':
      return 'danger'
    default:
      return 'info'
  }
}

async function viewMessage(message) {
  currentMessage.value = message
  try {
    const data = await api(`/api/messages/${message.id}`)
    if (currentMessage.value?.id === message.id && data.message) {
      currentMessage.value = { ...message, htmlBody: data.message.htmlBody || '' }
    }
  } catch (e) {
    // 详情取不到就退回列表里已有的文本正文
  }
}

function closeMessageDetail() {
  currentMessage.value = null
}

async function deleteCurrentMessage() {
  if (!currentMessage.value) return
  try {
    await ElMessageBox.confirm('确定删除这封邮件？', '确认删除')
    await api(`/api/messages/${currentMessage.value.id}`, { method: 'DELETE' })
    ElMessage.success('邮件已删除')
    currentMessage.value = null
    await loadMessages()
  } catch (e) {
    if (e !== 'cancel') ElMessage.error(e.message || String(e))
  }
}

async function deleteMessage(messageId) {
  try {
    await api(`/api/messages/${messageId}`, { method: 'DELETE' })
    ElMessage.success('邮件已删除')
    await loadMessages()
  } catch (error) {
    ElMessage.error(error.message || String(error))
  }
}

async function deleteSelectedMessages() {
  if (!state.selectedMessageIds.length) {
    ElMessage.warning('请先选择要删除的邮件')
    return
  }
  try {
    await ElMessageBox.confirm(`确定删除 ${state.selectedMessageIds.length} 封邮件？`, '确认删除')
    await Promise.all(state.selectedMessageIds.map((id) => api(`/api/messages/${id}`, { method: 'DELETE' })))
    ElMessage.success(`已删除 ${state.selectedMessageIds.length} 封邮件`)
    state.selectedMessageIds = []
    await loadMessages()
  } catch (e) {
    if (e !== 'cancel') ElMessage.error(e.message || String(e))
  }
}

async function deleteAllMessages() {
  if (!state.messages.length) {
    ElMessage.warning('没有邮件可删除')
    return
  }
  try {
    await ElMessageBox.confirm(`确定删除全部 ${state.messages.length} 封邮件？`, '确认删除')
    await Promise.all(state.messages.map((m) => api(`/api/messages/${m.id}`, { method: 'DELETE' })))
    ElMessage.success('已删除全部邮件')
    state.selectedMessageIds = []
    await loadMessages()
  } catch (e) {
    if (e !== 'cancel') ElMessage.error(e.message || String(e))
  }
}

async function openEndpointsDialog() {
  await loadEndpoints()
  showEndpointsDialog.value = true
}

async function openProxyDialog() {
  await loadProxySettings()
  showProxyDialog.value = true
}

async function saveProxySettings() {
  proxySaving.value = true
  try {
    const data = await api('/api/proxy-settings', {
      method: 'PUT',
      body: JSON.stringify({
        enabled: proxySettings.enabled,
        rawList: proxySettings.rawList
      })
    })
    const settings = data.settings || {}
    proxySettings.enabled = !!settings.enabled
    proxySettings.rawList = settings.rawList || ''
    proxySettings.count = settings.count || 0
    proxySettings.updatedAt = settings.updatedAt || ''
    ElMessage.success(`代理设置已保存，当前 ${proxySettings.count} 条代理${proxySettings.enabled ? '，已启用' : '，已关闭'}`)
    showProxyDialog.value = false
  } catch (error) {
    ElMessage.error(error.message || String(error))
  } finally {
    proxySaving.value = false
  }
}

function startEditEndpoint(endpoint) {
  editingEndpoint.value = { ...endpoint }
  newEndpoint.value = null
}

function cancelEditEndpoint() {
  editingEndpoint.value = null
}

function startNewEndpoint() {
  newEndpoint.value = {
    domain: '',
    requestedProtocol: 'imap',
    actualProtocol: 'imap',
    host: '',
    port: 993,
    mode: 'tls'
  }
  editingEndpoint.value = null
}

function cancelNewEndpoint() {
  newEndpoint.value = null
}

async function createEndpoint() {
  if (!newEndpoint.value) return
  const ep = newEndpoint.value
  if (!ep.domain || !ep.host || !ep.port) {
    ElMessage.warning('请填写完整信息')
    return
  }
  try {
    await api('/api/endpoints', {
      method: 'POST',
      body: JSON.stringify({
        domain: ep.domain,
        requestedProtocol: ep.requestedProtocol,
        actualProtocol: ep.actualProtocol,
        host: ep.host,
        port: ep.port,
        mode: ep.mode
      })
    })
    ElMessage.success('已添加')
    newEndpoint.value = null
    await loadEndpoints()
  } catch (error) {
    ElMessage.error(error.message || String(error))
  }
}

async function saveEndpoint() {
  if (!editingEndpoint.value) return
  try {
    await api(`/api/endpoints/${editingEndpoint.value.domain}/${editingEndpoint.value.requestedProtocol}`, {
      method: 'PUT',
      body: JSON.stringify({
        actualProtocol: editingEndpoint.value.actualProtocol,
        host: editingEndpoint.value.host,
        port: editingEndpoint.value.port,
        mode: editingEndpoint.value.mode
      })
    })
    ElMessage.success('已保存')
    editingEndpoint.value = null
    await loadEndpoints()
  } catch (error) {
    ElMessage.error(error.message || String(error))
  }
}

async function deleteEndpoint(endpoint) {
  try {
    await ElMessageBox.confirm(`确定删除 ${endpoint.domain} 的缓存？`, '确认删除')
    await api(`/api/endpoints/${endpoint.domain}/${endpoint.requestedProtocol}`, { method: 'DELETE' })
    ElMessage.success('已删除')
    await loadEndpoints()
  } catch (e) {
    if (e !== 'cancel') ElMessage.error(e.message || String(e))
  }
}

function protocolLabel(value) {
  if (value === 'imap') return 'IMAP'
  if (value === 'pop3') return 'POP3'
  return 'AUTO'
}

function protocolType(value) {
  if (value === 'imap') return 'primary'
  if (value === 'pop3') return 'warning'
  return 'info'
}

function formatTime(value) {
  if (!value) return '-'
  const date = new Date(value)
  return Number.isNaN(date.getTime()) ? value : date.toLocaleString()
}

onMounted(async () => {
  await refreshAll()
  try {
    const jobs = await loadFetchJob()
    if (jobs && state.fetchJobs.length > 0) {
      scheduleFetchJobPolling()
    }
  } catch {
  }
})

onBeforeUnmount(() => {
  stopFetchJobPolling()
})
</script>

<template>
  <div class="app-shell">
    <!-- 邮件详情页 -->
    <template v-if="currentMessage">
      <header class="header">
        <div class="header-left">
          <el-button :icon="ArrowLeft" @click="closeMessageDetail">返回列表</el-button>
          <span class="header-title">邮件详情</span>
        </div>
        <div class="header-actions">
          <el-button type="danger" :icon="Delete" @click="deleteCurrentMessage">删除邮件</el-button>
        </div>
      </header>

      <main class="main detail-main">
        <div class="detail-card">
          <div class="detail-header">
            <h1 class="detail-subject">{{ currentMessage.subject || '(无主题)' }}</h1>
            <div class="detail-tags">
              <el-tag :type="protocolType(currentMessage.protocol)">{{ protocolLabel(currentMessage.protocol) }}</el-tag>
            </div>
          </div>

          <div class="detail-meta">
            <div class="meta-row">
              <span class="meta-label">发件人</span>
              <span class="meta-value">{{ currentMessage.from || '-' }}</span>
            </div>
            <div class="meta-row">
              <span class="meta-label">收件账号</span>
              <span class="meta-value">{{ currentMessage.accountEmail }}</span>
            </div>
            <div class="meta-row">
              <span class="meta-label">发送时间</span>
              <span class="meta-value">{{ currentMessage.date || '-' }}</span>
            </div>
            <div class="meta-row">
              <span class="meta-label">收取时间</span>
              <span class="meta-value">{{ formatTime(currentMessage.fetchedAt) }}</span>
            </div>
          </div>

          <div class="detail-body">
            <h3><el-icon><Document /></el-icon> 邮件正文</h3>
            <iframe v-if="currentMessage.htmlBody" class="body-frame" :srcdoc="currentMessage.htmlBody" sandbox="allow-popups allow-popups-to-escape-sandbox"></iframe>
            <div v-else class="body-content">{{ currentMessage.body || currentMessage.preview || '(无内容)' }}</div>
          </div>

          <div v-if="currentMessage.attachments?.length" class="detail-attachments">
            <h3><el-icon><Files /></el-icon> 附件 ({{ currentMessage.attachments.length }})</h3>
            <div class="attachment-list">
              <div v-for="att in currentMessage.attachments" :key="att" class="attachment-item">
                <el-icon><Files /></el-icon>
                <span>{{ att.split('/').pop() }}</span>
              </div>
            </div>
          </div>
        </div>
      </main>
    </template>

    <!-- 主页面 -->
    <template v-else>
      <header class="header">
        <div class="header-left">
          <h1 class="logo">Email Get</h1>
          <span class="stats">
            <el-icon><UserFilled /></el-icon>
            {{ totalAccountCount }} 账号
            <el-divider direction="vertical" />
            <el-icon><FolderOpened /></el-icon>
            {{ messageCount }} 邮件
          </span>
        </div>
        <div class="header-actions">
          <el-button type="primary" :icon="Plus" @click="showImportDialog = true">添加账号</el-button>
          <el-button :icon="Promotion" @click="openProxyDialog">
            代理设置
            <el-badge v-if="proxySettings.count" :value="proxySettings.count" :max="99" class="endpoint-badge" />
          </el-button>
          <el-button :icon="Setting" @click="openEndpointsDialog">
            缓存配置
            <el-badge v-if="endpointCount" :value="endpointCount" :max="99" class="endpoint-badge" />
          </el-button>
          <el-button :icon="RefreshRight" :loading="loading" @click="refreshAll(true)">刷新</el-button>
        </div>
      </header>

      <main class="main">
        <section v-if="fetchJobs.length" class="card fetch-job-card" :class="{ active: fetchJobActive }">
          <div class="fetch-job-content">
            <div class="fetch-job-heading">
              <div>
                <h2>收件任务</h2>
                <p>当前共有 {{ fetchJobs.length }} 个后台收件任务在执行</p>
              </div>
            </div>
            <div class="fetch-job-list">
              <div v-for="job in fetchJobs" :key="job.id" class="fetch-job-item">
                <div class="fetch-job-item-head">
                  <div>
                    <strong>{{ job.currentEmail || job.targets?.[0]?.email || job.id }}</strong>
                    <p>{{ job.message || '暂无任务信息' }}</p>
                  </div>
                  <el-tag :type="fetchJobStatusType(job.status)">{{ fetchJobStatusLabel(job.status) }}</el-tag>
                </div>
                <div class="fetch-job-meta">
                  <span>进度 {{ job.completedAccounts || 0 }}/{{ job.totalAccounts || 0 }}</span>
                  <span>成功 {{ job.successCount || 0 }}</span>
                  <span>失败 {{ job.failureCount || 0 }}</span>
                  <span v-if="job.currentEmail">当前 {{ job.currentEmail }}</span>
                </div>
              </div>
            </div>
          </div>
        </section>

        <!-- 账号面板 -->
        <section class="card accounts-section">
          <div class="section-header">
            <div class="section-title">
              <h2>账号管理</h2>
              <span class="count" v-if="selectedAccountCount">已选 {{ selectedAccountCount }} 个</span>
            </div>
            <div class="section-actions">
              <el-input
                v-model="state.accountSearchText"
                placeholder="搜索账号..."
                :prefix-icon="Search"
                clearable
                :loading="accountSearchLoading"
                style="width: 180px"
                @click.stop
              />
              <el-checkbox
                :model-value="isAllAccountsSelected"
                :indeterminate="selectedAccountCount > 0 && selectedAccountCount < state.accounts.length"
                @change="toggleAllAccounts"
              >全选</el-checkbox>
              <el-button
                type="primary"
                :icon="Download"
                :disabled="!selectedAccountCount"
                @click="showFetchDialog = true"
              >批量收件</el-button>
              <el-button
                text type="danger"
                :disabled="!selectedAccountCount"
                @click="deleteSelectedAccounts"
              >删除选中</el-button>
              <el-button
                text type="danger"
                :disabled="!state.accounts.length"
                @click="deleteAllAccounts"
              >清空全部</el-button>
            </div>
          </div>

          <el-empty v-if="!state.accounts.length" description="暂无账号，点击「添加账号」导入">
            <el-button type="primary" @click="showImportDialog = true">添加账号</el-button>
          </el-empty>

          <template v-else>
            <div class="account-grid">
              <div
                v-for="account in displayedAccounts"
                :key="account.id"
                class="account-item"
                :class="{ selected: state.selectedAccountIds.includes(account.id) }"
                @click="toggleAccountSelection(account.id)"
              >
                <div class="account-check">
                  <el-checkbox
                    :model-value="state.selectedAccountIds.includes(account.id)"
                    @click.stop
                    @change="toggleAccountSelection(account.id)"
                  />
                </div>
                <div class="account-info">
                  <div class="account-email">{{ account.email }}</div>
                  <div class="account-meta">
                    <el-tag size="small" :type="protocolType(account.protocol)">{{ protocolLabel(account.protocol) }}</el-tag>
                    <el-tag v-if="account.hasOAuth" size="small" type="success" effect="plain">OAuth</el-tag>
                    <span class="meta-text" v-if="account.server">{{ account.server }}:{{ account.port }}</span>
                    <span class="meta-text" v-if="account.lastFetchedAt">最近: {{ formatTime(account.lastFetchedAt) }}</span>
                  </div>
                </div>
                <div class="account-actions" @click.stop>
                  <el-button text type="primary" size="small" :icon="Download" :loading="isAccountFetching(account.id)" @click="fetchOneAccount(account.id)">收件</el-button>
                  <el-button text type="danger" size="small" :icon="Delete" @click="removeAccount(account.id)">删除</el-button>
                </div>
                <el-alert v-if="account.lastError" type="error" :closable="false" show-icon class="account-error">
                  {{ account.lastError }}
                </el-alert>
              </div>
            </div>

            <!-- 展开/折叠按钮 -->
            <div v-if="hasMoreAccounts" class="expand-toggle">
              <el-button text @click="toggleAccountsExpanded">
                <el-icon><component :is="state.accountsExpanded ? ArrowUp : ArrowDown" /></el-icon>
                {{ state.accountsExpanded ? '收起' : `展开全部 (${state.accounts.length} 个账号)` }}
              </el-button>
            </div>
          </template>
        </section>

        <!-- 邮件面板 -->
        <section class="card messages-section">
          <div class="section-header">
            <div class="section-title">
              <h2>邮件列表</h2>
              <span class="count" v-if="selectedMessageCount">已选 {{ selectedMessageCount }} 封</span>
            </div>
            <div class="section-actions">
              <el-input
                v-model="state.searchText"
                placeholder="搜索邮件..."
                :prefix-icon="Search"
                clearable
                style="width: 200px"
              />
              <el-checkbox
                :model-value="isAllMessagesSelected"
                :indeterminate="selectedMessageCount > 0 && selectedMessageCount < filteredMessages.length"
                @change="toggleAllMessages"
              >全选</el-checkbox>
              <el-button
                text type="danger"
                :disabled="!selectedMessageCount"
                @click="deleteSelectedMessages"
              >删除选中</el-button>
              <el-button
                text type="danger"
                :disabled="!messageCount"
                @click="deleteAllMessages"
              >清空全部</el-button>
            </div>
          </div>

          <el-empty v-if="!filteredMessages.length" description="暂无邮件" :image-size="64" />

          <div v-else class="message-list">
            <div
              v-for="message in filteredMessages"
              :key="message.id"
              class="message-item"
              :class="{ selected: state.selectedMessageIds.includes(message.id) }"
            >
              <div class="message-check" @click.stop>
                <el-checkbox
                  :model-value="state.selectedMessageIds.includes(message.id)"
                  @change="toggleMessageSelection(message.id)"
                />
              </div>
              <div class="message-content" @click="viewMessage(message)">
                <div class="message-top">
                  <span class="message-account">{{ message.accountEmail }}</span>
                  <el-tag size="small" :type="protocolType(message.protocol)">{{ protocolLabel(message.protocol) }}</el-tag>
                </div>
                <h3 class="message-subject">{{ message.subject || '(无主题)' }}</h3>
                <div class="message-meta">
                  <span><el-icon><User /></el-icon>{{ message.from || '-' }}</span>
                  <span><el-icon><Calendar /></el-icon>{{ message.date || '-' }}</span>
                </div>
                <p class="message-body">{{ message.preview || message.body || '-' }}</p>
                <div v-if="message.attachments?.length" class="message-attachments">
                  <span v-for="att in message.attachments" :key="att" class="attachment-tag">
                    <el-icon><Files /></el-icon>
                    {{ att.split('/').pop() }}
                  </span>
                </div>
              </div>
              <div class="message-actions" @click.stop>
                <el-button text type="primary" size="small" @click="viewMessage(message)">查看</el-button>
                <el-button text type="danger" size="small" :icon="Delete" @click="deleteMessage(message.id)">删除</el-button>
              </div>
            </div>
          </div>
        </section>
      </main>
    </template>

    <!-- 导入账号弹窗 -->
    <el-dialog v-model="showImportDialog" title="添加账号" width="500px" :close-on-click-modal="false">
      <el-form label-position="top">
        <el-form-item label="协议类型">
          <el-radio-group v-model="state.importProtocol">
            <el-radio value="auto">自动探测</el-radio>
            <el-radio value="imap">IMAP</el-radio>
            <el-radio value="pop3">POP3</el-radio>
          </el-radio-group>
        </el-form-item>
        <el-form-item label="账号内容">
          <el-input
            v-model="state.importText"
            type="textarea"
            :rows="8"
            placeholder="一行一个，格式：
邮箱----密码
邮箱----密码----client_id----refresh_token"
          />
        </el-form-item>
      </el-form>
      <template #footer>
        <el-button @click="showImportDialog = false">取消</el-button>
        <el-button type="primary" :loading="state.importing" @click="importAccounts">导入</el-button>
      </template>
    </el-dialog>

    <!-- 收件设置弹窗 -->
    <el-dialog v-model="showFetchDialog" title="收件设置" width="400px" :close-on-click-modal="false">
      <el-form label-position="top">
        <el-form-item label="抓取数量">
          <el-input-number v-model="state.fetchLimit" :min="1" :max="200" style="width: 100%" />
        </el-form-item>
        <el-form-item>
          <el-checkbox v-model="state.fetchAllMessages">抓取最近邮件（不只未读）</el-checkbox>
        </el-form-item>
        <el-form-item v-if="selectedAccountCount">
          <span class="fetch-info">将为 {{ selectedAccountCount }} 个选中账号收件</span>
        </el-form-item>
        <el-form-item v-else>
          <el-alert type="warning" :closable="false">请先在账号列表中选择要收件的账号</el-alert>
        </el-form-item>
      </el-form>
      <template #footer>
        <el-button @click="showFetchDialog = false">取消</el-button>
        <el-button type="primary" :loading="false" :disabled="!selectedAccountCount" @click="fetchMail">开始收件</el-button>
      </template>
    </el-dialog>

    <!-- Endpoint 缓存弹窗 -->
    <el-dialog v-model="showEndpointsDialog" title="服务端点缓存" width="700px" :close-on-click-modal="false">
      <div class="endpoint-header">
        <el-button type="primary" size="small" :icon="Plus" @click="startNewEndpoint">新增缓存</el-button>
      </div>

      <div v-if="newEndpoint" class="endpoint-item endpoint-new">
        <div class="endpoint-edit-form">
          <div class="endpoint-edit-row">
            <el-input v-model="newEndpoint.domain" size="small" placeholder="域名 (如 gmail.com)" style="width: 160px" />
            <el-select v-model="newEndpoint.requestedProtocol" size="small" style="width: 100px">
              <el-option value="imap" label="IMAP" />
              <el-option value="pop3" label="POP3" />
            </el-select>
          </div>
          <div class="endpoint-edit-fields">
            <el-select v-model="newEndpoint.actualProtocol" size="small" style="width: 100px">
              <el-option value="imap" label="IMAP" />
              <el-option value="pop3" label="POP3" />
            </el-select>
            <el-input v-model="newEndpoint.host" size="small" placeholder="服务器地址" style="width: 180px" />
            <el-input-number v-model="newEndpoint.port" size="small" :min="1" :max="65535" style="width: 110px" />
            <el-select v-model="newEndpoint.mode" size="small" style="width: 90px">
              <el-option value="tls" label="TLS" />
              <el-option value="starttls" label="STARTTLS" />
              <el-option value="plain" label="Plain" />
            </el-select>
          </div>
          <div class="endpoint-edit-actions">
            <el-button size="small" @click="cancelNewEndpoint">取消</el-button>
            <el-button type="primary" size="small" @click="createEndpoint">添加</el-button>
          </div>
        </div>
      </div>

      <el-empty v-if="!filteredEndpoints.length" description="暂无缓存数据" :image-size="64" />
      <div v-else class="endpoint-list">
        <div v-for="ep in filteredEndpoints" :key="ep.domain + ep.requestedProtocol" class="endpoint-item">
          <template v-if="editingEndpoint?.domain === ep.domain && editingEndpoint?.requestedProtocol === ep.requestedProtocol">
            <div class="endpoint-edit-form">
              <div class="endpoint-edit-row">
                <span class="endpoint-domain">{{ ep.domain }}</span>
                <el-tag size="small" :type="protocolType(ep.requestedProtocol)">{{ protocolLabel(ep.requestedProtocol) }}</el-tag>
              </div>
              <div class="endpoint-edit-fields">
                <el-select v-model="editingEndpoint.actualProtocol" size="small" style="width: 100px">
                  <el-option value="imap" label="IMAP" />
                  <el-option value="pop3" label="POP3" />
                </el-select>
                <el-input v-model="editingEndpoint.host" size="small" placeholder="服务器地址" style="width: 180px" />
                <el-input-number v-model="editingEndpoint.port" size="small" :min="1" :max="65535" style="width: 110px" />
                <el-select v-model="editingEndpoint.mode" size="small" style="width: 90px">
                  <el-option value="tls" label="TLS" />
                  <el-option value="starttls" label="STARTTLS" />
                  <el-option value="plain" label="Plain" />
                </el-select>
              </div>
              <div class="endpoint-edit-actions">
                <el-button size="small" @click="cancelEditEndpoint">取消</el-button>
                <el-button type="primary" size="small" @click="saveEndpoint">保存</el-button>
              </div>
            </div>
          </template>
          <template v-else>
            <div class="endpoint-info">
              <div class="endpoint-main">
                <span class="endpoint-domain">{{ ep.domain }}</span>
                <el-tag size="small" :type="protocolType(ep.requestedProtocol)">{{ protocolLabel(ep.requestedProtocol) }}</el-tag>
              </div>
              <div class="endpoint-detail">
                <el-tag size="small" type="success">{{ protocolLabel(ep.actualProtocol) }}</el-tag>
                <span class="endpoint-host">{{ ep.host }}:{{ ep.port }}</span>
                <el-tag size="small" type="info" effect="plain">{{ ep.mode }}</el-tag>
                <span class="endpoint-time">{{ formatTime(ep.updatedAt) }}</span>
              </div>
            </div>
            <div class="endpoint-actions">
              <el-button text size="small" @click="startEditEndpoint(ep)">编辑</el-button>
              <el-button text type="danger" size="small" @click="deleteEndpoint(ep)">删除</el-button>
            </div>
          </template>
        </div>
      </div>
    </el-dialog>

    <el-dialog v-model="showProxyDialog" title="代理设置" width="680px" :close-on-click-modal="false">
      <el-form label-position="top">
        <el-form-item>
          <div class="proxy-toggle-row">
            <div>
              <div class="proxy-toggle-title">启用代理</div>
              <div class="proxy-toggle-desc">探测、收件、OAuth token 请求都会按列表轮询走代理</div>
            </div>
            <el-switch v-model="proxySettings.enabled" />
          </div>
        </el-form-item>
        <el-form-item label="代理列表">
          <el-input
            v-model="proxySettings.rawList"
            type="textarea"
            :rows="10"
            placeholder="一行一个，格式：&#10;user:pass@proxy.example.com:8080&#10;http://user:pass@proxy.example.com:8080"
          />
        </el-form-item>
        <el-form-item>
          <div class="proxy-meta">
            <span>当前解析代理数：{{ proxySettings.count }}</span>
            <span v-if="proxySettings.updatedAt">最近更新：{{ formatTime(proxySettings.updatedAt) }}</span>
          </div>
        </el-form-item>
      </el-form>
      <template #footer>
        <el-button @click="showProxyDialog = false">取消</el-button>
        <el-button type="primary" :loading="proxySaving" @click="saveProxySettings">保存</el-button>
      </template>
    </el-dialog>
  </div>
</template>
