<template>
  <el-container class="layout-container">
    <el-header class="header">
      <div class="header-title">{{ $t('layout.title') }}</div>
      <div class="header-right">
        <LangSwitcher />
        <el-button class="logout-btn" type="danger" @click="handleLogout" style="margin-left: 20px;">{{ $t('layout.logout') }}</el-button>
      </div>
    </el-header>
    <el-main>
      <router-view />
    </el-main>
  </el-container>
</template>

<script setup>
import { useRouter } from 'vue-router'
import LangSwitcher from '../components/LangSwitcher.vue'

const router = useRouter()

const handleLogout = () => {
  localStorage.removeItem('authToken')
  router.push({ name: 'Login' })
}
</script>

<style scoped>
.layout-container {
  height: 100vh;
}
.header {
  display: flex;
  justify-content: space-between;
  align-items: center;
}
.header-right {
  display: flex;
  align-items: center;
}
.header {
  background-color: #303133;
  color: #fff;
  font-size: 20px;
  height: auto !important;
  min-height: 60px;
  padding: 10px 16px;
  flex-wrap: wrap;
  row-gap: 8px;
}
.el-main {
  padding: 16px;
}
@media (max-width: 768px) {
  .header {
    font-size: 16px;
    padding: 10px 12px;
  }
  .header-title {
    flex: 1 1 auto;
    min-width: 0;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .header-right {
    flex-wrap: wrap;
    row-gap: 8px;
  }
  .logout-btn {
    margin-left: 8px !important;
  }
  .el-main {
    padding: 12px;
  }
}
.header-right .el-button.el-button--danger:hover,
.header-right .el-button.el-button--danger:focus {
    background-color: var(--el-color-danger);
    border-color: var(--el-color-danger);
}
</style>