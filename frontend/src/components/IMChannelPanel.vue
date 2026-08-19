<template>
  <div class="im-panel">
    <div class="channels-section">
      <div class="channels-header">
        <span class="channels-title">{{ $t('agentEditor.im.channelsTitle') }}</span>
        <IntegrationsAgentFilter v-model="filterAgentId" :agents="agents" />
        <span class="channels-count">{{ channels.length }}</span>
      </div>

      <t-loading :loading="loading" size="small" class="channels-loading-wrap">
        <div v-if="!loading && channels.length === 0 && !authStore.hasRole('admin')" class="channels-empty">
          <t-empty :description="$t('agentEditor.im.empty')" />
        </div>

        <div v-else-if="!loading" class="channel-grid">
          <button v-for="channel in channels" :key="channel.id" type="button"
            class="channel-card channel-card--clickable" @click="openDrawer(channel)">
            <div class="channel-card__badge" :class="`channel-card__badge--${channel.platform}`">
              <img v-if="platformLogo(channel.platform)" :src="platformLogo(channel.platform)"
                :alt="platformLabel(channel.platform)" class="channel-card__logo" />
              <t-icon v-else name="chat-message" size="22px" />
            </div>
            <div class="channel-card__body">
              <div class="channel-card__header">
                <h3 class="channel-card__title">{{ channel.name || $t('agentEditor.im.unnamed') }}</h3>
                <t-tag v-if="!channel.enabled" size="small" variant="light" theme="warning">
                  {{ $t('agentEditor.im.disabled') }}
                </t-tag>
                <span v-else-if="cardStatus(channel)" class="channel-card__status"
                  :class="`channel-card__status--${statusTone(cardStatus(channel)?.state)}`"
                  :title="cardStatus(channel)?.detail || ''">
                  <span class="channel-card__status-dot" aria-hidden="true" />
                  {{ statusText(cardStatus(channel)?.state) }}
                </span>
              </div>
              <span v-if="agentDisplayName(channel)" class="channel-card__agent-name">
                {{ agentDisplayName(channel) }}
              </span>
            </div>
            <div v-if="authStore.hasRole('admin')" class="channel-card__actions" @click.stop>
              <t-dropdown trigger="click" placement="bottom-right" attach="body" :options="channelMenuOptions(channel)"
                @click="handleChannelMenuClick($event, channel)">
                <t-button variant="text" shape="square" size="small" class="channel-card__action-btn channel-card__more"
                  @click.stop>
                  <template #icon><t-icon name="ellipsis" /></template>
                </t-button>
              </t-dropdown>
              <t-popconfirm :content="$t('agentEditor.im.deleteConfirm')"
                :confirm-btn="{ content: $t('common.delete'), theme: 'danger' }"
                :cancel-btn="{ content: $t('common.cancel') }" placement="bottom-right"
                @confirm="() => handleDelete(channel.id)">
                <t-tooltip :content="$t('common.delete')" placement="top">
                  <t-button theme="danger" shape="square" variant="text" size="small"
                    class="channel-card__action-btn channel-card__delete" @click.stop>
                    <template #icon><t-icon name="delete" /></template>
                  </t-button>
                </t-tooltip>
              </t-popconfirm>
            </div>
          </button>

          <button v-if="authStore.hasRole('admin')" type="button" class="channel-card channel-card--add"
            @click="openCreate">
            <span class="channel-card__badge" aria-hidden="true">
              <t-icon name="add" />
            </span>
            <div class="channel-card__body">
              <div class="channel-card__header">
                <span class="channel-card__title">{{ $t('agentEditor.im.addChannel') }}</span>
              </div>
            </div>
            <span class="channel-card__actions channel-card__actions--spacer" aria-hidden="true" />
          </button>
        </div>
      </t-loading>
    </div>

    <SettingDrawer v-model:visible="showCreateDialog" class="im-channel-drawer" :title="drawerTitle"
      :description="drawerStepDescription" storage-key="setting-drawer:im-channel" width="560px"
      :confirm-loading="saving" :confirm-text="drawerConfirmText" :hide-footer="!authStore.hasRole('admin')"
      @confirm="handleDrawerConfirm" @cancel="resetForm">
      <template #headerIcon>
        <img v-if="platformLogo(formData.platform)" :src="platformLogo(formData.platform)"
          :alt="platformLabel(formData.platform)" class="drawer-platform-icon" />
        <t-icon v-else name="chat-message" />
      </template>

      <template v-if="wizardStep > 0" #footer-left>
        <t-button variant="outline" @click="prevWizardStep">
          {{ $t('datasource.back') }}
        </t-button>
      </template>

      <div class="im-steps">
        <div v-for="(title, i) in stepTitles" :key="i"
          :class="['im-step', { active: wizardStep === i, done: wizardStep > i }]">
          <span class="im-step-num">
            <t-icon v-if="wizardStep > i" name="check" class="im-step-check" />
            <template v-else>{{ i + 1 }}</template>
          </span>
          <span class="im-step-title">{{ title }}</span>
        </div>
      </div>

      <!-- Step 1: Basic -->
      <div v-if="wizardStep === 0" class="im-step-body">
        <section class="setting-drawer__section im-drawer__section">
          <h4 class="setting-drawer__section-title">{{ $t('agentEditor.im.sectionChannel') }}</h4>

          <div class="form-item">
            <label class="form-label required">{{ $t('integrations.boundAgent') }}</label>
            <div class="agent-field-row">
              <t-select v-model="formData.target_agent_id" :options="agentOptions" filterable
                :placeholder="$t('integrations.selectAgentPlaceholder')" />
            </div>
          </div>

          <div class="form-item">
            <label class="form-label required">{{ $t('agentEditor.im.platform') }}</label>
            <t-select v-model="formData.platform" :disabled="!!editingChannel" class="im-platform-select"
              @change="onPlatformChange">
              <template v-if="platformLogo(formData.platform)" #prefixIcon>
                <img :src="platformLogo(formData.platform)" :alt="platformLabel(formData.platform)"
                  class="im-platform-select-prefix" />
              </template>
              <t-option v-for="item in platformOptions" :key="item.value" :value="item.value" :label="item.label">
                <div class="im-platform-select-option">
                  <img :src="item.logo" :alt="item.label" class="im-platform-select-option__icon" />
                  <span>{{ item.label }}</span>
                </div>
              </t-option>
            </t-select>
          </div>

          <div class="form-item">
            <label class="form-label">{{ $t('agentEditor.im.channelName') }}</label>
            <t-input v-model="formData.name" :placeholder="$t('agentEditor.im.channelNamePlaceholder')"
              @focus="channelNameTouched = true" />
            <p v-if="!editingChannel" class="form-desc">{{ $t('agentEditor.im.channelNameDefaultHint') }}</p>
          </div>

          <div v-if="editingChannel && authStore.hasRole('admin')" class="setting-row setting-row--last">
            <div class="setting-info">
              <label>{{ $t('agentEditor.im.enabled') }}</label>
            </div>
            <div class="setting-control">
              <t-switch v-model="editingEnabled" size="small" />
            </div>
          </div>
        </section>
      </div>

      <!-- Step 2: Connection -->
      <div v-else-if="wizardStep === 1" class="im-step-body">
        <!-- WeChat and WhatsApp run on a fixed connection mode with full output -->
        <section v-if="formData.platform !== 'wechat' && formData.platform !== 'whatsapp'"
          class="setting-drawer__section im-drawer__section">
          <h4 class="setting-drawer__section-title">{{ $t('agentEditor.im.sectionAccess') }}</h4>

          <div class="form-item">
            <label class="form-label required">{{ $t('agentEditor.im.mode') }}</label>
            <div class="option-chips">
              <button type="button" class="option-chip"
                :class="{ 'option-chip--active': formData.mode === 'websocket' }"
                :disabled="formData.platform === 'mattermost'"
                @click="formData.mode = 'websocket'">
                WebSocket
              </button>
              <button type="button" class="option-chip" :class="{ 'option-chip--active': formData.mode === 'webhook' }"
                @click="formData.mode = 'webhook'">
                Webhook
              </button>
            </div>
            <p class="form-desc">
              {{ formData.platform === 'mattermost' ? $t('agentEditor.im.mattermostModeHint') :
                formData.platform === 'yunzhijia' ? $t('agentEditor.im.yunzhijiaModeHint') :
                  $t('agentEditor.im.modeHint') }}
            </p>
          </div>

          <div class="form-item">
            <label class="form-label required">{{ $t('agentEditor.im.outputMode') }}</label>
            <div class="option-chips">
              <button type="button" class="option-chip"
                :class="{ 'option-chip--active': formData.output_mode === 'stream' }"
                @click="formData.output_mode = 'stream'">
                {{ $t('agentEditor.im.outputStream') }}
              </button>
              <button type="button" class="option-chip"
                :class="{ 'option-chip--active': formData.output_mode === 'full' }"
                @click="formData.output_mode = 'full'">
                {{ $t('agentEditor.im.outputFull') }}
              </button>
            </div>
          </div>
        </section>

        <section class="setting-drawer__section im-drawer__section">
          <h4 class="setting-drawer__section-title">{{ $t('agentEditor.im.sectionSession') }}</h4>
          <div class="form-item">
            <label class="form-label required">{{ $t('agentEditor.im.sessionMode') }}</label>
            <div class="option-chips">
              <button type="button" class="option-chip"
                :class="{ 'option-chip--active': formData.session_mode === 'user' }"
                @click="formData.session_mode = 'user'">
                {{ $t('agentEditor.im.sessionModeUser') }}
              </button>
              <button type="button" class="option-chip"
                :class="{ 'option-chip--active': formData.session_mode === 'thread' }"
                :disabled="!platformSupportsThread(formData.platform)" @click="formData.session_mode = 'thread'">
                {{ $t('agentEditor.im.sessionModeThread') }}
              </button>
            </div>
            <p class="form-desc">{{ $t('agentEditor.im.sessionModeHint') }}</p>
          </div>
        </section>

        <section v-if="editingChannel && formData.mode === 'webhook'"
          class="setting-drawer__section im-drawer__section">
          <h4 class="setting-drawer__section-title">{{ $t('agentEditor.im.sectionCallback') }}</h4>
          <div class="form-item">
            <label class="form-label">{{ $t('agentEditor.im.callbackUrl') }}</label>
            <div class="callback-url-control">
              <t-input :model-value="getCallbackUrl(editingChannel)" readonly
                class="mono-text-input callback-url-input" />
              <t-button size="small" variant="text" :title="$t('common.copy')" @click="copyUrl(editingChannel)">
                <t-icon name="file-copy" />
              </t-button>
            </div>
          </div>
        </section>
      </div>

      <!-- Step 3: File knowledge base -->
      <div v-else-if="wizardStep === 2" class="im-step-body">
        <section class="setting-drawer__section im-drawer__section">
          <h4 class="setting-drawer__section-title">{{ $t('agentEditor.im.sectionKnowledge') }}</h4>
          <div class="form-item">
            <label class="form-label">{{ $t('agentEditor.im.fileKnowledgeBase') }}</label>
            <t-select v-model="formData.knowledge_base_id"
              :placeholder="$t('agentEditor.im.fileKnowledgeBasePlaceholder')" clearable filterable>
              <t-option v-for="kb in knowledgeBases" :key="kb.id" :value="kb.id" :label="kb.name" />
            </t-select>
            <p class="form-desc">{{ $t('agentEditor.im.fileKnowledgeBaseHint') }}</p>
          </div>
        </section>
      </div>

      <!-- Step 4: Credentials -->
      <div v-else class="im-step-body">
        <section class="setting-drawer__section im-drawer__section">
          <h4 class="setting-drawer__section-title">{{ $t('agentEditor.im.sectionCredentials') }}</h4>
          <div class="drawer-form">
            <!-- WeCom credentials -->
            <template v-if="formData.platform === 'wecom'">
              <div class="platform-link-hint">
                <a href="https://work.weixin.qq.com/" target="_blank" rel="noopener noreferrer" class="doc-link">
                  {{ $t('agentEditor.im.wecomConsole') }}
                  <t-icon name="link" class="link-icon" />
                </a>
                <span class="hint-text">{{ $t('agentEditor.im.consoleTip') }}</span>
              </div>
              <template v-if="formData.mode === 'websocket'">
                <div class="form-item">
                  <label class="form-label">Bot ID</label>
                  <t-input v-model="formData.credentials.bot_id" placeholder="Bot ID" />
                </div>
                <div class="form-item">
                  <label class="form-label">Bot Secret</label>
                  <t-input v-model="formData.credentials.bot_secret" type="password" placeholder="Bot Secret" />
                </div>
                <div class="form-item">
                  <label class="form-label">WebSocket Endpoint</label>
                  <t-input v-model="formData.credentials.ws_endpoint" placeholder="wss://openws.work.weixin.qq.com" />
                  <p class="form-desc">{{ $t('agentEditor.im.wecomWSEndpointHint') }}</p>
                </div>
              </template>
              <template v-else>
                <div class="form-item">
                  <label class="form-label">Corp ID</label>
                  <t-input v-model="formData.credentials.corp_id" placeholder="Corp ID" />
                </div>
                <div class="form-item">
                  <label class="form-label">Agent Secret</label>
                  <t-input v-model="formData.credentials.agent_secret" type="password" placeholder="Agent Secret" />
                </div>
                <div class="form-item">
                  <label class="form-label">Token</label>
                  <t-input v-model="formData.credentials.token" placeholder="Token" />
                </div>
                <div class="form-item">
                  <label class="form-label">EncodingAESKey</label>
                  <t-input v-model="formData.credentials.encoding_aes_key" placeholder="EncodingAESKey" />
                </div>
                <div class="form-item">
                  <label class="form-label">Corp Agent ID</label>
                  <t-input-number v-model="formData.credentials.corp_agent_id" placeholder="Corp Agent ID"
                    style="width: 100%;" />
                </div>
                <div class="form-item">
                  <label class="form-label">API Base URL</label>
                  <t-input v-model="formData.credentials.api_base_url" placeholder="https://qyapi.weixin.qq.com" />
                  <p class="form-desc">{{ $t('agentEditor.im.wecomAPIBaseURLHint') }}</p>
                </div>
              </template>
            </template>

            <!-- Feishu / Lark credentials — same fields, different open platform -->
            <template v-if="formData.platform === 'feishu' || formData.platform === 'lark'">
              <div class="platform-link-hint">
                <a :href="openPlatformConsole.url" target="_blank" rel="noopener noreferrer" class="doc-link">
                  {{ $t(openPlatformConsole.labelKey) }}
                  <t-icon name="link" class="link-icon" />
                </a>
                <span class="hint-text">{{ $t('agentEditor.im.consoleTip') }}</span>
              </div>
              <div class="form-item">
                <label class="form-label">App ID</label>
                <t-input v-model="formData.credentials.app_id" placeholder="App ID" />
              </div>
              <div class="form-item">
                <label class="form-label">App Secret</label>
                <t-input v-model="formData.credentials.app_secret" type="password" placeholder="App Secret" />
              </div>
              <div class="form-item">
                <label class="form-label">Base URL</label>
                <t-input v-model="formData.credentials.api_base_url" placeholder="https://open.feishu.cn" />
                <p class="form-desc">{{ $t('agentEditor.im.feishuAPIBaseURLHint') }}</p>
              </div>
              <template v-if="formData.mode === 'webhook'">
                <div class="form-item">
                  <label class="form-label">Verification Token</label>
                  <t-input v-model="formData.credentials.verification_token" placeholder="Verification Token" />
                </div>
                <div class="form-item">
                  <label class="form-label">Encrypt Key</label>
                  <t-input v-model="formData.credentials.encrypt_key" type="password" placeholder="Encrypt Key" />
                </div>
              </template>
            </template>

            <!-- Slack credentials -->
            <template v-if="formData.platform === 'slack'">
              <div class="platform-link-hint">
                <a href="https://api.slack.com/apps" target="_blank" rel="noopener noreferrer" class="doc-link">
                  {{ $t('agentEditor.im.slackConsole') }}
                  <t-icon name="link" class="link-icon" />
                </a>
                <span class="hint-text">{{ $t('agentEditor.im.consoleTip') }}</span>
              </div>
              <template v-if="formData.mode === 'websocket'">
                <div class="form-item">
                  <label class="form-label">App Token</label>
                  <t-input v-model="formData.credentials.app_token" type="password" placeholder="xapp-..." />
                </div>
                <div class="form-item">
                  <label class="form-label">Bot Token</label>
                  <t-input v-model="formData.credentials.bot_token" type="password" placeholder="xoxb-..." />
                </div>
              </template>
              <template v-else>
                <div class="form-item">
                  <label class="form-label">Bot Token</label>
                  <t-input v-model="formData.credentials.bot_token" type="password" placeholder="xoxb-..." />
                </div>
                <div class="form-item">
                  <label class="form-label">Signing Secret</label>
                  <t-input v-model="formData.credentials.signing_secret" type="password" placeholder="Signing Secret" />
                </div>
              </template>
            </template>

            <!-- Telegram credentials -->
            <template v-if="formData.platform === 'telegram'">
              <div class="platform-link-hint">
                <a href="https://t.me/BotFather" target="_blank" rel="noopener noreferrer" class="doc-link">
                  {{ $t('agentEditor.im.telegramConsole') }}
                  <t-icon name="link" class="link-icon" />
                </a>
                <span class="hint-text">{{ $t('agentEditor.im.consoleTip') }}</span>
              </div>
              <div class="form-item">
                <label class="form-label">Bot Token</label>
                <t-input v-model="formData.credentials.bot_token" type="password" placeholder="123456789:AABBccdd..." />
              </div>
              <template v-if="formData.mode === 'webhook'">
                <div class="form-item">
                  <label class="form-label">Secret Token</label>
                  <t-input v-model="formData.credentials.secret_token" type="password"
                    placeholder="Secret Token (optional)" />
                </div>
              </template>
            </template>

            <!-- DingTalk credentials -->
            <template v-if="formData.platform === 'dingtalk'">
              <div class="platform-link-hint">
                <a href="https://open.dingtalk.com/" target="_blank" rel="noopener noreferrer" class="doc-link">
                  {{ $t('agentEditor.im.dingtalkConsole') }}
                  <t-icon name="link" class="link-icon" />
                </a>
                <span class="hint-text">{{ $t('agentEditor.im.consoleTip') }}</span>
              </div>
              <div class="form-item">
                <label class="form-label">Client ID (AppKey)</label>
                <t-input v-model="formData.credentials.client_id" placeholder="Client ID / AppKey" />
              </div>
              <div class="form-item">
                <label class="form-label">Client Secret (AppSecret)</label>
                <t-input v-model="formData.credentials.client_secret" type="password"
                  placeholder="Client Secret / AppSecret" />
              </div>
              <div class="form-item">
                <label class="form-label">{{ $t('agentEditor.im.dingtalkCardTemplateId') }}</label>
                <t-input v-model="formData.credentials.card_template_id"
                  placeholder="xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx.schema" />
                <p class="form-desc">{{ $t('agentEditor.im.dingtalkCardTemplateIdHint') }}</p>
              </div>
            </template>

            <!-- QQBot credentials -->
            <template v-if="formData.platform === 'qqbot'">
              <div class="platform-link-hint">
                <a href="https://q.qq.com/" target="_blank" rel="noopener noreferrer" class="doc-link">
                  {{ $t('agentEditor.im.qqbotConsole') }}
                  <t-icon name="link" class="link-icon" />
                </a>
                <span class="hint-text">{{ $t('agentEditor.im.consoleTip') }}</span>
              </div>
              <div class="form-item">
                <label class="form-label">App ID</label>
                <t-input v-model="formData.credentials.app_id" placeholder="QQBot App ID" />
              </div>
              <div class="form-item">
                <label class="form-label">App Secret</label>
                <t-input v-model="formData.credentials.client_secret" type="password" placeholder="QQBot App Secret" />
              </div>
              <div class="form-item">
                <label class="form-label">API Base URL</label>
                <t-input v-model="formData.credentials.api_base_url" placeholder="https://api.sgroup.qq.com" />
                <p class="form-desc">{{ $t('agentEditor.im.qqbotAPIBaseURLHint') }}</p>
              </div>
              <div class="form-item">
                <label class="form-label">Gateway URL</label>
                <t-input v-model="formData.credentials.gateway_url" placeholder="wss://api.sgroup.qq.com/websocket/" />
                <p class="form-desc">{{ $t('agentEditor.im.qqbotGatewayURLHint') }}</p>
              </div>
            </template>

            <!-- Mattermost credentials -->
            <template v-if="formData.platform === 'mattermost'">
              <div class="platform-link-hint">
                <a href="https://developers.mattermost.com/integrate/webhooks/outgoing/" target="_blank"
                  rel="noopener noreferrer" class="doc-link">
                  {{ $t('agentEditor.im.mattermostConsole') }}
                  <t-icon name="link" class="link-icon" />
                </a>
                <span class="hint-text">{{ $t('agentEditor.im.consoleTip') }}</span>
              </div>
              <div class="form-item">
                <label class="form-label">Site URL</label>
                <t-input v-model="formData.credentials.site_url" placeholder="https://mattermost.example.com" />
              </div>
              <div class="form-item">
                <label class="form-label">Bot Token</label>
                <t-input v-model="formData.credentials.bot_token" type="password" placeholder="Bot Token" />
              </div>
              <div class="form-item">
                <label class="form-label">Outgoing Webhook Token</label>
                <t-input v-model="formData.credentials.outgoing_token" type="password"
                  placeholder="Token from Outgoing Webhook" />
              </div>
              <div class="form-item">
                <label class="form-label">Bot User ID</label>
                <t-input v-model="formData.credentials.bot_user_id" placeholder="Optional — filter bot self-messages" />
              </div>
              <div class="settings-group">
                <div class="setting-row setting-row--last">
                  <div class="setting-info">
                    <label>{{ $t('agentEditor.im.mattermostPostToMain') }}</label>
                    <p class="desc">{{ $t('agentEditor.im.mattermostPostToMainHint') }}</p>
                  </div>
                  <div class="setting-control">
                    <t-switch :value="!!formData.credentials.post_to_main" size="small"
                      @change="(v: boolean) => { formData.credentials.post_to_main = v }" />
                  </div>
                </div>
              </div>
            </template>

            <!-- Yunzhijia credentials -->
            <template v-if="formData.platform === 'yunzhijia'">
              <div class="form-item">
                <label class="form-label required">{{ $t('agentEditor.im.yunzhijiaSendMsgUrl') }}</label>
                <t-input v-model="formData.credentials.send_msg_url"
                  placeholder="https://www.yunzhijia.com/gateway/robot/webhook/send?yzjtype=0&yzjtoken=..." />
                <p class="form-desc">
                  {{ $t('agentEditor.im.yunzhijiaSendMsgUrlHint') }}
                  <a href="https://www.yunzhijia.com/opendocs/docs.html#/guide/im/robot" target="_blank"
                    rel="noopener noreferrer" class="doc-link">
                    {{ $t('agentEditor.im.yunzhijiaRobotDoc') }}
                  </a>
                </p>
              </div>
              <div class="form-item">
                <label class="form-label">{{ $t('agentEditor.im.yunzhijiaSecret') }}</label>
                <t-input v-model="formData.credentials.secret" type="password"
                  :placeholder="$t('agentEditor.im.yunzhijiaSecretPlaceholder')" />
                <p class="form-desc">{{ $t('agentEditor.im.yunzhijiaSecretHint') }}</p>
              </div>
              <div class="form-item">
                <label class="form-label">{{ $t('agentEditor.im.yunzhijiaAppId') }}</label>
                <t-input v-model="formData.credentials.app_id"
                  :placeholder="$t('agentEditor.im.yunzhijiaAppIdPlaceholder')" />
                <p class="form-desc">
                  {{ $t('agentEditor.im.yunzhijiaAppCredentialHint') }}
                  <a href="https://www.yunzhijia.com/developers/" target="_blank" rel="noopener noreferrer"
                    class="doc-link">
                    {{ $t('agentEditor.im.yunzhijiaImageDoc') }}
                  </a>
                </p>
              </div>
              <div class="form-item">
                <label class="form-label">{{ $t('agentEditor.im.yunzhijiaAppSecret') }}</label>
                <t-input v-model="formData.credentials.app_secret" type="password"
                  :placeholder="$t('agentEditor.im.yunzhijiaAppSecretPlaceholder')" />
              </div>
              <div class="form-item">
                <label class="form-label">{{ $t('agentEditor.im.yunzhijiaTimeout') }}</label>
                <t-input-number v-model="formData.credentials.timeout_seconds" placeholder="10" :min="1" :max="60"
                  style="width: 100%;" />
                <p class="form-desc">{{ $t('agentEditor.im.yunzhijiaTimeoutHint') }}</p>
              </div>
              <div class="form-item">
                <label class="form-label required">{{ $t('agentEditor.im.yunzhijiaAllowedHostSuffix') }}</label>
                <t-input v-model="formData.credentials.allowed_webhook_host_suffix" placeholder="yunzhijia.com" />
                <p class="form-desc">{{ $t('agentEditor.im.yunzhijiaAllowedHostSuffixHint') }}</p>
              </div>
            </template>

            <!-- WeChat credentials (QR code binding) -->
            <template v-if="formData.platform === 'wechat'">
              <p class="form-desc">{{ $t('agentEditor.im.wechatHint') }}</p>

              <!-- Already bound state -->
              <div v-if="wechatBound" class="wechat-bound-status">
                <t-icon name="check-circle-filled" class="bound-icon" />
                <span>{{ $t('agentEditor.im.wechatBindSuccess') }}</span>
                <t-button size="small" variant="outline" theme="default" @click="startWeChatBinding">
                  {{ $t('agentEditor.im.wechatRebind') }}
                </t-button>
              </div>

              <!-- QR code binding flow -->
              <div v-else class="wechat-qr-section">
                <!-- Initial state: show bind button -->
                <div v-if="!wechatQRImgUrl" class="wechat-bind-action">
                  <t-button theme="default" variant="outline" :loading="wechatLoading" @click="startWeChatBinding">
                    <template #icon><t-icon name="scan" /></template>
                    {{ $t('agentEditor.im.wechatScanBind') }}
                  </t-button>
                </div>

                <!-- QR code displayed -->
                <div v-else class="wechat-qr-display">
                  <div class="qr-container">
                    <img :src="wechatQRImgUrl" alt="WeChat QR Code" class="qr-image" />
                    <div v-if="wechatQRStatus === 'expired'" class="qr-expired-overlay" @click="startWeChatBinding">
                      <t-icon name="refresh" class="refresh-icon" />
                      <span>{{ $t('agentEditor.im.wechatQRExpired') }}</span>
                    </div>
                  </div>
                  <p class="qr-hint">
                    <template v-if="wechatQRStatus === 'scaned'">
                      {{ $t('agentEditor.im.wechatBinding') }}
                    </template>
                    <template v-else>
                      {{ $t('agentEditor.im.wechatScanning') }}
                    </template>
                  </p>
                </div>
              </div>
            </template>

            <!-- WhatsApp credentials (QR pairing via WhatsApp Web multidevice protocol) -->
            <template v-if="formData.platform === 'whatsapp'">
              <p class="form-desc">{{ $t('agentEditor.im.whatsappHint') }}</p>
              <p class="form-desc whatsapp-risk-warning">{{ $t('agentEditor.im.whatsappRiskWarning') }}</p>

              <!-- Already paired state, colored by the live runtime status.
                   During re-pairing the existing binding is kept in the form
                   so a failed attempt cannot lose it. -->
              <template v-if="whatsappBound && !whatsappRepairing">
                <div class="wechat-bound-status" :class="`wechat-bound-status--${whatsappDrawerTone}`">
                  <t-icon :name="whatsappDrawerIcon" class="bound-icon" />
                  <span>{{ whatsappDrawerText }}
                    <span class="mono-text">{{ formData.credentials.device_jid }}</span></span>
                  <t-button size="small" :variant="whatsappNeedsRepair ? 'base' : 'outline'"
                    :theme="whatsappNeedsRepair ? 'primary' : 'default'" @click="startWhatsAppBinding">
                    {{ $t('agentEditor.im.whatsappRebind') }}
                  </t-button>
                </div>
                <p v-if="drawerStatus?.detail && whatsappDrawerTone !== 'ok'" class="form-desc whatsapp-status-detail">
                  {{ drawerStatus.detail }}
                </p>
              </template>

              <!-- QR pairing flow -->
              <div v-else class="wechat-qr-section">
                <div v-if="!whatsappQRImg" class="wechat-bind-action">
                  <t-button theme="default" variant="outline" :loading="whatsappLoading" @click="startWhatsAppBinding">
                    <template #icon><t-icon name="scan" /></template>
                    {{ $t('agentEditor.im.whatsappScanBind') }}
                  </t-button>
                </div>
                <div v-else class="wechat-qr-display">
                  <div class="qr-container">
                    <img :src="whatsappQRImg" alt="WhatsApp QR Code" class="qr-image" />
                    <div v-if="whatsappQRStatus === 'expired' || whatsappQRStatus === 'error'"
                      class="qr-expired-overlay" @click="startWhatsAppBinding">
                      <t-icon name="refresh" class="refresh-icon" />
                      <span>{{ $t('agentEditor.im.whatsappQRExpired') }}</span>
                    </div>
                  </div>
                  <p class="qr-hint">{{ $t('agentEditor.im.whatsappScanning') }}</p>
                </div>
                <t-button v-if="whatsappRepairing && formData.credentials.device_jid" size="small" variant="text"
                  theme="default" @click="cancelWhatsAppRepairing">
                  {{ $t('common.cancel') }}
                </t-button>
              </div>

              <div class="form-item">
                <label class="form-label">{{ $t('agentEditor.im.whatsappAllowFrom') }}</label>
                <t-input v-model="formData.credentials.allow_from" placeholder="8613800138000,15551234567" />
                <p class="form-desc">{{ $t('agentEditor.im.whatsappAllowFromHint') }}</p>
              </div>
            </template>
          </div>
        </section>
      </div>
    </SettingDrawer>
  </div>
</template>

<script setup lang="ts">
import { ref, onMounted, watch, onUnmounted, computed } from 'vue';
import { useI18n } from 'vue-i18n';
import { MessagePlugin } from 'tdesign-vue-next';
import { copyWithToast } from '@/utils/clipboard';
import {
  listIMChannels, createIMChannel, updateIMChannel, deleteIMChannel, toggleIMChannel,
  getWeChatQRCode, pollWeChatQRCodeStatus, startWhatsAppPairing, pollWhatsAppPairing,
  getIMChannelStatus, listAllIMChannels, listAgents,
  type IMChannelOverview, type CustomAgent, type IMChannelRuntimeStatus,
} from '@/api/agent';
import { useChatResourcesStore } from '@/stores/chatResources';
import type { IMChannel } from '@/api/agent';
import { useAuthStore } from '@/stores/auth';
import SettingDrawer from '@/components/settings/SettingDrawer.vue';
import IntegrationsAgentFilter from '@/components/IntegrationsAgentFilter.vue';
import wecomLogo from '@/assets/img/im/wecom.svg';
import feishuLogo from '@/assets/img/im/feishu.svg';
import larkLogo from '@/assets/img/im/lark.svg';
import slackLogo from '@/assets/img/im/slack.svg';
import telegramLogo from '@/assets/img/im/telegram.svg';
import dingtalkLogo from '@/assets/img/im/dingtalk.svg';
import mattermostLogo from '@/assets/img/im/mattermost.svg';
import wechatLogo from '@/assets/img/im/wechat.svg';
import qqbotLogo from '@/assets/img/im/qqbot.png';
import yunzhijiaLogo from '@/assets/img/im/yunzhijia.svg';
import whatsappLogo from '@/assets/img/im/whatsapp.svg';

type IMPlatform = IMChannel['platform'];

const PLATFORM_LOGO: Record<string, string> = {
  wecom: wecomLogo,
  feishu: feishuLogo,
  lark: larkLogo,
  slack: slackLogo,
  telegram: telegramLogo,
  dingtalk: dingtalkLogo,
  mattermost: mattermostLogo,
  wechat: wechatLogo,
  qqbot: qqbotLogo,
  yunzhijia: yunzhijiaLogo,
  whatsapp: whatsappLogo,
};

const platformLogo = (platform: string): string => (platform ? PLATFORM_LOGO[platform] || '' : '');

const { t } = useI18n();
const authStore = useAuthStore();

const filterAgentId = defineModel<string>('filterAgentId', { default: '' });

const agents = ref<CustomAgent[]>([]);
const agentOptions = computed(() =>
  agents.value.map((agent) => ({ label: agent.name, value: agent.id })),
);

const allChannels = ref<Array<IMChannel | IMChannelOverview>>([]);
const channels = computed(() => {
  const filter = filterAgentId.value?.trim();
  if (!filter) return allChannels.value;
  return allChannels.value.filter((channel) => channel.agent_id === filter);
});
const loading = ref(false);
const saving = ref(false);
const showCreateDialog = ref(false);
const editingChannel = ref<IMChannel | null>(null);
const editingEnabled = ref(true);
const wizardStep = ref(0);
const channelNameTouched = ref(false);

const stepTitles = computed(() => [
  t('agentEditor.im.stepBasic'),
  t('agentEditor.im.stepConnection'),
  t('agentEditor.im.stepKnowledge'),
  t('agentEditor.im.stepCredentials'),
]);

const drawerStepDescription = computed(() => stepTitles.value[wizardStep.value] ?? '');

const drawerConfirmText = computed(() =>
  wizardStep.value < stepTitles.value.length - 1 ? t('common.next') : t('common.save'),
);

const platformOptions = computed(() => ([
  { value: 'wecom' as IMPlatform, label: t('agentEditor.im.wecom'), logo: wecomLogo },
  { value: 'feishu' as IMPlatform, label: t('agentEditor.im.feishu'), logo: feishuLogo },
  { value: 'lark' as IMPlatform, label: t('agentEditor.im.lark'), logo: larkLogo },
  { value: 'slack' as IMPlatform, label: t('agentEditor.im.slack'), logo: slackLogo },
  { value: 'telegram' as IMPlatform, label: t('agentEditor.im.telegram'), logo: telegramLogo },
  { value: 'dingtalk' as IMPlatform, label: t('agentEditor.im.dingtalk'), logo: dingtalkLogo },
  { value: 'mattermost' as IMPlatform, label: t('agentEditor.im.mattermost'), logo: mattermostLogo },
  { value: 'wechat' as IMPlatform, label: t('agentEditor.im.wechat'), logo: wechatLogo },
  { value: 'qqbot' as IMPlatform, label: t('agentEditor.im.qqbot'), logo: qqbotLogo },
  { value: 'yunzhijia' as IMPlatform, label: t('agentEditor.im.yunzhijia'), logo: yunzhijiaLogo },
  { value: 'whatsapp' as IMPlatform, label: t('agentEditor.im.whatsapp'), logo: whatsappLogo },
]));

// Feishu and Lark are the same product on separate clouds, so each has its own
// open platform console. Bots must be created on the one matching the channel.
const openPlatformConsole = computed(() =>
  formData.value.platform === 'lark'
    ? { url: 'https://open.larksuite.com/', labelKey: 'agentEditor.im.larkConsole' }
    : { url: 'https://open.feishu.cn/', labelKey: 'agentEditor.im.feishuConsole' },
);

const drawerTitle = computed(() => {
  if (editingChannel.value) {
    return formData.value.name?.trim() || t('agentEditor.im.unnamed');
  }
  return t('agentEditor.im.addChannel');
});

function validateWizardStep(step: number): boolean {
  if (step === 0 && !formData.value.target_agent_id) {
    MessagePlugin.warning(t('integrations.selectAgentHint'));
    return false;
  }
  return true;
}

function prevWizardStep() {
  if (wizardStep.value > 0) wizardStep.value -= 1;
}

async function handleDrawerConfirm() {
  if (wizardStep.value < stepTitles.value.length - 1) {
    if (!validateWizardStep(wizardStep.value)) return;
    wizardStep.value += 1;
    return;
  }
  await handleSave();
}

// Knowledge base options for file-to-KB feature
const knowledgeBases = ref<{ id: string; name: string }[]>([]);

// ── Channel runtime status ──
// Live health per channel, fetched only for platforms whose adapter reports
// connection state (currently WhatsApp). The card list refreshes on load and
// periodically; the drawer polls faster while a paired channel is open so a
// dead pairing shows up as "re-pair required" instead of a stale green tick.
const STATUS_PLATFORMS = new Set<string>(['whatsapp']);
const CARD_STATUS_REFRESH_MS = 30000;
const DRAWER_STATUS_REFRESH_MS = 15000;

const channelStatuses = ref<Record<string, IMChannelRuntimeStatus>>({});
const drawerStatus = ref<IMChannelRuntimeStatus | null>(null);
let cardStatusTimer: ReturnType<typeof setInterval> | null = null;
let drawerStatusTimer: ReturnType<typeof setInterval> | null = null;

type StatusTone = 'ok' | 'warn' | 'error' | 'muted';

function statusTone(state?: string): StatusTone {
  switch (state) {
    case 'connected':
    case 'running':
      return 'ok';
    case 'connecting':
      return 'warn';
    case 'disabled':
    case 'not_running':
    case undefined:
      return 'muted';
    default:
      return 'error';
  }
}

function statusText(state?: string): string {
  switch (state) {
    case 'connected':
    case 'running':
      return t('agentEditor.im.statusConnected');
    case 'connecting':
      return t('agentEditor.im.statusConnecting');
    case 'logged_out':
      return t('agentEditor.im.statusLoggedOut');
    case 'needs_pairing':
      return t('agentEditor.im.statusNeedsPairing');
    case 'stream_replaced':
      return t('agentEditor.im.statusStreamReplaced');
    case 'not_running':
      return t('agentEditor.im.statusNotRunning');
    case 'disabled':
      return t('agentEditor.im.disabled');
    default:
      return t('agentEditor.im.statusError');
  }
}

// Status shown on a channel card; disabled channels already carry a tag.
function cardStatus(channel: { id: string; enabled?: boolean }): IMChannelRuntimeStatus | null {
  if (channel.enabled === false) return null;
  return channelStatuses.value[channel.id] || null;
}

async function refreshChannelStatuses() {
  const targets = allChannels.value.filter(
    (ch) => STATUS_PLATFORMS.has(ch.platform) && ch.enabled !== false,
  );
  if (targets.length === 0) {
    channelStatuses.value = {};
    return;
  }
  const results = await Promise.allSettled(
    targets.map(async (ch) => ({ id: ch.id, status: (await getIMChannelStatus(ch.id)).data })),
  );
  const next: Record<string, IMChannelRuntimeStatus> = {};
  for (const r of results) {
    if (r.status === 'fulfilled' && r.value.status) next[r.value.id] = r.value.status;
  }
  channelStatuses.value = next;
}

function startDrawerStatusPolling(channelId: string) {
  stopDrawerStatusPolling();
  const fetchOnce = async () => {
    try {
      const res = await getIMChannelStatus(channelId);
      drawerStatus.value = res.data || null;
    } catch {
      // keep the last snapshot; transient fetch errors must not flip the badge
    }
  };
  void fetchOnce();
  drawerStatusTimer = setInterval(fetchOnce, DRAWER_STATUS_REFRESH_MS);
}

function stopDrawerStatusPolling() {
  if (drawerStatusTimer) {
    clearInterval(drawerStatusTimer);
    drawerStatusTimer = null;
  }
  drawerStatus.value = null;
}

// Drawer badge for an already-paired WhatsApp channel. Until the first status
// response arrives (or for a freshly paired, unsaved channel) fall back to the
// plain "paired" look rather than flashing an error state.
const whatsappDrawerTone = computed<StatusTone>(() =>
  drawerStatus.value ? statusTone(drawerStatus.value.state) : 'ok',
);
const whatsappDrawerText = computed(() =>
  drawerStatus.value ? statusText(drawerStatus.value.state) : t('agentEditor.im.whatsappBindSuccess'),
);
const whatsappNeedsRepair = computed(() =>
  drawerStatus.value ? ['logged_out', 'needs_pairing'].includes(drawerStatus.value.state) : false,
);
const whatsappDrawerIcon = computed(() => {
  switch (whatsappDrawerTone.value) {
    case 'ok':
      return 'check-circle-filled';
    case 'warn':
      return 'time-filled';
    case 'muted':
      return 'minus-circle-filled';
    default:
      return 'error-circle-filled';
  }
});

// WeChat QR code binding state
const wechatQRContent = ref('');  // raw text to encode as QR code
const wechatQRImgUrl = ref('');   // generated QR image URL
const wechatQRCode = ref('');     // opaque token for polling status
const wechatQRStatus = ref<string>('');
const wechatLoading = ref(false);
let wechatPollActive = false;
let wechatPollTimer: ReturnType<typeof setTimeout> | null = null;

// WhatsApp QR pairing state
const whatsappQRImg = ref('');      // locally rendered PNG data URL from backend
const whatsappSessionId = ref('');  // pairing session identifier for polling
const whatsappQRStatus = ref<string>('');
const whatsappLoading = ref(false);
// True while re-pairing an already-bound channel: the QR flow is shown but the
// existing device_jid stays in the form until the new pairing succeeds.
const whatsappRepairing = ref(false);
let whatsappPollActive = false;
let whatsappPollTimer: ReturnType<typeof setTimeout> | null = null;
let whatsappPollFailures = 0;
// Consecutive poll failures tolerated before giving up (~20s at 1s cadence):
// the session may be gone for good (5-minute pairing timeout, backend
// restart), and polling a dead session forever shows a stale QR code.
const WHATSAPP_POLL_MAX_FAILURES = 20;

const defaultCredentials = (): Record<string, any> => ({});

const formData = ref({
  target_agent_id: '',
  platform: 'wecom' as IMPlatform,
  name: '',
  mode: 'websocket' as 'webhook' | 'websocket' | 'longpoll',
  output_mode: 'stream' as 'stream' | 'full',
  session_mode: 'user' as 'user' | 'thread',
  knowledge_base_id: '',
  credentials: defaultCredentials(),
});

const channelMenuOptions = (channel: IMChannel | IMChannelOverview) => ([
  {
    content: channel.enabled ? t('common.off') : t('common.on'),
    value: 'toggle',
  },
]);

function handleChannelMenuClick(
  data: { value?: string },
  channel: IMChannel | IMChannelOverview,
) {
  if (data.value === 'toggle') {
    void handleToggle(channel);
  }
}

function agentDisplayName(channel: IMChannel | IMChannelOverview): string {
  return agentForChannel(channel)?.name || '';
}

function agentForChannel(channel: IMChannel | IMChannelOverview): CustomAgent | undefined {
  const found = agents.value.find((agent) => agent.id === channel.agent_id);
  if (found) return found;
  const overviewName = (channel as IMChannelOverview).agent_name;
  if (!overviewName) return undefined;
  return {
    id: channel.agent_id,
    name: overviewName,
    is_builtin: false,
    config: {},
  };
}

function platformLabel(platform: string): string {
  const key = `agentEditor.im.${platform}`;
  return t(key);
}

function defaultChannelName(platform: string = formData.value.platform): string {
  return platformLabel(platform);
}

function resolvedChannelName(): string {
  return formData.value.name.trim() || defaultChannelName();
}

function platformSupportsThread(platform: string): boolean {
  return ['slack', 'mattermost', 'feishu', 'lark', 'telegram', 'yunzhijia'].includes(platform);
}

watch(
  () => formData.value.platform,
  (p) => {
    if (p === 'mattermost') {
      formData.value.mode = 'webhook';
      if (typeof formData.value.credentials.post_to_main !== 'boolean') {
        formData.value.credentials.post_to_main = false;
      }
    }
    if (!platformSupportsThread(p)) {
      formData.value.session_mode = 'user';
    }
  },
);
// Whether WeChat credentials are already bound
const wechatBound = computed(() => {
  return formData.value.platform === 'wechat' &&
    formData.value.credentials.bot_token &&
    formData.value.credentials.ilink_bot_id;
});

// Whether a WhatsApp device is already paired
const whatsappBound = computed(() => {
  return formData.value.platform === 'whatsapp' && !!formData.value.credentials.device_jid;
});


function onPlatformChange(val: string | number | boolean) {
  if (editingChannel.value) return;
  formData.value.credentials = defaultCredentials();
  stopWeChatPolling();
  wechatQRContent.value = '';
  wechatQRImgUrl.value = '';
  wechatQRCode.value = '';
  wechatQRStatus.value = '';
  stopWhatsAppPolling();
  whatsappQRImg.value = '';
  whatsappSessionId.value = '';
  whatsappQRStatus.value = '';
  // WeChat uses fixed mode/output
  if (val === 'wechat') {
    formData.value.mode = 'longpoll';
    formData.value.output_mode = 'full';
  } else if (val === 'whatsapp') {
    // Connection-based socket; no streaming (message edits are conspicuous
    // automation on an unofficial client).
    formData.value.mode = 'websocket';
    formData.value.output_mode = 'full';
  } else if (val === 'mattermost' || val === 'yunzhijia') {
    formData.value.mode = 'webhook';
    formData.value.output_mode = 'stream';
    if (val === 'yunzhijia') {
      formData.value.credentials = {
        timeout_seconds: 10,
        allowed_webhook_host_suffix: 'yunzhijia.com',
      };
    }
  } else {
    formData.value.mode = 'websocket';
    formData.value.output_mode = 'stream';
  }
  if (!channelNameTouched.value) {
    formData.value.name = defaultChannelName(String(val));
  }
}

function normalizeYunzhijiaCredentials() {
  if (formData.value.platform !== 'yunzhijia') return;
  if (!formData.value.credentials.allowed_webhook_host_suffix) {
    formData.value.credentials.allowed_webhook_host_suffix = 'yunzhijia.com';
  }
  if (!formData.value.credentials.timeout_seconds) {
    formData.value.credentials.timeout_seconds = 10;
  }
}

async function startWeChatBinding() {
  stopWeChatPolling();
  wechatLoading.value = true;
  wechatQRContent.value = '';
  wechatQRImgUrl.value = '';
  wechatQRStatus.value = '';

  try {
    const res = await getWeChatQRCode();
    // qrcode_url is the text content to encode as QR code (e.g. a weixin:// URL)
    wechatQRContent.value = res.data.qrcode_url;
    wechatQRCode.value = res.data.qrcode;
    wechatQRStatus.value = 'wait';

    // Generate QR code image via public API (no extra npm dependency needed)
    wechatQRImgUrl.value = `https://api.qrserver.com/v1/create-qr-code/?size=200x200&data=${encodeURIComponent(res.data.qrcode_url)}`;

    // Start long-polling for scan status
    startStatusPolling();
  } catch (e: any) {
    MessagePlugin.error(e?.message || 'Failed to generate QR code');
  } finally {
    wechatLoading.value = false;
  }
}

function startStatusPolling() {
  wechatPollActive = true;
  pollOnce();
}

async function pollOnce() {
  if (!wechatPollActive) return;
  try {
    const statusRes = await pollWeChatQRCodeStatus(wechatQRCode.value);
    if (!wechatPollActive) return;
    wechatQRStatus.value = statusRes.data.status;

    if (statusRes.data.status === 'confirmed' && statusRes.data.credentials) {
      formData.value.credentials = {
        bot_token: statusRes.data.credentials.bot_token,
        ilink_bot_id: statusRes.data.credentials.ilink_bot_id,
        ilink_user_id: statusRes.data.credentials.ilink_user_id,
      };
      stopWeChatPolling();
      wechatQRContent.value = '';
      wechatQRImgUrl.value = '';
      MessagePlugin.success(t('agentEditor.im.wechatBindSuccess'));
      return;
    }
    if (statusRes.data.status === 'expired') {
      stopWeChatPolling();
      return;
    }
  } catch {
    // transient error
  }
  // Schedule next poll with a short delay (the backend already long-polled ~35s)
  if (wechatPollActive) {
    wechatPollTimer = setTimeout(pollOnce, 500);
  }
}

function stopWeChatPolling() {
  wechatPollActive = false;
  if (wechatPollTimer) {
    clearTimeout(wechatPollTimer);
    wechatPollTimer = null;
  }
}

async function startWhatsAppBinding() {
  stopWhatsAppPolling();
  whatsappLoading.value = true;
  whatsappQRImg.value = '';
  whatsappQRStatus.value = '';
  // Re-pairing keeps the existing device_jid in the form: if this attempt
  // fails, the admin can cancel (or just save) without losing the binding.
  whatsappRepairing.value = !!formData.value.credentials.device_jid;

  try {
    const res = await startWhatsAppPairing();
    whatsappSessionId.value = res.data.session_id;
    whatsappQRStatus.value = res.data.status;
    whatsappPollFailures = 0;
    // qr_png is rendered by the backend: the QR payload is the pairing secret
    // and must never leave for a third-party QR image service.
    whatsappQRImg.value = res.data.qr_png || '';
    if (res.data.status === 'wait') {
      whatsappPollActive = true;
      pollWhatsAppOnce(res.data.session_id);
    } else if (res.data.error) {
      MessagePlugin.error(res.data.error);
    }
  } catch (e: any) {
    MessagePlugin.error(e?.message || 'Failed to start WhatsApp pairing');
    whatsappRepairing.value = false;
  } finally {
    whatsappLoading.value = false;
  }
}

async function pollWhatsAppOnce(sessionId: string) {
  // A late response from a superseded session must not touch current state
  // (it could kill the new session's loop or leave two loops running).
  const stale = () => !whatsappPollActive || sessionId !== whatsappSessionId.value;
  if (stale()) return;
  try {
    const res = await pollWhatsAppPairing(sessionId);
    if (stale()) return;
    whatsappPollFailures = 0;
    whatsappQRStatus.value = res.data.status;
    // The QR code rotates every ~20-60s; always show the latest one
    if (res.data.qr_png) {
      whatsappQRImg.value = res.data.qr_png;
    }

    if (res.data.status === 'success' && res.data.credentials) {
      formData.value.credentials = {
        ...formData.value.credentials,
        device_jid: res.data.credentials.device_jid,
      };
      whatsappRepairing.value = false;
      stopWhatsAppPolling();
      whatsappQRImg.value = '';
      MessagePlugin.success(t('agentEditor.im.whatsappBindSuccess'));
      return;
    }
    if (res.data.status === 'expired' || res.data.status === 'error') {
      stopWhatsAppPolling();
      return;
    }
  } catch {
    if (stale()) return;
    // Tolerate transient errors, but give up after a run of failures and show
    // the expired overlay instead of polling a dead session forever.
    whatsappPollFailures += 1;
    if (whatsappPollFailures >= WHATSAPP_POLL_MAX_FAILURES) {
      whatsappQRStatus.value = 'error';
      stopWhatsAppPolling();
      return;
    }
  }
  if (!stale()) {
    whatsappPollTimer = setTimeout(() => pollWhatsAppOnce(sessionId), 1000);
  }
}

// Abandon an in-progress re-pairing and fall back to the existing binding.
function cancelWhatsAppRepairing() {
  stopWhatsAppPolling();
  whatsappRepairing.value = false;
  whatsappQRImg.value = '';
  whatsappSessionId.value = '';
  whatsappQRStatus.value = '';
}

function stopWhatsAppPolling() {
  whatsappPollActive = false;
  whatsappPollFailures = 0;
  if (whatsappPollTimer) {
    clearTimeout(whatsappPollTimer);
    whatsappPollTimer = null;
  }
}

async function loadChannels() {
  loading.value = true;
  try {
    const chatResources = useChatResourcesStore();
    const [channelRes, agentRes] = await Promise.all([
      listAllIMChannels(),
      listAgents(),
      chatResources.ensureKnowledgeBases(),
    ]);
    allChannels.value = channelRes.data || [];
    agents.value = agentRes?.data || [];
    knowledgeBases.value = chatResources.rawKnowledgeBases.map((kb: any) => ({ id: kb.id, name: kb.name }));
    void refreshChannelStatuses();
  } catch {
    allChannels.value = [];
  } finally {
    loading.value = false;
  }
}

function getCallbackUrl(channel: IMChannel): string {
  const base = window.location.origin;
  return `${base}/api/v1/im/callback/${channel.id}`;
}

async function copyUrl(channel: IMChannel) {
  await copyWithToast(getCallbackUrl(channel), 'common.copySuccess');
}

function openCreate() {
  resetForm();
  if (filterAgentId.value) {
    formData.value.target_agent_id = filterAgentId.value;
  }
  showCreateDialog.value = true;
}

function openDrawer(channel: IMChannel | IMChannelOverview) {
  void editChannel(channel);
}

async function editChannel(channel: IMChannel | IMChannelOverview) {
  wizardStep.value = 0;
  let fullChannel: IMChannel | null = null;
  if (!('credentials' in channel)) {
    try {
      const res = await listIMChannels(channel.agent_id);
      fullChannel = (res.data || []).find((item) => item.id === channel.id) || null;
    } catch {
      fullChannel = null;
    }
  } else {
    fullChannel = channel as IMChannel;
  }
  if (!fullChannel) {
    MessagePlugin.error(t('common.operationFailed'));
    return;
  }
  editingChannel.value = fullChannel;
  editingEnabled.value = fullChannel.enabled;
  channelNameTouched.value = true;
  formData.value = {
    target_agent_id: fullChannel.agent_id,
    platform: fullChannel.platform,
    name: fullChannel.name,
    mode: fullChannel.mode,
    output_mode: fullChannel.output_mode,
    session_mode: fullChannel.session_mode || 'user',
    knowledge_base_id: fullChannel.knowledge_base_id || '',
    credentials: { ...fullChannel.credentials },
  };
  normalizeYunzhijiaCredentials();
  if (STATUS_PLATFORMS.has(fullChannel.platform)) {
    startDrawerStatusPolling(fullChannel.id);
  }
  showCreateDialog.value = true;
}

function resetForm() {
  editingChannel.value = null;
  editingEnabled.value = true;
  wizardStep.value = 0;
  channelNameTouched.value = false;
  stopDrawerStatusPolling();
  stopWeChatPolling();
  wechatQRContent.value = '';
  wechatQRImgUrl.value = '';
  wechatQRCode.value = '';
  wechatQRStatus.value = '';
  stopWhatsAppPolling();
  whatsappQRImg.value = '';
  whatsappSessionId.value = '';
  whatsappQRStatus.value = '';
  whatsappRepairing.value = false;
  formData.value = {
    target_agent_id: filterAgentId.value || '',
    platform: 'wecom',
    name: defaultChannelName('wecom'),
    mode: 'websocket',
    output_mode: 'stream',
    session_mode: 'user',
    knowledge_base_id: '',
    credentials: defaultCredentials(),
  };
}

async function handleSave() {
  saving.value = true;
  try {
    // For WeChat, validate that credentials are bound
    if (formData.value.platform === 'wechat' && !formData.value.credentials.bot_token) {
      MessagePlugin.warning(t('agentEditor.im.wechatScanBind'));
      return;
    }
    // For WhatsApp, a device must be paired before saving
    if (formData.value.platform === 'whatsapp' && !formData.value.credentials.device_jid) {
      MessagePlugin.warning(t('agentEditor.im.whatsappScanBind'));
      return;
    }
    if (formData.value.platform === 'yunzhijia') {
      // normalize fills in the default allowed host suffix, so only the send URL
      // needs explicit validation here.
      normalizeYunzhijiaCredentials();
      if (!String(formData.value.credentials.send_msg_url || '').trim()) {
        MessagePlugin.warning(t('agentEditor.im.yunzhijiaSendMsgUrlRequired'));
        return;
      }
    }

    if (editingChannel.value) {
      await updateIMChannel(editingChannel.value.id, {
        name: resolvedChannelName(),
        mode: formData.value.mode,
        output_mode: formData.value.output_mode,
        session_mode: formData.value.session_mode,
        knowledge_base_id: formData.value.knowledge_base_id,
        credentials: formData.value.credentials,
        enabled: editingEnabled.value,
        ...(formData.value.target_agent_id ? { agent_id: formData.value.target_agent_id } : {}),
      });
      MessagePlugin.success(t('common.updateSuccess'));
    } else {
      const targetAgentId = formData.value.target_agent_id;
      if (!targetAgentId) {
        MessagePlugin.warning(t('integrations.selectAgentHint'));
        return;
      }
      await createIMChannel(targetAgentId, {
        platform: formData.value.platform,
        name: resolvedChannelName(),
        mode: formData.value.mode,
        output_mode: formData.value.output_mode,
        session_mode: formData.value.session_mode,
        knowledge_base_id: formData.value.knowledge_base_id,
        credentials: formData.value.credentials,
      });
      MessagePlugin.success(t('common.createSuccess'));
    }
    showCreateDialog.value = false;
    resetForm();
    await loadChannels();
  } catch (e: any) {
    const msg = e?.message || (typeof e?.error === 'string' ? e.error : null) || t('common.operationFailed');
    MessagePlugin.error(msg);
  } finally {
    saving.value = false;
  }
}

async function handleToggle(channel: IMChannel | IMChannelOverview) {
  try {
    await toggleIMChannel(channel.id);
    await loadChannels();
  } catch (e: any) {
    MessagePlugin.error(e?.message || t('common.operationFailed'));
  }
}

async function handleDelete(id: string) {
  try {
    await deleteIMChannel(id);
    MessagePlugin.success(t('common.deleteSuccess'));
    await loadChannels();
  } catch (e: any) {
    MessagePlugin.error(e?.message || t('common.operationFailed'));
  }
}

onMounted(() => {
  loadChannels();
  cardStatusTimer = setInterval(() => {
    void refreshChannelStatuses();
  }, CARD_STATUS_REFRESH_MS);
});

watch(filterAgentId, (id) => {
  if (!showCreateDialog.value && !editingChannel.value && id) {
    formData.value.target_agent_id = id;
  }
});

// Closing the drawer via X / overlay bypasses the footer's cancel handler;
// clean up here so QR polling loops don't keep running against an abandoned
// pairing session in the background. resetForm is idempotent, so the save
// and cancel paths calling it first is harmless.
watch(showCreateDialog, (visible) => {
  if (!visible) {
    resetForm();
  }
});

onUnmounted(() => {
  stopWeChatPolling();
  stopWhatsAppPolling();
  stopDrawerStatusPolling();
  if (cardStatusTimer) {
    clearInterval(cardStatusTimer);
    cardStatusTimer = null;
  }
});
</script>

<style scoped lang="less">
@import './css/channel-panel-list.less';

.im-panel {
  display: flex;
  flex-direction: column;
}

.drawer-platform-icon {
  width: 16px;
  height: 16px;
  object-fit: contain;
}

.im-steps {
  display: flex;
  gap: 8px;
  margin-bottom: 16px;
  border-bottom: 1px solid var(--td-component-stroke);
  padding-bottom: 12px;
}

.im-step {
  display: flex;
  align-items: center;
  gap: 6px;
  flex: 1;
  min-width: 0;
  font-size: 12px;
  color: var(--td-text-color-placeholder);
}

.im-step-title {
  min-width: 0;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.im-step.active {
  color: var(--td-brand-color);
  font-weight: 500;
}

.im-step.done {
  color: var(--td-text-color-secondary);
  font-weight: 500;
}

.im-step-num {
  flex-shrink: 0;
  width: 20px;
  height: 20px;
  border-radius: 50%;
  display: flex;
  align-items: center;
  justify-content: center;
  font-size: 11px;
  font-weight: 600;
  border: 1px solid var(--td-component-stroke);
  color: var(--td-text-color-placeholder);
  background: transparent;
}

.im-step.active .im-step-num {
  background: var(--td-brand-color);
  color: #fff;
  border-color: var(--td-brand-color);
}

.im-step.done .im-step-num {
  background: var(--td-bg-color-secondarycontainer);
  color: var(--td-brand-color);
  border-color: var(--td-component-stroke);
}

.im-step-check {
  font-size: 12px;
}

.im-step-body {
  display: flex;
  flex-direction: column;
  gap: 0;
}

:deep(.im-drawer__section.setting-drawer__section) {
  gap: 10px;
  padding: 10px 0 14px;
}

.callback-url-control {
  display: flex;
  align-items: center;
  gap: 4px;
}

.callback-url-input {
  flex: 1;
  min-width: 0;
}

.mono-text-input :deep(input) {
  font-family: var(--app-font-family-mono, ui-monospace, SFMono-Regular, Menlo, monospace);
  font-size: 12px;
}

.drawer-form {
  display: flex;
  flex-direction: column;
  gap: 12px;
}

.form-item {
  margin-bottom: 0;
}

.form-label {
  display: block;
  margin-bottom: 6px;
  font-size: 13px;
  font-weight: 500;
  color: var(--td-text-color-primary);
  line-height: 1.4;

  &--inline {
    margin-bottom: 0;
  }

  &.required::after {
    content: '*';
    color: var(--td-error-color);
    margin-left: 4px;
  }
}

.im-platform-select-prefix {
  width: 16px;
  height: 16px;
  object-fit: contain;
}

.im-platform-select-option {
  display: flex;
  align-items: center;
  gap: 8px;

  &__icon {
    width: 16px;
    height: 16px;
    object-fit: contain;
    flex-shrink: 0;
  }
}

.form-desc {
  margin: 4px 0 0;
  font-size: 12px;
  line-height: 1.45;
  color: var(--td-text-color-placeholder);

  .doc-link {
    margin-left: 4px;
    color: var(--td-brand-color);
  }
}

.option-chips {
  display: inline-flex;
  flex-wrap: wrap;
  gap: 4px;
  padding: 3px;
  border-radius: 8px;
  background: var(--td-bg-color-secondarycontainer);
}

.option-chip {
  border: none;
  background: transparent;
  color: var(--td-text-color-secondary);
  font: inherit;
  font-size: 12px;
  line-height: 1.3;
  padding: 5px 10px;
  border-radius: 6px;
  cursor: pointer;
  transition: background 0.15s ease, color 0.15s ease, box-shadow 0.15s ease;
  white-space: nowrap;

  &:hover:not(:disabled) {
    color: var(--td-text-color-primary);
  }

  &:disabled {
    opacity: 0.45;
    cursor: not-allowed;
  }

  &--active {
    background: var(--td-bg-color-container);
    color: var(--td-brand-color);
    font-weight: 500;
    box-shadow: 0 1px 2px rgba(15, 23, 42, 0.08);
  }
}

.settings-group {
  display: flex;
  flex-direction: column;
  gap: 0;
}

.setting-row {
  display: flex;
  align-items: flex-start;
  justify-content: space-between;
  gap: 12px;
  padding: 12px 0;
  border-bottom: 1px solid var(--td-component-stroke);

  &--last {
    border-bottom: none;
    padding-bottom: 0;
  }
}

.setting-info {
  flex: 1;
  min-width: 0;
  max-width: 72%;
  padding-right: 8px;

  label {
    display: block;
    margin: 0 0 4px;
    font-size: 13px;
    font-weight: 500;
    color: var(--td-text-color-primary);
    line-height: 1.4;
  }

  .desc {
    margin: 0;
    font-size: 12px;
    line-height: 1.45;
    color: var(--td-text-color-placeholder);
  }
}

.setting-control {
  flex-shrink: 0;
  padding-top: 2px;
}

.platform-link-hint {
  display: flex;
  align-items: center;
  flex-wrap: wrap;
  gap: 6px;
  font-size: 12px;
  line-height: 1.4;
  color: var(--td-text-color-placeholder);

  .doc-link {
    white-space: nowrap;
  }

  .hint-text {
    color: var(--td-text-color-placeholder);
  }
}

// --- WhatsApp ---
.whatsapp-risk-warning {
  padding: 8px 12px;
  background: var(--td-warning-color-1);
  border: 1px solid var(--td-warning-color-3);
  border-radius: 6px;
  color: var(--td-warning-color-7);
}

.mono-text {
  font-family: var(--td-font-family-medium, monospace);
  font-size: 12px;
  word-break: break-all;
}

// --- WeChat QR code binding ---
.wechat-bound-status {
  display: flex;
  align-items: center;
  gap: 8px;
  padding: 12px 16px;
  background: rgba(7, 193, 96, 0.06);
  border: 1px solid rgba(7, 193, 96, 0.2);
  border-radius: 8px;
  font-size: 14px;
  color: var(--td-text-color-primary);

  .bound-icon {
    font-size: 18px;
    color: #07c160;
  }

  // Tone variants driven by the live channel status. The default (ok) look
  // above stays green for backward compatibility with the WeChat block.
  &--warn {
    background: rgba(230, 162, 60, 0.08);
    border-color: rgba(230, 162, 60, 0.3);

    .bound-icon {
      color: var(--td-warning-color, #e6a23c);
    }
  }

  &--error {
    background: rgba(213, 73, 65, 0.06);
    border-color: rgba(213, 73, 65, 0.28);

    .bound-icon {
      color: var(--td-error-color, #d54941);
    }
  }

  &--muted {
    background: var(--td-bg-color-secondarycontainer);
    border-color: var(--td-component-border);

    .bound-icon {
      color: var(--td-text-color-placeholder);
    }
  }
}

.whatsapp-status-detail {
  margin-top: 6px;
  color: var(--td-error-color, #d54941);
}

.channel-card__status {
  display: inline-flex;
  align-items: center;
  gap: 5px;
  flex-shrink: 0;
  font-size: 12px;
  line-height: 1;
  color: var(--td-text-color-secondary);

  &-dot {
    width: 7px;
    height: 7px;
    border-radius: 50%;
    background: currentColor;
  }

  &--ok {
    color: #07c160;
  }

  &--warn {
    color: var(--td-warning-color, #e6a23c);
  }

  &--error {
    color: var(--td-error-color, #d54941);
  }

  &--muted {
    color: var(--td-text-color-placeholder);
  }
}

.wechat-qr-section {
  display: flex;
  flex-direction: column;
  align-items: center;
  gap: 12px;
}

.wechat-bind-action {
  padding: 24px 0;
}

.wechat-qr-display {
  display: flex;
  flex-direction: column;
  align-items: center;
  gap: 12px;
  padding: 16px 0;
}

.qr-container {
  position: relative;
  width: 200px;
  height: 200px;
  border: 1px solid var(--td-component-stroke);
  border-radius: 8px;
  overflow: hidden;
  // QR code images are always black-on-white; force white background
  // so the code remains scannable in dark mode.
  background: #fff;

  .qr-image {
    width: 100%;
    height: 100%;
    object-fit: contain;
  }
}

.qr-expired-overlay {
  position: absolute;
  top: 0;
  left: 0;
  right: 0;
  bottom: 0;
  display: flex;
  flex-direction: column;
  align-items: center;
  justify-content: center;
  gap: 8px;
  background: rgba(0, 0, 0, 0.6);
  color: #fff;
  cursor: pointer;
  font-size: 12px;

  .refresh-icon {
    font-size: 24px;
  }
}

.qr-hint {
  font-size: 13px;
  color: var(--td-text-color-secondary);
  text-align: center;
}
</style>

<style lang="less">
.im-channel-drawer .setting-drawer__header-icon:has(.drawer-platform-icon) {
  background: var(--td-bg-color-container, #fff);
  box-shadow: inset 0 0 0 1px var(--td-component-stroke);
}

.im-channel-drawer .drawer-platform-icon {
  display: block;
  width: 20px;
  height: 20px;
  object-fit: contain;
}
</style>
