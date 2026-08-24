<template>
  <div class="im-analytics">
    <div class="im-analytics__header">
      <div class="im-analytics__heading">
        <h3 class="im-analytics__title">{{ $t('imAnalytics.title') }}</h3>
        <span v-if="result" class="im-analytics__range-hint">{{ result.start_date }} ~ {{ result.end_date }}</span>
      </div>
      <div class="im-analytics__controls">
        <t-select
          v-model="channelId"
          class="im-analytics__channel-select"
          size="small"
          clearable
          :placeholder="$t('imAnalytics.allChannels')"
          :options="channelOptions"
        />
        <t-radio-group v-model="days" variant="default-filled" size="small">
          <t-radio-button :value="7">{{ $t('imAnalytics.range7') }}</t-radio-button>
          <t-radio-button :value="30">{{ $t('imAnalytics.range30') }}</t-radio-button>
          <t-radio-button :value="90">{{ $t('imAnalytics.range90') }}</t-radio-button>
        </t-radio-group>
      </div>
    </div>

    <t-loading :loading="loading" show-overlay>
      <div class="im-analytics__cards">
        <div v-for="card in cards" :key="card.key" class="im-analytics__card">
          <span class="im-analytics__card-label">{{ $t(`imAnalytics.cards.${card.key}`) }}</span>
          <span class="im-analytics__card-value">{{ card.value }}</span>
          <span v-if="card.hint" class="im-analytics__card-hint">{{ card.hint }}</span>
        </div>
      </div>

      <div class="im-analytics__chart-block">
        <div class="im-analytics__chart-head">
          <span class="im-analytics__block-title">{{ $t('imAnalytics.chart.title') }}</span>
          <span class="im-analytics__legend">
            <i class="im-analytics__dot im-analytics__dot--user" />{{ $t('imAnalytics.chart.legendUser') }}
            <i class="im-analytics__dot im-analytics__dot--bot" />{{ $t('imAnalytics.chart.legendBot') }}
          </span>
        </div>
        <div v-if="hasTraffic" class="im-analytics__chart">
          <div
            v-for="(day, i) in daily"
            :key="day.date"
            class="im-analytics__col"
            :title="barTitle(day)"
          >
            <div class="im-analytics__bars">
              <div class="im-analytics__bar im-analytics__bar--user" :style="{ height: barHeight(day.user_messages) }" />
              <div class="im-analytics__bar im-analytics__bar--bot" :style="{ height: barHeight(day.bot_replies + day.manual_replies) }" />
            </div>
            <span class="im-analytics__col-label">{{ colLabel(i, day.date) }}</span>
          </div>
        </div>
        <div v-else class="im-analytics__empty">{{ $t('imAnalytics.chart.empty') }}</div>
      </div>

      <div class="im-analytics__tables">
        <div class="im-analytics__table-block">
          <span class="im-analytics__block-title">{{ $t('imAnalytics.channels.title') }}</span>
          <table class="im-analytics__table">
            <thead>
              <tr>
                <th>{{ $t('imAnalytics.channels.colChannel') }}</th>
                <th>{{ $t('imAnalytics.channels.colPlatform') }}</th>
                <th class="im-analytics__num">{{ $t('imAnalytics.channels.colSessions') }}</th>
                <th class="im-analytics__num">{{ $t('imAnalytics.channels.colMessages') }}</th>
              </tr>
            </thead>
            <tbody>
              <tr v-for="ch in result?.channels || []" :key="ch.im_channel_id">
                <td>{{ ch.name || $t('imAnalytics.channels.deleted') }}</td>
                <td>{{ ch.platform || '—' }}</td>
                <td class="im-analytics__num">{{ ch.sessions }}</td>
                <td class="im-analytics__num">{{ ch.user_messages }}</td>
              </tr>
              <tr v-if="!result?.channels?.length">
                <td colspan="4" class="im-analytics__table-empty">{{ $t('imAnalytics.noData') }}</td>
              </tr>
            </tbody>
          </table>
        </div>

        <div class="im-analytics__table-block">
          <span class="im-analytics__block-title">{{ $t('imAnalytics.topUsers.title') }}</span>
          <table class="im-analytics__table">
            <thead>
              <tr>
                <th>{{ $t('imAnalytics.topUsers.colUser') }}</th>
                <th>{{ $t('imAnalytics.topUsers.colPlatform') }}</th>
                <th class="im-analytics__num">{{ $t('imAnalytics.topUsers.colMessages') }}</th>
                <th class="im-analytics__num">{{ $t('imAnalytics.topUsers.colLastActive') }}</th>
              </tr>
            </thead>
            <tbody>
              <tr v-for="u in result?.top_users || []" :key="`${u.platform}:${u.user_id}`">
                <td class="im-analytics__user-id" :title="u.user_id">{{ u.user_id }}</td>
                <td>{{ u.platform }}</td>
                <td class="im-analytics__num">{{ u.messages }}</td>
                <td class="im-analytics__num">{{ u.last_active_date }}</td>
              </tr>
              <tr v-if="!result?.top_users?.length">
                <td colspan="4" class="im-analytics__table-empty">{{ $t('imAnalytics.noData') }}</td>
              </tr>
            </tbody>
          </table>
        </div>
      </div>
    </t-loading>
  </div>
</template>

<script setup lang="ts">
import { computed, onMounted, ref, watch } from 'vue';
import { useI18n } from 'vue-i18n';
import { MessagePlugin } from 'tdesign-vue-next';
import {
  getIMAnalytics,
  listAllIMChannels,
  type IMAnalyticsDay,
  type IMAnalyticsResult,
} from '@/api/agent/index';
import { dayLabel, formatDurationMs, labelStep, maxDailyValue } from './imAnalytics';

const { t } = useI18n();

const days = ref<7 | 30 | 90>(7);
const channelId = ref('');
const loading = ref(false);
const result = ref<IMAnalyticsResult | null>(null);
const channelOptions = ref<{ label: string; value: string }[]>([]);

const daily = computed(() => result.value?.daily || []);
const hasTraffic = computed(() =>
  daily.value.some((d) => d.user_messages + d.bot_replies + d.manual_replies > 0),
);
const dailyMax = computed(() => maxDailyValue(daily.value));

const cards = computed(() => {
  const totals = result.value?.totals;
  const fmt = (n?: number) => (n ?? 0).toLocaleString();
  return [
    { key: 'activeSessions', value: fmt(totals?.active_sessions) },
    { key: 'newSessions', value: fmt(totals?.new_sessions) },
    { key: 'activeUsers', value: fmt(totals?.active_users) },
    { key: 'userMessages', value: fmt(totals?.user_messages) },
    { key: 'botReplies', value: fmt(totals?.bot_replies) },
    { key: 'manualReplies', value: fmt(totals?.manual_replies) },
    {
      key: 'takeoverSessions',
      value: fmt(totals?.takeover_sessions),
      hint: totals?.human_handled_now
        ? t('imAnalytics.cards.humanHandledNow', { count: totals.human_handled_now })
        : '',
    },
    { key: 'avgBotReply', value: formatDurationMs(totals?.avg_bot_reply_ms ?? 0) },
  ];
});

function barHeight(value: number): string {
  return `${Math.round((value / dailyMax.value) * 100)}%`;
}

function barTitle(day: IMAnalyticsDay): string {
  return `${day.date}\n${t('imAnalytics.chart.legendUser')}: ${day.user_messages}\n${t('imAnalytics.chart.legendBot')}: ${day.bot_replies + day.manual_replies}`;
}

function colLabel(index: number, date: string): string {
  const total = daily.value.length;
  const step = labelStep(total);
  return index % step === 0 || index === total - 1 ? dayLabel(date) : '';
}

async function fetchAnalytics() {
  loading.value = true;
  try {
    const res = await getIMAnalytics({
      days: days.value,
      tz_offset_minutes: -new Date().getTimezoneOffset(),
      im_channel_id: channelId.value || undefined,
    });
    result.value = res.data;
  } catch (e: any) {
    MessagePlugin.error(e?.message || t('imAnalytics.loadFailed'));
  } finally {
    loading.value = false;
  }
}

async function fetchChannels() {
  try {
    const res = await listAllIMChannels();
    channelOptions.value = (res.data || []).map((ch) => ({
      label: ch.name || ch.platform,
      value: ch.id,
    }));
  } catch {
    // The filter dropdown is a convenience; the report itself already loaded.
  }
}

watch([days, channelId], fetchAnalytics);
onMounted(() => {
  fetchAnalytics();
  fetchChannels();
});
</script>

<style scoped lang="less">
.im-analytics {
  margin-top: 32px;
  padding: 20px;
  border: 1px solid var(--td-component-border, #e7e7e7);
  border-radius: 12px;
  background: var(--td-bg-color-container, #fff);
}

.im-analytics__header {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 12px;
  flex-wrap: wrap;
  margin-bottom: 16px;
}

.im-analytics__heading {
  display: flex;
  align-items: baseline;
  gap: 10px;
}

.im-analytics__title {
  margin: 0;
  font-size: 16px;
  font-weight: 600;
  color: var(--td-text-color-primary, #000);
}

.im-analytics__range-hint {
  font-size: 12px;
  color: var(--td-text-color-placeholder, #999);
}

.im-analytics__controls {
  display: flex;
  align-items: center;
  gap: 8px;
}

.im-analytics__channel-select {
  width: 180px;
}

.im-analytics__cards {
  display: grid;
  grid-template-columns: repeat(auto-fit, minmax(130px, 1fr));
  gap: 10px;
  margin-bottom: 20px;
}

.im-analytics__card {
  display: flex;
  flex-direction: column;
  gap: 4px;
  padding: 12px 14px;
  border-radius: 10px;
  background: var(--td-bg-color-secondarycontainer, #f5f6f7);
}

.im-analytics__card-label {
  font-size: 12px;
  color: var(--td-text-color-secondary, #666);
}

.im-analytics__card-value {
  font-size: 20px;
  font-weight: 600;
  color: var(--td-text-color-primary, #000);
  font-variant-numeric: tabular-nums;
}

.im-analytics__card-hint {
  font-size: 11px;
  color: var(--td-warning-color, #e37318);
}

.im-analytics__block-title {
  font-size: 13px;
  font-weight: 600;
  color: var(--td-text-color-primary, #000);
}

.im-analytics__chart-block {
  margin-bottom: 20px;
}

.im-analytics__chart-head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  margin-bottom: 8px;
}

.im-analytics__legend {
  display: inline-flex;
  align-items: center;
  gap: 6px;
  font-size: 12px;
  color: var(--td-text-color-secondary, #666);
}

.im-analytics__dot {
  width: 8px;
  height: 8px;
  border-radius: 2px;
  display: inline-block;

  &--user {
    background: var(--td-brand-color, #0052d9);
  }

  &--bot {
    background: var(--td-success-color, #2ba471);
  }
}

.im-analytics__chart {
  display: flex;
  align-items: flex-end;
  gap: 2px;
  height: 160px;
  padding: 8px 4px 0;
  border-bottom: 1px solid var(--td-component-border, #e7e7e7);
}

.im-analytics__col {
  flex: 1;
  min-width: 0;
  display: flex;
  flex-direction: column;
  align-items: center;
  height: 100%;
}

.im-analytics__bars {
  display: flex;
  align-items: flex-end;
  gap: 1px;
  flex: 1;
  width: 100%;
  max-width: 26px;
}

.im-analytics__bar {
  flex: 1;
  min-height: 1px;
  border-radius: 2px 2px 0 0;

  &--user {
    background: var(--td-brand-color, #0052d9);
  }

  &--bot {
    background: var(--td-success-color, #2ba471);
  }
}

.im-analytics__col-label {
  height: 16px;
  margin-top: 2px;
  font-size: 10px;
  color: var(--td-text-color-placeholder, #999);
  white-space: nowrap;
}

.im-analytics__empty {
  padding: 40px 0;
  text-align: center;
  font-size: 13px;
  color: var(--td-text-color-placeholder, #999);
}

.im-analytics__tables {
  display: grid;
  grid-template-columns: repeat(auto-fit, minmax(300px, 1fr));
  gap: 20px;
}

.im-analytics__table {
  width: 100%;
  margin-top: 8px;
  border-collapse: collapse;
  font-size: 13px;

  th,
  td {
    padding: 6px 8px;
    text-align: left;
    border-bottom: 1px solid var(--td-component-stroke, #f0f0f0);
    color: var(--td-text-color-primary, #333);
  }

  th {
    font-weight: 500;
    color: var(--td-text-color-secondary, #666);
  }
}

.im-analytics__num {
  text-align: right !important;
  font-variant-numeric: tabular-nums;
}

.im-analytics__user-id {
  max-width: 180px;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.im-analytics__table-empty {
  text-align: center;
  color: var(--td-text-color-placeholder, #999);
}
</style>
