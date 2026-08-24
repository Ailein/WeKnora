import type { IMHandoffConfig } from '@/api/agent/index'

/** 表单里可编辑的转人工触发器字段（关键词以文本框逗号分隔编辑）。 */
export interface HandoffForm {
  enabled: boolean
  keywordsText: string
  fallbackThreshold: number
  autoReply: string
  timeoutMinutes: number
  webhookUrl: string
  webhookFormat: NonNullable<IMHandoffConfig['webhook_format']>
}

const MAX_KEYWORDS = 20
const MAX_KEYWORD_CHARS = 50

/**
 * 把关键词文本框内容解析为列表：支持中英文逗号/分号与换行分隔，
 * 去空白、按（不区分大小写）去重，超长条目丢弃，最多保留 20 条。
 */
export function parseHandoffKeywords(text: string): string[] {
  const seen = new Set<string>()
  const keywords: string[] = []
  for (const part of text.split(/[,，;；\n]/)) {
    const kw = part.trim()
    if (!kw || kw.length > MAX_KEYWORD_CHARS) continue
    const key = kw.toLowerCase()
    if (seen.has(key)) continue
    seen.add(key)
    keywords.push(kw)
    if (keywords.length === MAX_KEYWORDS) break
  }
  return keywords
}

/** 关键词列表还原为文本框内容。 */
export function formatHandoffKeywords(keywords: string[] | undefined): string {
  return (keywords ?? []).join('，')
}

/** 新渠道的空白触发器表单（默认关闭，60 分钟接管窗口）。 */
export function defaultHandoffForm(): HandoffForm {
  return {
    enabled: false,
    keywordsText: '',
    fallbackThreshold: 0,
    autoReply: '',
    timeoutMinutes: 60,
    webhookUrl: '',
    webhookFormat: 'generic',
  }
}

/** 已保存配置 → 表单；缺失字段回落到默认值。 */
export function handoffFormFromConfig(cfg: IMHandoffConfig | undefined): HandoffForm {
  const form = defaultHandoffForm()
  if (!cfg) return form
  form.enabled = !!cfg.enabled
  form.keywordsText = formatHandoffKeywords(cfg.keywords)
  form.fallbackThreshold = cfg.fallback_threshold ?? 0
  form.autoReply = cfg.auto_reply ?? ''
  form.timeoutMinutes = cfg.timeout_minutes || 60
  form.webhookUrl = cfg.webhook_url ?? ''
  form.webhookFormat = cfg.webhook_format ?? 'generic'
  return form
}

/** 表单 → 保存到后端的配置对象。 */
export function handoffConfigFromForm(form: HandoffForm): IMHandoffConfig {
  return {
    enabled: form.enabled,
    keywords: parseHandoffKeywords(form.keywordsText),
    fallback_threshold: Math.max(0, Math.round(form.fallbackThreshold || 0)),
    auto_reply: form.autoReply.trim(),
    timeout_minutes: Math.max(0, Math.round(form.timeoutMinutes || 0)),
    webhook_url: form.webhookUrl.trim(),
    webhook_format: form.webhookFormat,
  }
}

/**
 * 保存前的表单校验，返回 i18n key；null 表示通过。
 * 开启时必须至少配置一种触发方式；webhook 地址必须是 http(s)。
 */
export function validateHandoffForm(form: HandoffForm): string | null {
  if (!form.enabled) return null
  if (parseHandoffKeywords(form.keywordsText).length === 0 && (form.fallbackThreshold || 0) <= 0) {
    return 'agentEditor.im.handoff.needTrigger'
  }
  const webhook = form.webhookUrl.trim()
  if (webhook && !/^https?:\/\/.+/i.test(webhook)) {
    return 'agentEditor.im.handoff.badWebhook'
  }
  return null
}
