import type { IMAnalyticsDay } from '@/api/agent/index'

/**
 * 将毫秒时长格式化为看板短文本：<1s 用 "850ms"，<60s 用 "3.2s"，
 * 更长用 "2m05s"。0 或负值表示暂无数据，返回 "—"。
 */
export function formatDurationMs(ms: number): string {
  if (!Number.isFinite(ms) || ms <= 0) return '—'
  if (ms < 1000) return `${Math.round(ms)}ms`
  if (ms < 60_000) {
    const s = ms / 1000
    return `${s >= 10 ? Math.round(s) : Math.round(s * 10) / 10}s`
  }
  const totalSeconds = Math.round(ms / 1000)
  const minutes = Math.floor(totalSeconds / 60)
  const seconds = totalSeconds % 60
  return seconds === 0 ? `${minutes}m` : `${minutes}m${String(seconds).padStart(2, '0')}s`
}

/** 柱状图归一化用的每日最大值（用户消息与机器人+人工回复中较大者，至少 1 避免除零）。 */
export function maxDailyValue(daily: IMAnalyticsDay[]): number {
  let max = 0
  for (const day of daily) {
    max = Math.max(max, day.user_messages, day.bot_replies + day.manual_replies)
  }
  return Math.max(max, 1)
}

/** "YYYY-MM-DD" → "MM-DD" 轴标签。 */
export function dayLabel(date: string): string {
  return date.length >= 10 ? date.slice(5) : date
}

/**
 * X 轴标签抽样步长：柱子多时每 step 根显示一个标签（最后一根始终显示），
 * 保证 90 天视图下轴标签不多于 ~12 个。
 */
export function labelStep(total: number): number {
  if (total <= 14) return 1
  return Math.ceil(total / 12)
}
