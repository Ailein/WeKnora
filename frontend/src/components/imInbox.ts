import type { IMInboxItem } from '@/api/imInbox';

// 收件箱列表的纯逻辑：排序、事件合并、过滤与展示辅助。
// 与 ImInbox.vue 分离以便用 node:test 直接测试。

// 接管是否仍然生效（过期的接管按 bot 显示；后端已做同样归一，这里兜底
// 前端本地过期的场景——列表打开期间窗口到期）。
export function isHumanHandling(item: IMInboxItem, now: number = Date.now()): boolean {
  if (item.handling_mode !== 'human') return false;
  if (!item.handling_expires_at) return true;
  const expires = Date.parse(item.handling_expires_at);
  return Number.isNaN(expires) || expires > now;
}

// 列表排序：人工接管中的置顶，其余按最近消息倒序。
export function sortInboxItems(items: IMInboxItem[], now: number = Date.now()): IMInboxItem[] {
  const ts = (item: IMInboxItem) => {
    const t = item.last_message_at ? Date.parse(item.last_message_at) : NaN;
    return Number.isNaN(t) ? 0 : t;
  };
  return [...items].sort((a, b) => {
    const pinned = Number(isHumanHandling(b, now)) - Number(isHumanHandling(a, now));
    if (pinned !== 0) return pinned;
    return ts(b) - ts(a);
  });
}

// SSE 事件合并：按 session_id 更新或插入，返回重新排序后的新数组。
export function upsertInboxItem(
  items: IMInboxItem[],
  incoming: IMInboxItem,
  now: number = Date.now(),
): IMInboxItem[] {
  const next = items.filter((item) => item.session_id !== incoming.session_id);
  next.push(incoming);
  return sortInboxItems(next, now);
}

// 客户端侧过滤（SSE 更新后无需重新请求列表）。
export function filterInboxItems(
  items: IMInboxItem[],
  filter: '' | 'human' | 'unread',
  now: number = Date.now(),
): IMInboxItem[] {
  switch (filter) {
    case 'human':
      return items.filter((item) => isHumanHandling(item, now));
    case 'unread':
      return items.filter((item) => item.unread_count > 0);
    default:
      return items;
  }
}

// 会话显示名：昵称 > 群聊 ID > 用户 ID。
export function inboxPeerLabel(item: IMInboxItem): string {
  if (item.peer_name) return item.peer_name;
  if (item.chat_id) return item.chat_id;
  return item.user_id;
}

// 预览前缀的 i18n key（'' 表示无前缀）。
export function previewRoleKey(role: IMInboxItem['last_message_role']): string {
  switch (role) {
    case 'user':
      return 'imInbox.roleUser';
    case 'assistant':
      return 'imInbox.roleAssistant';
    case 'operator':
      return 'imInbox.roleOperator';
    default:
      return '';
  }
}

// 相对时间：分钟内/小时内/天内，超过 7 天回落到本地日期。
export function formatInboxTime(
  iso: string | undefined,
  now: number = Date.now(),
  locale: string = 'zh-CN',
): string {
  if (!iso) return '';
  const t = Date.parse(iso);
  if (Number.isNaN(t)) return '';
  const diff = Math.max(0, now - t);
  const minute = 60_000;
  const hour = 60 * minute;
  const day = 24 * hour;
  const rtf = new Intl.RelativeTimeFormat(locale, { numeric: 'auto' });
  if (diff < minute) return rtf.format(0, 'minute');
  if (diff < hour) return rtf.format(-Math.floor(diff / minute), 'minute');
  if (diff < day) return rtf.format(-Math.floor(diff / hour), 'hour');
  if (diff < 7 * day) return rtf.format(-Math.floor(diff / day), 'day');
  return new Date(t).toLocaleDateString(locale);
}
