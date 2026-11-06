<template>
  <el-dialog
    v-model="visible"
    :title="t('models.title', { name: configName })"
    :width="dialogWidth"
    :fullscreen="isMobile"
    class="responsive-dialog"
    @close="handleClose"
  >
    <div v-loading="loading">
      <el-alert
        v-if="error"
        :title="error"
        type="error"
        :closable="false"
        show-icon
        class="models-error"
      />

      <template v-else>
        <el-input
          v-if="models.length > 0"
          v-model="searchQuery"
          :placeholder="t('models.searchPlaceholder')"
          :prefix-icon="Search"
          clearable
          class="models-search"
        />
        <div class="models-hint">{{ t('models.clickHint') }}</div>

        <div v-if="isMobile" class="model-cards">
          <div v-for="model in filteredModels" :key="model.id" class="model-card">
            <div class="model-card-main">
              <div class="model-id">{{ model.id }}</div>
              <div v-if="model.name && model.name !== model.id" class="model-name">
                {{ model.name }}
              </div>
              <div class="model-context">
                <span class="model-context-label">{{ t('models.table.contextWindow') }}:</span>
                {{ formatContextWindow(model.context_window) }}
              </div>
            </div>
            <el-button size="small" type="primary" @click="handleCopy(model)">
              {{ t('models.copy') }}
            </el-button>
          </div>
        </div>

        <el-table
          v-else-if="filteredModels.length > 0"
          :data="filteredModels"
          class="models-table"
        >
          <el-table-column :label="t('models.table.model')">
            <template #default="scope">
              <div class="model-cell">
                <span class="model-id">{{ scope.row.id }}</span>
                <span v-if="scope.row.name && scope.row.name !== scope.row.id" class="model-name">
                  {{ scope.row.name }}
                </span>
              </div>
            </template>
          </el-table-column>
          <el-table-column :label="t('models.table.contextWindow')" :width="contextColumnWidth">
            <template #default="scope">
              <span>{{ formatContextWindow(scope.row.context_window) }}</span>
            </template>
          </el-table-column>
          <el-table-column :label="t('models.table.actions')" :width="actionsColumnWidth">
            <template #default="scope">
              <el-button size="small" type="primary" @click="handleCopy(scope.row)">
                {{ t('models.copy') }}
              </el-button>
            </template>
          </el-table-column>
        </el-table>

        <el-empty
          v-if="!loading && filteredModels.length === 0"
          :description="models.length === 0 ? t('models.empty') : t('models.noMatches')"
        />
      </template>
    </div>

    <template #footer>
      <span class="dialog-footer">
        <el-button @click="visible = false">{{ t('models.close') }}</el-button>
      </span>
    </template>
  </el-dialog>
</template>

<script setup>
import { computed, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { getModelsForConfig } from '../api'
import { ElMessage } from 'element-plus'
import { Search } from '@element-plus/icons-vue'
import { useI18n } from 'vue-i18n'

const { t } = useI18n()

const props = defineProps({
  modelValue: Boolean, // 控制对话框显示
  configId: Number,
  configName: String
})

const emit = defineEmits(['update:modelValue'])

const visible = ref(props.modelValue)
const models = ref([])
const loading = ref(false)
const error = ref('')
const searchQuery = ref('')

const viewportWidth = ref(typeof window !== 'undefined' ? window.innerWidth : 1280)
const updateViewport = () => {
  viewportWidth.value = window.innerWidth
}
onMounted(() => window.addEventListener('resize', updateViewport))
onBeforeUnmount(() => window.removeEventListener('resize', updateViewport))

const isMobile = computed(() => viewportWidth.value <= 768)
const isTablet = computed(() => viewportWidth.value > 768 && viewportWidth.value <= 1024)
const dialogWidth = computed(() => {
  if (isMobile.value) return '96%'
  if (isTablet.value) return '92%'
  return '60%'
})
// The table is hidden on phones; these widths only affect tablet/desktop.
const contextColumnWidth = computed(() => isTablet.value ? 130 : 180)
const actionsColumnWidth = computed(() => isTablet.value ? 88 : 110)

// 上下文窗口缺失 (上游没返回该元数据) 时显示为空, 不猜测默认值
const formatContextWindow = (value) => {
  if (value === null || value === undefined || value === '') return '—'
  return Number(value).toLocaleString()
}

// 输入时即时过滤, 同时匹配模型 ID 和展示名称。
const filteredModels = computed(() => {
  const query = searchQuery.value.trim().toLowerCase()
  if (!query) return models.value
  return models.value.filter((model) => {
    return (model.id || '').toLowerCase().includes(query) ||
      (model.name || '').toLowerCase().includes(query)
  })
})

const fetchModels = async () => {
  if (!props.configId || props.configId <= 0) {
    models.value = []
    return
  }
  loading.value = true
  error.value = ''
  searchQuery.value = ''
  try {
    const response = await getModelsForConfig(props.configId)
    models.value = Array.isArray(response.data) ? response.data : []
  } catch (err) {
    error.value = t('models.loadFailed')
    models.value = []
    console.error(err)
  } finally {
    loading.value = false
  }
}

const handleCopy = (row) => {
  const text = row && (row.id || row.name)
  if (!text) return
  copyText(text)
}

const copyText = (text) => {
  const onSuccess = () => {
    ElMessage.success({ message: t('models.copySuccess', { model: text }), duration: 2000 })
  }
  const onFailure = () => {
    ElMessage.error(t('models.copyFailed'))
  }

  if (navigator.clipboard && window.isSecureContext) {
    navigator.clipboard.writeText(text).then(onSuccess).catch((err) => {
      console.error('Could not copy text: ', err)
      onFailure()
    })
    return
  }

  // 降级方案: 非安全上下文下用传统的 execCommand('copy')
  let textArea
  try {
    textArea = document.createElement('textarea')
    textArea.value = text
    textArea.style.top = '0'
    textArea.style.left = '0'
    textArea.style.position = 'fixed'
    textArea.style.opacity = '0'
    document.body.appendChild(textArea)
    textArea.focus()
    textArea.select()
    const successful = document.execCommand('copy')
    successful ? onSuccess() : onFailure()
  } catch (err) {
    console.error('Could not copy text: ', err)
    onFailure()
  } finally {
    if (textArea && textArea.parentNode) {
      textArea.parentNode.removeChild(textArea)
    }
  }
}

// 监听 v-model 的变化来控制对话框
watch(() => props.modelValue, (newValue) => {
  visible.value = newValue
})

watch(() => props.configId, (newId) => {
  if (newId && props.modelValue) {
    fetchModels()
  }
}, { immediate: true })

const handleClose = () => {
  emit('update:modelValue', false)
}
</script>

<style scoped>
.models-search {
  margin-bottom: 12px;
}
.models-hint {
  color: #909399;
  font-size: 13px;
  margin-bottom: 12px;
}
.models-error {
  margin-bottom: 12px;
}
.model-cell {
  display: flex;
  flex-direction: column;
}
.model-id {
  font-family: monospace;
  word-break: break-all;
}
.model-name {
  color: #909399;
  font-size: 12px;
  word-break: break-all;
}
.model-cards {
  display: flex;
  flex-direction: column;
  gap: 10px;
}
.model-card {
  display: flex;
  align-items: center;
  gap: 12px;
  padding: 12px;
  border: 1px solid var(--el-border-color-lighter);
  border-radius: 8px;
}
.model-card-main {
  flex: 1;
  min-width: 0;
}
.model-card .el-button {
  flex: 0 0 auto;
}
.model-context {
  color: #909399;
  font-size: 12px;
  margin-top: 4px;
}
.model-context-label {
  color: #606266;
}
.models-table,
.models-table :deep(.el-table__row),
.models-table :deep(.el-table__cell),
.models-table :deep(.el-button) {
  -webkit-tap-highlight-color: transparent;
}
.models-table :deep(.el-table__row) {
  cursor: default;
}
.models-table :deep(.el-table__body tr:hover > td.el-table__cell) {
  background-color: transparent;
}
.dialog-footer {
  display: flex;
  justify-content: flex-end;
}
@media (max-width: 1024px) {
  .models-table :deep(.el-table .cell) {
    padding: 6px 8px;
    font-size: 13px;
  }
}
@media (max-width: 768px) {
  .models-search {
    margin-bottom: 10px;
  }
  .models-hint {
    font-size: 12px;
    margin-bottom: 10px;
  }
  .model-card {
    padding: 10px;
  }
}
</style>