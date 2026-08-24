import { get, post, put } from "../../utils/request";
import { fetchEventSource } from "@microsoft/fetch-event-source";
import { getApiBaseUrl } from "../../utils/api-base";

// 运营者收件箱：IM 会话列表 / 已读 / 快捷短语 / SSE 实时事件。
// 全部接口为 Admin+（与手动回复同权限），后端见 internal/handler/im_inbox.go。

export interface IMInboxItem {
  session_id: string;
  platform: string;
  im_channel_id: string;
  channel_name: string;
  agent_id: string;
  user_id: string;
  chat_id: string;
  peer_name: string;
  title: string;
  handling_mode: 'bot' | 'human';
  handling_expires_at?: string;
  unread_count: number;
  last_message_preview: string;
  last_message_role: 'user' | 'assistant' | 'operator' | '';
  last_message_at?: string;
  manual_reply_supported: boolean;
}

export interface IMInboxList {
  items: IMInboxItem[];
  total: number;
  unread_total: number;
}

export interface IMInboxEvent {
  type: 'ready' | 'session';
  item?: IMInboxItem;
  unread_total: number;
}

export type IMInboxFilter = '' | 'human' | 'unread';

export function listImInbox(params: {
  filter?: IMInboxFilter;
  im_channel_id?: string;
  page?: number;
  page_size?: number;
} = {}): Promise<{ data: IMInboxList }> {
  const query = new URLSearchParams();
  if (params.filter) query.set('filter', params.filter);
  if (params.im_channel_id) query.set('im_channel_id', params.im_channel_id);
  if (params.page) query.set('page', String(params.page));
  if (params.page_size) query.set('page_size', String(params.page_size));
  const qs = query.toString();
  return get(`/api/v1/im-inbox${qs ? `?${qs}` : ''}`) as Promise<{ data: IMInboxList }>;
}

export function markImInboxRead(sessionId: string): Promise<{ data: { unread_total: number } }> {
  return post(`/api/v1/im-inbox/sessions/${sessionId}/read`) as Promise<{ data: { unread_total: number } }>;
}

export function getImQuickReplies(): Promise<{ data: { items: string[] } }> {
  return get('/api/v1/im-inbox/quick-replies') as Promise<{ data: { items: string[] } }>;
}

export function putImQuickReplies(items: string[]): Promise<{ data: { items: string[] } }> {
  return put('/api/v1/im-inbox/quick-replies', { items }) as Promise<{ data: { items: string[] } }>;
}

// streamImInbox 建立 SSE 订阅并返回中止函数。fetch-event-source 会在网络断开
// 后自动重连（每次重连重新走 onopen）；组件卸载时必须调用返回的函数。
export function streamImInbox(handlers: {
  onEvent: (evt: IMInboxEvent) => void;
  onOpen?: () => void;
}): () => void {
  const controller = new AbortController();
  const token = localStorage.getItem('weknora_token');
  const tenantId = localStorage.getItem('weknora_selected_tenant_id');

  fetchEventSource(`${getApiBaseUrl()}/api/v1/im-inbox/stream`, {
    method: 'GET',
    headers: {
      Authorization: `Bearer ${token}`,
      ...(tenantId ? { 'X-Tenant-ID': tenantId } : {}),
    },
    signal: controller.signal,
    openWhenHidden: true,
    onopen: async (res) => {
      if (!res.ok) throw new Error(`HTTP ${res.status}`);
      handlers.onOpen?.();
    },
    onmessage: (ev) => {
      if (!ev.data) return;
      try {
        handlers.onEvent(JSON.parse(ev.data) as IMInboxEvent);
      } catch {
        // 心跳或异常载荷：忽略。
      }
    },
    onerror: () => {
      // 返回 undefined → fetch-event-source 按默认退避重连。
    },
  }).catch(() => {
    // 连接被 abort 或彻底失败：静默（页面卸载或由轮询兜底）。
  });

  return () => controller.abort();
}
