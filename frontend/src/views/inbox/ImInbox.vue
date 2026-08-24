<template>
  <div class="im-inbox">
    <!-- 左栏：会话列表 -->
    <aside class="inbox-list">
      <div class="inbox-list__header">
        <div class="inbox-list__title">
          {{ t('imInbox.title') }}
          <span v-if="unreadTotal > 0" class="inbox-badge" :title="t('imInbox.unreadTotalTip')">{{
            unreadTotal > 99 ? '99+' : unreadTotal
          }}</span>
        </div>
        <div class="inbox-filters">
          <button
            v-for="f in filters"
            :key="f.value"
            class="inbox-filter"
            :class="{ 'inbox-filter--active': filter === f.value }"
            @click="filter = f.value"
          >
            {{ t(f.labelKey) }}
          </button>
        </div>
      </div>

      <div class="inbox-list__body" v-if="!listLoading">
        <div v-if="visibleItems.length === 0" class="inbox-empty">
          {{ t(allItems.length === 0 ? 'imInbox.empty' : 'imInbox.emptyFiltered') }}
        </div>
        <div
          v-for="item in visibleItems"
          :key="item.session_id"
          class="inbox-item"
          :class="{
            'inbox-item--active': item.session_id === selectedId,
            'inbox-item--human': isHumanHandling(item),
          }"
          @click="selectItem(item)"
        >
          <div class="inbox-item__top">
            <span class="inbox-item__peer" :title="inboxPeerLabel(item)">{{ inboxPeerLabel(item) }}</span>
            <span class="inbox-item__time">{{ formatInboxTime(item.last_message_at, nowTick, locale) }}</span>
          </div>
          <div class="inbox-item__mid">
            <span class="inbox-item__preview" :title="item.last_message_preview">
              <template v-if="previewRoleKey(item.last_message_role)">{{ t(previewRoleKey(item.last_message_role)) }}：</template>{{ item.last_message_preview || item.title }}
            </span>
            <span v-if="item.unread_count > 0" class="inbox-badge">{{
              item.unread_count > 99 ? '99+' : item.unread_count
            }}</span>
          </div>
          <div class="inbox-item__tags">
            <span class="inbox-tag inbox-tag--platform">{{ platformLabel(item.platform) }}</span>
            <span v-if="item.channel_name" class="inbox-tag">{{ item.channel_name }}</span>
            <span v-if="isHumanHandling(item)" class="inbox-tag inbox-tag--human">{{ t('imInbox.humanTag') }}</span>
          </div>
        </div>
        <div v-if="allItems.length < listTotal" class="inbox-load-more">
          <t-button variant="text" size="small" :loading="loadingMore" @click="loadMore">
            {{ t('imInbox.loadMore') }}
          </t-button>
        </div>
      </div>
      <div v-else class="inbox-empty">{{ t('imInbox.loading') }}</div>
    </aside>

    <!-- 右栏：会话详情 -->
    <section class="inbox-thread">
      <div v-if="!selected" class="inbox-thread__empty">{{ t('imInbox.selectPrompt') }}</div>
      <template v-else>
        <header class="thread-header">
          <div class="thread-header__info">
            <div class="thread-header__peer">{{ inboxPeerLabel(selected) }}</div>
            <div class="thread-header__meta">
              <span class="inbox-tag inbox-tag--platform">{{ platformLabel(selected.platform) }}</span>
              <span v-if="selected.channel_name" class="inbox-tag">{{ selected.channel_name }}</span>
              <span v-if="selected.title" class="thread-header__title" :title="selected.title">{{ selected.title }}</span>
            </div>
          </div>
          <div class="thread-header__actions">
            <template v-if="selected.manual_reply_supported">
              <span v-if="isHumanHandling(selected)" class="inbox-tag inbox-tag--human">
                {{ takeoverStateText }}
              </span>
              <t-dropdown
                v-if="!isHumanHandling(selected)"
                :options="takeoverOptions"
                @click="handleTakeoverStart"
              >
                <t-button size="small" theme="primary" variant="outline">{{ t('imInbox.takeover') }}</t-button>
              </t-dropdown>
              <t-button v-else size="small" variant="outline" @click="handleTakeoverRelease">
                {{ t('imInbox.takeoverRelease') }}
              </t-button>
            </template>
            <t-button size="small" variant="text" @click="openInChat">{{ t('imInbox.openInChat') }}</t-button>
          </div>
        </header>

        <div class="thread-messages" ref="messagesEl">
          <div v-if="hasMoreMessages" class="thread-load-older">
            <t-button variant="text" size="small" :loading="messagesLoading" @click="loadOlderMessages">
              {{ t('imInbox.loadOlder') }}
            </t-button>
          </div>
          <div v-if="messagesLoading && messages.length === 0" class="inbox-empty">{{ t('imInbox.loading') }}</div>
          <div
            v-for="msg in messages"
            :key="msg.id"
            class="thread-msg"
            :class="msg.role === 'user' ? 'thread-msg--user' : 'thread-msg--reply'"
          >
            <div class="thread-msg__bubble">
              <div v-if="channelTagKey(msg)" class="thread-msg__channel">{{ t(channelTagKey(msg)) }}</div>
              <div
                v-if="msg.role !== 'user'"
                class="thread-msg__content thread-msg__content--md"
                v-html="renderReply(msg.content)"
              ></div>
              <div v-else class="thread-msg__content">{{ msg.content }}</div>
              <div class="thread-msg__time">{{ formatMsgTime(msg.created_at) }}</div>
            </div>
          </div>
        </div>

        <footer class="thread-composer">
          <template v-if="selected.manual_reply_supported">
            <div class="composer-toolbar">
              <t-select
                v-if="quickReplies.length > 0"
                :placeholder="t('imInbox.quickReplies')"
                size="small"
                class="composer-quick-select"
                :value="null"
                @change="applyQuickReply"
              >
                <t-option
                  v-for="(qr, idx) in quickReplies"
                  :key="idx"
                  :value="qr"
                  :label="qr.length > 30 ? qr.slice(0, 30) + '…' : qr"
                />
              </t-select>
              <t-button variant="text" size="small" @click="openQuickReplyEditor">
                {{ t('imInbox.quickRepliesManage') }}
              </t-button>
            </div>
            <div class="composer-input">
              <t-textarea
                v-model="draft"
                :autosize="{ minRows: 2, maxRows: 6 }"
                :placeholder="t('imInbox.composerPlaceholder')"
                @keydown="handleComposerKeydown"
              />
              <t-button
                theme="primary"
                :loading="sending"
                :disabled="!draft.trim()"
                @click="sendReply"
              >{{ t('imInbox.send') }}</t-button>
            </div>
          </template>
          <div v-else class="composer-unsupported">{{ t('imInbox.composerUnsupported') }}</div>
        </footer>
      </template>
    </section>

    <!-- 快捷短语编辑 -->
    <t-dialog
      v-model:visible="quickReplyEditorVisible"
      :header="t('imInbox.quickRepliesManage')"
      :confirm-btn="{ content: t('imInbox.save'), loading: quickReplySaving }"
      :cancel-btn="t('imInbox.cancel')"
      width="520px"
      @confirm="saveQuickReplies"
    >
      <div class="quick-reply-editor">
        <div class="quick-reply-editor__hint">{{ t('imInbox.quickRepliesHint') }}</div>
        <t-textarea
          v-model="quickReplyDraft"
          :autosize="{ minRows: 8, maxRows: 16 }"
          :placeholder="t('imInbox.quickRepliesPlaceholder')"
        />
      </div>
    </t-dialog>
  </div>
</template>

<script setup lang="ts">
import { computed, nextTick, onMounted, onUnmounted, ref } from 'vue';
import { useRouter } from 'vue-router';
import { useI18n } from 'vue-i18n';
import { MessagePlugin } from 'tdesign-vue-next';
import {
  listImInbox,
  markImInboxRead,
  getImQuickReplies,
  putImQuickReplies,
  streamImInbox,
  type IMInboxItem,
  type IMInboxEvent,
} from '@/api/imInbox';
import { sendImManualReply, setImSessionHandling, getMessageList } from '@/api/chat';
import {
  filterInboxItems,
  formatInboxTime,
  inboxPeerLabel,
  isHumanHandling,
  previewRoleKey,
  sortInboxItems,
  upsertInboxItem,
} from '@/components/imInbox';
import { useImInboxStore } from '@/stores/imInbox';
import {
  createChatMarkdownRenderer,
  renderChatMarkdown,
} from '@/utils/chatMarkdownRenderer';
import {
  createSafeImage,
  isValidImageURL,
  safeMarkdownToHTML,
  sanitizeMarkdownHTML,
} from '@/utils/security';

const { t, locale } = useI18n();
const router = useRouter();
const inboxStore = useImInboxStore();

const PAGE_SIZE = 100;
const MESSAGE_PAGE = 50;

// ── 列表状态 ──
const allItems = ref<IMInboxItem[]>([]);
const listTotal = ref(0);
const unreadTotal = ref(0);
const listLoading = ref(true);
const loadingMore = ref(false);
const listPage = ref(1);
const filter = ref<'' | 'human' | 'unread'>('');
const filters = [
  { value: '' as const, labelKey: 'imInbox.filterAll' },
  { value: 'human' as const, labelKey: 'imInbox.filterHuman' },
  { value: 'unread' as const, labelKey: 'imInbox.filterUnread' },
];

// 每 30 秒刷新一次相对时间显示。
const nowTick = ref(Date.now());
let tickTimer: ReturnType<typeof setInterval> | null = null;

const visibleItems = computed(() =>
  filterInboxItems(sortInboxItems(allItems.value, nowTick.value), filter.value, nowTick.value),
);

const setUnreadTotal = (n: number) => {
  unreadTotal.value = n;
  inboxStore.unreadTotal = n;
};

const loadList = async () => {
  listLoading.value = true;
  try {
    const res = await listImInbox({ page: 1, page_size: PAGE_SIZE });
    allItems.value = res.data?.items || [];
    listTotal.value = res.data?.total || 0;
    setUnreadTotal(res.data?.unread_total || 0);
    listPage.value = 1;
  } catch {
    MessagePlugin.error(t('imInbox.loadFailed'));
  } finally {
    listLoading.value = false;
  }
};

const loadMore = async () => {
  loadingMore.value = true;
  try {
    const res = await listImInbox({ page: listPage.value + 1, page_size: PAGE_SIZE });
    const fresh = (res.data?.items || []).filter(
      (item) => !allItems.value.some((existing) => existing.session_id === item.session_id),
    );
    allItems.value = [...allItems.value, ...fresh];
    listTotal.value = res.data?.total || listTotal.value;
    listPage.value += 1;
  } catch {
    MessagePlugin.error(t('imInbox.loadFailed'));
  } finally {
    loadingMore.value = false;
  }
};

// ── 会话详情 ──
const selectedId = ref('');
const selected = computed(() => allItems.value.find((i) => i.session_id === selectedId.value) || null);
const messages = ref<any[]>([]);
const messagesLoading = ref(false);
const hasMoreMessages = ref(false);
const messagesEl = ref<HTMLElement | null>(null);

const scrollToBottom = async () => {
  await nextTick();
  if (messagesEl.value) {
    messagesEl.value.scrollTop = messagesEl.value.scrollHeight;
  }
};

const normalizeMessages = (list: any[]) =>
  (list || [])
    .filter((m) => (m?.content || '').trim() !== '')
    .sort((a, b) => Date.parse(a.created_at) - Date.parse(b.created_at));

const loadMessages = async (sessionId: string) => {
  messagesLoading.value = true;
  try {
    const res: any = await getMessageList({ session_id: sessionId, limit: MESSAGE_PAGE, created_at: '' });
    const batch = Array.isArray(res?.data) ? res.data : [];
    if (selectedId.value !== sessionId) return; // 加载期间切换了会话
    messages.value = normalizeMessages(batch);
    hasMoreMessages.value = batch.length >= MESSAGE_PAGE;
    await scrollToBottom();
  } catch {
    MessagePlugin.error(t('imInbox.loadFailed'));
  } finally {
    messagesLoading.value = false;
  }
};

const loadOlderMessages = async () => {
  if (!selectedId.value || messages.value.length === 0) return;
  messagesLoading.value = true;
  const sessionId = selectedId.value;
  try {
    const oldest = messages.value[0];
    const res: any = await getMessageList({
      session_id: sessionId,
      limit: MESSAGE_PAGE,
      created_at: oldest.created_at,
    });
    const batch = Array.isArray(res?.data) ? res.data : [];
    if (selectedId.value !== sessionId) return;
    const existing = new Set(messages.value.map((m) => m.id));
    messages.value = [
      ...normalizeMessages(batch).filter((m) => !existing.has(m.id)),
      ...messages.value,
    ];
    hasMoreMessages.value = batch.length >= MESSAGE_PAGE;
  } catch {
    MessagePlugin.error(t('imInbox.loadFailed'));
  } finally {
    messagesLoading.value = false;
  }
};

const markRead = async (sessionId: string) => {
  try {
    const res = await markImInboxRead(sessionId);
    setUnreadTotal(res.data?.unread_total ?? unreadTotal.value);
    const item = allItems.value.find((i) => i.session_id === sessionId);
    if (item) item.unread_count = 0;
  } catch {
    // 已读标记失败不打扰运营者。
  }
};

const selectItem = (item: IMInboxItem) => {
  if (selectedId.value === item.session_id) return;
  selectedId.value = item.session_id;
  messages.value = [];
  hasMoreMessages.value = false;
  loadMessages(item.session_id);
  if (item.unread_count > 0) markRead(item.session_id);
};

// ── 实时事件 ──
let stopStream: (() => void) | null = null;
let refreshTimer: ReturnType<typeof setTimeout> | null = null;

const handleInboxEvent = (evt: IMInboxEvent) => {
  setUnreadTotal(evt.unread_total ?? unreadTotal.value);
  if (evt.type !== 'session' || !evt.item) return;
  allItems.value = upsertInboxItem(allItems.value, evt.item, nowTick.value);
  if (evt.item.session_id === selectedId.value) {
    // 打开中的会话有新消息：合并刷新最新一页并保持已读。
    if (refreshTimer) clearTimeout(refreshTimer);
    refreshTimer = setTimeout(() => {
      if (!selectedId.value) return;
      loadMessages(selectedId.value);
      if ((evt.item?.unread_count || 0) > 0) markRead(selectedId.value);
    }, 300);
  }
};

// ── 接管控制 ──
const takeoverOptions = computed(() => [
  { content: t('imInbox.takeover30m'), value: 30 },
  { content: t('imInbox.takeover1h'), value: 60 },
  { content: t('imInbox.takeover4h'), value: 240 },
  { content: t('imInbox.takeoverNoExpiry'), value: 0 },
]);

const takeoverStateText = computed(() => {
  if (!selected.value) return '';
  const expires = selected.value.handling_expires_at;
  if (!expires) return t('imInbox.takeoverActiveNoExpiry');
  return t('imInbox.takeoverActive', {
    time: new Date(expires).toLocaleTimeString(locale.value, { hour: '2-digit', minute: '2-digit' }),
  });
});

const applyHandling = (mode: 'bot' | 'human', expiresAt?: string) => {
  const item = allItems.value.find((i) => i.session_id === selectedId.value);
  if (!item) return;
  item.handling_mode = mode;
  item.handling_expires_at = mode === 'human' ? expiresAt : undefined;
  allItems.value = sortInboxItems(allItems.value, nowTick.value);
};

const handleTakeoverStart = async (option: { value: number }) => {
  if (!selectedId.value) return;
  try {
    const res: any = await setImSessionHandling(selectedId.value, 'human', option.value);
    applyHandling('human', res?.data?.expires_at);
    MessagePlugin.success(t('imInbox.takeoverStarted'));
  } catch (err: any) {
    MessagePlugin.error(err?.response?.data?.error || t('imInbox.actionFailed'));
  }
};

const handleTakeoverRelease = async () => {
  if (!selectedId.value) return;
  try {
    await setImSessionHandling(selectedId.value, 'bot');
    applyHandling('bot');
    MessagePlugin.success(t('imInbox.takeoverReleased'));
  } catch (err: any) {
    MessagePlugin.error(err?.response?.data?.error || t('imInbox.actionFailed'));
  }
};

// ── 回复 ──
const draft = ref('');
const sending = ref(false);

const handleComposerKeydown = (_val: string, ctx: { e: KeyboardEvent }) => {
  const e = ctx?.e;
  if (!e || e.key !== 'Enter' || e.shiftKey || e.isComposing) return;
  e.preventDefault();
  sendReply();
};

const sendReply = async () => {
  const content = draft.value.trim();
  if (!content || sending.value || !selectedId.value) return;
  sending.value = true;
  const sessionId = selectedId.value;
  try {
    const res: any = await sendImManualReply(sessionId, content);
    draft.value = '';
    const msg = res?.data;
    if (msg && selectedId.value === sessionId) {
      messages.value = [...messages.value, msg];
      await scrollToBottom();
    }
    // 人工回复即已读；本地立即同步徽标。
    const item = allItems.value.find((i) => i.session_id === sessionId);
    if (item) {
      item.unread_count = 0;
      item.last_message_preview = content;
      item.last_message_role = 'operator';
      item.last_message_at = new Date().toISOString();
    }
  } catch (err: any) {
    MessagePlugin.error(err?.response?.data?.error || t('imInbox.sendFailed'));
  } finally {
    sending.value = false;
  }
};

// ── 快捷短语 ──
const quickReplies = ref<string[]>([]);
const quickReplyEditorVisible = ref(false);
const quickReplyDraft = ref('');
const quickReplySaving = ref(false);

const loadQuickReplies = async () => {
  try {
    const res = await getImQuickReplies();
    quickReplies.value = res.data?.items || [];
  } catch {
    // 短语加载失败不阻塞收件箱。
  }
};

const applyQuickReply = (value: unknown) => {
  if (typeof value !== 'string' || !value) return;
  draft.value = draft.value ? `${draft.value}\n${value}` : value;
};

const openQuickReplyEditor = () => {
  quickReplyDraft.value = quickReplies.value.join('\n');
  quickReplyEditorVisible.value = true;
};

const saveQuickReplies = async () => {
  quickReplySaving.value = true;
  try {
    const items = quickReplyDraft.value.split('\n').map((s) => s.trim()).filter(Boolean);
    const res = await putImQuickReplies(items);
    quickReplies.value = res.data?.items || items;
    quickReplyEditorVisible.value = false;
    MessagePlugin.success(t('imInbox.quickRepliesSaved'));
  } catch (err: any) {
    MessagePlugin.error(err?.response?.data?.error || t('imInbox.actionFailed'));
  } finally {
    quickReplySaving.value = false;
  }
};

// ── 展示辅助 ──
const platformLabel = (platform: string) => {
  const key = `agentEditor.im.${platform}`;
  const label = t(key);
  return label === key ? platform : label;
};

const channelTagKey = (msg: any): string => {
  if (msg.channel === 'im_takeover') return 'imInbox.channelTakeover';
  if (msg.channel === 'im_manual') return 'imInbox.channelManual';
  return '';
};

const formatMsgTime = (iso: string) => {
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return '';
  return d.toLocaleString(locale.value, {
    month: '2-digit',
    day: '2-digit',
    hour: '2-digit',
    minute: '2-digit',
  });
};

// The inbox shows short operator/bot turns, so it needs no mermaid or code
// renderer — but it must still bring its own instance: `marked` keeps a global
// renderer and sharing one would leak the chat view's image validator here.
const markdownRenderer = createChatMarkdownRenderer({
  imageRenderer: ({ href, title, text }) => createSafeImage(href, text || '', title || ''),
  isValidImageUrl: isValidImageURL,
});

const renderReply = (content: string) =>
  renderChatMarkdown(content || '', {
    renderer: markdownRenderer,
    escapeMarkdown: safeMarkdownToHTML,
    sanitizeHtml: sanitizeMarkdownHTML,
  });

const openInChat = () => {
  if (selectedId.value) router.push(`/platform/chat/${selectedId.value}`);
};

// ── 生命周期 ──
onMounted(async () => {
  await loadList();
  loadQuickReplies();
  stopStream = streamImInbox({ onEvent: handleInboxEvent });
  tickTimer = setInterval(() => {
    nowTick.value = Date.now();
  }, 30_000);
});

onUnmounted(() => {
  stopStream?.();
  if (tickTimer) clearInterval(tickTimer);
  if (refreshTimer) clearTimeout(refreshTimer);
});
</script>

<style lang="less" scoped>
.im-inbox {
  flex: 1;
  min-height: 0;
  display: flex;
  background: #fff;
  overflow: hidden;
}

.inbox-badge {
  display: inline-flex;
  align-items: center;
  justify-content: center;
  min-width: 18px;
  height: 18px;
  padding: 0 5px;
  border-radius: 9px;
  background: #e34d59;
  color: #fff;
  font-size: 11px;
  font-weight: 600;
  line-height: 1;
}

// ── 左栏 ──
.inbox-list {
  width: 320px;
  flex-shrink: 0;
  display: flex;
  flex-direction: column;
  border-right: 1px solid #e7e7e7;
  min-height: 0;

  &__header {
    padding: 16px 16px 10px;
    border-bottom: 1px solid #f0f0f0;
  }

  &__title {
    display: flex;
    align-items: center;
    gap: 8px;
    font-size: 16px;
    font-weight: 600;
    color: #000000e6;
    margin-bottom: 10px;
  }

  &__body {
    flex: 1;
    overflow-y: auto;
    min-height: 0;
  }
}

.inbox-filters {
  display: flex;
  gap: 6px;
}

.inbox-filter {
  border: 1px solid #dcdcdc;
  background: #fff;
  border-radius: 14px;
  padding: 3px 12px;
  font-size: 12px;
  color: #00000099;
  cursor: pointer;
  transition: all 0.15s;

  &:hover {
    border-color: #07c05f;
    color: #07c05f;
  }

  &--active {
    background: #07c05f;
    border-color: #07c05f;
    color: #fff;

    &:hover {
      color: #fff;
    }
  }
}

.inbox-empty {
  padding: 40px 16px;
  text-align: center;
  color: #00000066;
  font-size: 13px;
}

.inbox-item {
  padding: 10px 16px;
  cursor: pointer;
  border-bottom: 1px solid #f5f5f5;
  transition: background 0.15s;

  &:hover {
    background: #f7f9f8;
  }

  &--active {
    background: #e8f7ef;

    &:hover {
      background: #e8f7ef;
    }
  }

  &--human {
    border-left: 3px solid #07c05f;
    padding-left: 13px;
  }

  &__top {
    display: flex;
    justify-content: space-between;
    align-items: center;
    gap: 8px;
  }

  &__peer {
    font-size: 14px;
    font-weight: 500;
    color: #000000e6;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  &__time {
    flex-shrink: 0;
    font-size: 11px;
    color: #00000066;
  }

  &__mid {
    display: flex;
    justify-content: space-between;
    align-items: center;
    gap: 8px;
    margin-top: 4px;
  }

  &__preview {
    font-size: 12px;
    color: #00000099;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  &__tags {
    display: flex;
    gap: 4px;
    margin-top: 6px;
    flex-wrap: wrap;
  }
}

.inbox-tag {
  display: inline-block;
  padding: 1px 6px;
  border-radius: 3px;
  font-size: 11px;
  background: #f3f3f3;
  color: #00000099;
  max-width: 140px;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;

  &--platform {
    background: #eef3ff;
    color: #366ef4;
  }

  &--human {
    background: #e8f7ef;
    color: #07c05f;
    font-weight: 500;
  }
}

.inbox-load-more {
  padding: 8px;
  text-align: center;
}

// ── 右栏 ──
.inbox-thread {
  flex: 1;
  min-width: 0;
  display: flex;
  flex-direction: column;
  min-height: 0;

  &__empty {
    flex: 1;
    display: flex;
    align-items: center;
    justify-content: center;
    color: #00000066;
    font-size: 14px;
  }
}

.thread-header {
  display: flex;
  justify-content: space-between;
  align-items: center;
  gap: 12px;
  padding: 12px 20px;
  border-bottom: 1px solid #f0f0f0;

  &__peer {
    font-size: 15px;
    font-weight: 600;
    color: #000000e6;
  }

  &__meta {
    display: flex;
    align-items: center;
    gap: 6px;
    margin-top: 4px;
  }

  &__title {
    font-size: 12px;
    color: #00000066;
    max-width: 260px;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  &__actions {
    display: flex;
    align-items: center;
    gap: 8px;
    flex-shrink: 0;
  }
}

.thread-messages {
  flex: 1;
  overflow-y: auto;
  min-height: 0;
  padding: 16px 20px;
  background: #fafafa;
}

.thread-load-older {
  text-align: center;
  margin-bottom: 8px;
}

.thread-msg {
  display: flex;
  margin-bottom: 12px;

  &--user {
    justify-content: flex-start;

    .thread-msg__bubble {
      background: #fff;
      border: 1px solid #ececec;
    }
  }

  &--reply {
    justify-content: flex-end;

    .thread-msg__bubble {
      background: #e8f7ef;
    }
  }

  &__bubble {
    max-width: 72%;
    border-radius: 8px;
    padding: 8px 12px;
  }

  &__channel {
    font-size: 11px;
    color: #07a355;
    margin-bottom: 3px;
    font-weight: 500;
  }

  &__content {
    font-size: 13px;
    color: #000000e6;
    white-space: pre-wrap;
    word-break: break-word;

    &--md {
      white-space: normal;

      :deep(p) {
        margin: 0 0 6px;

        &:last-child {
          margin-bottom: 0;
        }
      }

      :deep(pre) {
        background: #f5f5f5;
        border-radius: 4px;
        padding: 8px;
        overflow-x: auto;
        font-size: 12px;
      }

      :deep(ul),
      :deep(ol) {
        margin: 4px 0;
        padding-left: 18px;
      }
    }
  }

  &__time {
    font-size: 10px;
    color: #00000059;
    margin-top: 4px;
    text-align: right;
  }
}

.thread-composer {
  border-top: 1px solid #f0f0f0;
  padding: 10px 20px 14px;
}

.composer-toolbar {
  display: flex;
  align-items: center;
  gap: 8px;
  margin-bottom: 8px;
}

.composer-quick-select {
  width: 220px;
}

.composer-input {
  display: flex;
  align-items: flex-end;
  gap: 10px;

  :deep(.t-textarea) {
    flex: 1;
  }
}

.composer-unsupported {
  text-align: center;
  color: #00000066;
  font-size: 13px;
  padding: 6px 0;
}

.quick-reply-editor {
  &__hint {
    font-size: 12px;
    color: #00000099;
    margin-bottom: 8px;
  }
}
</style>
