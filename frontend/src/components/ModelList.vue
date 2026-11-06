<template>
  <el-dialog
    v-model="visible"
    :title="t('models.title', { name: configName })"
    width="60%"
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
        <div class="models-hint">{{ t('models.clickHint') }}</div>
        <el-table
          :data="models"
          class="models-table"
          highlight-current-row
          @row-click="handleCopy"
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
          <el-table-column :label="t('models.table.contextWindow')" width="180">
            <template #default="scope">
              <span>{{ formatContextWindow(scope.row.context_window) }}</span>
            </template>
          </el-table-column>
          <el-table-column :label="t('models.table.actions')" width="110">
            <template #default="scope">
              <el-button size="small" type="primary" @click.stop="handleCopy(scope.row)">
                {{ t('models.copy') }}
              </el-button>
            </template>
          </el-table-column>
        </el-table>

        <el-empty
          v-if="!loading && models.length === 0"
          :description="t('models.empty')"
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
import { ref, watch } from 'vue'
import { getModelsForConfig } from '../api'
import { ElMessage } from 'element-plus'
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

// 上下文窗口缺失 (上游没返回该元数据) 时显示为空, 不猜测默认值
const formatContextWindow = (value) => {
  if (value === null || value === undefined || value === '') return '—'
  return Number(value).toLocaleString()
}

const fetchModels = async () => {
  if (!props.configId || props.configId <= 0) {
    models.value = []
    return
  }
  loading.value = true
  error.value = ''
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
.models-table :deep(.el-table__row) {
  cursor: pointer;
}
.dialog-footer {
  display: flex;
  justify-content: flex-end;
}
</style>
