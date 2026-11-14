<template>
  <el-container class="layout-container">
    <el-header class="header">
      <div class="header-left">
        <div class="header-title">{{ $t('layout.title') }}</div>
        <el-menu
          class="nav-menu"
          mode="horizontal"
          :default-active="activePath"
          :ellipsis="false"
          router
        >
          <el-menu-item index="/">{{ $t('status.nav.dashboard') }}</el-menu-item>
          <el-menu-item index="/status">{{ $t('status.nav.status') }}</el-menu-item>
        </el-menu>
      </div>
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
import { computed } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import LangSwitcher from '../components/LangSwitcher.vue'

const route = useRoute()
const router = useRouter()

const activePath = computed(() => route.path)

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
.header-left {
    display: flex;
    align-items: center;
    gap: 24px;
    min-width: 0;
}
.nav-menu {
    background: transparent;
    border-bottom: none;
}
.nav-menu :deep(.el-menu-item) {
    height: 48px;
    line-height: 48px;
    color: #cfd3dc;
    border-bottom: 2px solid transparent;
    background: transparent;
}
.nav-menu :deep(.el-menu-item.is-active) {
    color: #ffd04b;
    border-bottom-color: #ffd04b;
    background: transparent;
}
.nav-menu :deep(.el-menu-item:hover),
.nav-menu :deep(.el-menu-item:focus) {
    background: rgba(255, 255, 255, 0.08);
}
@media (max-width: 768px) {
    .header-left {
        gap: 12px;
        width: 100%;
        flex-wrap: wrap;
    }
    .nav-menu :deep(.el-menu-item) {
        height: 40px;
        line-height: 40px;
        padding: 0 12px;
    }
}
</style>