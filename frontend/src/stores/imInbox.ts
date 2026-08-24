import { ref } from 'vue'
import { defineStore } from 'pinia'
import { listImInbox } from '@/api/imInbox'

// 收件箱徽标状态：侧栏菜单显示未读总数。
// 收件箱页面内的 SSE 会持续更新它；不在页面时由 ensureFresh 的一次性
// 拉取兜底（会有滞后，实时性以页面内为准）。
export const useImInboxStore = defineStore('imInboxStore', () => {
  const unreadTotal = ref(0)
  let fetched = false

  const refreshUnread = async () => {
    try {
      const res = await listImInbox({ page_size: 1 })
      unreadTotal.value = res.data?.unread_total ?? 0
    } catch {
      // 非 admin 或网络失败：保持 0，不打扰。
    }
  }

  // 菜单首次展示收件箱入口时调用；仅拉取一次。
  const ensureFresh = () => {
    if (fetched) return
    fetched = true
    refreshUnread()
  }

  return { unreadTotal, refreshUnread, ensureFresh }
})
