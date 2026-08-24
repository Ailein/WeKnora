import assert from 'node:assert/strict'
import test from 'node:test'

import {
  filterInboxItems,
  formatInboxTime,
  inboxPeerLabel,
  isHumanHandling,
  previewRoleKey,
  sortInboxItems,
  upsertInboxItem,
} from './imInbox.ts'
import type { IMInboxItem } from '@/api/imInbox'

const NOW = Date.parse('2026-08-24T12:00:00Z')

function makeItem(overrides: Partial<IMInboxItem>): IMInboxItem {
  return {
    session_id: 's',
    platform: 'whatsapp',
    im_channel_id: 'ch',
    channel_name: '客服号',
    agent_id: 'a',
    user_id: '8613800138000',
    chat_id: '',
    peer_name: '',
    title: '',
    handling_mode: 'bot',
    unread_count: 0,
    last_message_preview: '',
    last_message_role: '',
    manual_reply_supported: true,
    ...overrides,
  }
}

/**
 * 验证置顶排序：未过期的人工接管会话排最前（即便消息更旧），其余按最近
 * 消息倒序；过期接管不置顶。upsert 后顺序保持同一规则。
 */
test('sorts human-held conversations first, then by recency', () => {
  const humanOld = makeItem({
    session_id: 'human-old',
    handling_mode: 'human',
    handling_expires_at: new Date(NOW + 60_000).toISOString(),
    last_message_at: new Date(NOW - 3_600_000).toISOString(),
  })
  const humanExpired = makeItem({
    session_id: 'human-expired',
    handling_mode: 'human',
    handling_expires_at: new Date(NOW - 60_000).toISOString(),
    last_message_at: new Date(NOW - 7_200_000).toISOString(),
  })
  const botNew = makeItem({
    session_id: 'bot-new',
    last_message_at: new Date(NOW - 60_000).toISOString(),
  })

  assert.equal(isHumanHandling(humanOld, NOW), true)
  assert.equal(isHumanHandling(humanExpired, NOW), false)
  // 无到期时间的接管视为持续生效。
  assert.equal(isHumanHandling(makeItem({ handling_mode: 'human' }), NOW), true)

  const sorted = sortInboxItems([humanExpired, botNew, humanOld], NOW)
  assert.deepEqual(
    sorted.map((i) => i.session_id),
    ['human-old', 'bot-new', 'human-expired'],
  )

  // SSE 事件把过期接管会话刷成最新消息 → 仍不置顶，但按时间上浮。
  const refreshed = upsertInboxItem(
    sorted,
    { ...humanExpired, last_message_at: new Date(NOW).toISOString() },
    NOW,
  )
  assert.deepEqual(
    refreshed.map((i) => i.session_id),
    ['human-old', 'human-expired', 'bot-new'],
  )
  assert.equal(refreshed.length, 3, 'upsert 不应产生重复条目')
})

/**
 * 验证过滤器：human 只留未过期接管，unread 只留有未读的；显示名按
 * 昵称 > 群聊 ID > 用户 ID 回落；预览角色映射到 i18n key。
 */
test('filters, peer labels and role keys', () => {
  const items = [
    makeItem({
      session_id: 'h',
      handling_mode: 'human',
      unread_count: 2,
      peer_name: '张三',
    }),
    makeItem({ session_id: 'u', unread_count: 1, chat_id: 'group@g.us' }),
    makeItem({ session_id: 'b' }),
  ]
  assert.deepEqual(filterInboxItems(items, 'human', NOW).map((i) => i.session_id), ['h'])
  assert.deepEqual(filterInboxItems(items, 'unread', NOW).map((i) => i.session_id), ['h', 'u'])
  assert.equal(filterInboxItems(items, '', NOW).length, 3)

  assert.equal(inboxPeerLabel(items[0]), '张三')
  assert.equal(inboxPeerLabel(items[1]), 'group@g.us')
  assert.equal(inboxPeerLabel(items[2]), '8613800138000')

  assert.equal(previewRoleKey('user'), 'imInbox.roleUser')
  assert.equal(previewRoleKey('assistant'), 'imInbox.roleAssistant')
  assert.equal(previewRoleKey('operator'), 'imInbox.roleOperator')
  assert.equal(previewRoleKey(''), '')
})

/**
 * 验证相对时间：分钟/小时/天内用相对表述，7 天以上回落为日期，
 * 非法与缺失输入返回空串。
 */
test('formats inbox timestamps relative to now', () => {
  assert.equal(formatInboxTime(undefined, NOW), '')
  assert.equal(formatInboxTime('not-a-date', NOW), '')
  const min5 = formatInboxTime(new Date(NOW - 5 * 60_000).toISOString(), NOW, 'zh-CN')
  assert.ok(min5.includes('5'), `5 分钟前应含数字 5，got ${min5}`)
  const hour3 = formatInboxTime(new Date(NOW - 3 * 3_600_000).toISOString(), NOW, 'zh-CN')
  assert.ok(hour3.includes('3'), `3 小时前应含数字 3，got ${hour3}`)
  const old = formatInboxTime(new Date(NOW - 30 * 86_400_000).toISOString(), NOW, 'zh-CN')
  assert.ok(/\d{4}/.test(old), `超过 7 天应显示日期，got ${old}`)
})
