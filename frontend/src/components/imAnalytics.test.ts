import assert from 'node:assert/strict'
import test from 'node:test'

import { dayLabel, formatDurationMs, labelStep, maxDailyValue } from './imAnalytics.ts'
import type { IMAnalyticsDay } from '@/api/agent/index'

function day(partial: Partial<IMAnalyticsDay>): IMAnalyticsDay {
  return {
    date: '2026-08-24',
    user_messages: 0,
    bot_replies: 0,
    manual_replies: 0,
    new_sessions: 0,
    active_users: 0,
    ...partial,
  }
}

/**
 * 验证时长格式化在各量级下输出简短可读的文本，且无数据（0/负值/NaN）时显示占位符。
 * 传入代表性毫秒值，期望毫秒、秒（一位小数与取整）、分钟三个量级的格式各自正确。
 */
test('formats reply durations across magnitudes', () => {
  assert.equal(formatDurationMs(0), '—')
  assert.equal(formatDurationMs(-5), '—')
  assert.equal(formatDurationMs(Number.NaN), '—')
  assert.equal(formatDurationMs(850), '850ms')
  assert.equal(formatDurationMs(3210), '3.2s')
  assert.equal(formatDurationMs(15600), '16s')
  assert.equal(formatDurationMs(60_000), '1m')
  assert.equal(formatDurationMs(125_000), '2m05s')
})

/**
 * 验证柱状图归一化取「用户消息」与「机器人+人工回复合计」的全局最大值，
 * 且全零序列返回 1 以避免除零。传入混合数据，期望 max 为合计回复 7。
 */
test('computes the daily maximum across both bar series', () => {
  const daily = [
    day({ user_messages: 5, bot_replies: 4 }),
    day({ user_messages: 2, bot_replies: 6, manual_replies: 1 }),
  ]
  assert.equal(maxDailyValue(daily), 7)
  assert.equal(maxDailyValue([]), 1)
  assert.equal(maxDailyValue([day({})]), 1)
})

/**
 * 验证轴标签与抽样步长：日期缩短为 MM-DD；14 天内逐日显示，
 * 90 天时步长保证标签数不超过 12 个左右。
 */
test('shortens axis labels and samples long ranges', () => {
  assert.equal(dayLabel('2026-08-24'), '08-24')
  assert.equal(dayLabel('bad'), 'bad')
  assert.equal(labelStep(7), 1)
  assert.equal(labelStep(14), 1)
  assert.equal(labelStep(30), 3)
  assert.equal(labelStep(90), 8)
  assert.ok(Math.ceil(90 / labelStep(90)) <= 13)
})
