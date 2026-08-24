import assert from 'node:assert/strict'
import test from 'node:test'

import {
  defaultHandoffForm,
  formatHandoffKeywords,
  handoffConfigFromForm,
  handoffFormFromConfig,
  parseHandoffKeywords,
  validateHandoffForm,
} from './imHandoff.ts'

/**
 * 验证关键词解析支持中英文逗号/分号与换行分隔，去空白去重（不区分大小写），
 * 并在 20 条处截断。传入混合分隔与重复项，期望得到有序去重列表。
 */
test('parses keyword text with mixed separators and dedupes', () => {
  assert.deepEqual(
    parseHandoffKeywords('转人工，人工客服, human; HUMAN\n 转人工 '),
    ['转人工', '人工客服', 'human'],
  )
  assert.deepEqual(parseHandoffKeywords('  ,，;\n'), [])
  const many = Array.from({ length: 30 }, (_, i) => `kw${i}`).join(',')
  assert.equal(parseHandoffKeywords(many).length, 20)
  assert.equal(formatHandoffKeywords(['a', 'b']), 'a，b')
  assert.equal(formatHandoffKeywords(undefined), '')
})

/**
 * 验证配置与表单的双向转换：缺失字段回落默认值，表单序列化时
 * 清洗关键词并裁剪数字/空白。构造部分配置往返一遍，期望字段一致。
 */
test('round-trips config through the form shape', () => {
  const form = handoffFormFromConfig({
    enabled: true,
    keywords: ['转人工'],
    fallback_threshold: 3,
    webhook_url: 'https://example.com/hook',
    webhook_format: 'wecom',
  })
  assert.equal(form.enabled, true)
  assert.equal(form.keywordsText, '转人工')
  assert.equal(form.fallbackThreshold, 3)
  assert.equal(form.timeoutMinutes, 60)
  assert.equal(form.webhookFormat, 'wecom')

  const cfg = handoffConfigFromForm({ ...form, autoReply: '  稍等 ', fallbackThreshold: 2.6 })
  assert.deepEqual(cfg.keywords, ['转人工'])
  assert.equal(cfg.auto_reply, '稍等')
  assert.equal(cfg.fallback_threshold, 3)
  assert.equal(cfg.webhook_url, 'https://example.com/hook')

  assert.equal(handoffFormFromConfig(undefined).enabled, false)
})

/**
 * 验证保存前校验：开启但没有任何触发方式、webhook 非 http(s) 时返回
 * 对应提示 key；关闭状态与合法配置返回 null。
 */
test('validates enabled forms for a trigger and webhook scheme', () => {
  const off = defaultHandoffForm()
  assert.equal(validateHandoffForm(off), null)

  const on = { ...defaultHandoffForm(), enabled: true }
  assert.equal(validateHandoffForm(on), 'agentEditor.im.handoff.needTrigger')
  assert.equal(validateHandoffForm({ ...on, fallbackThreshold: 3 }), null)
  assert.equal(validateHandoffForm({ ...on, keywordsText: '转人工' }), null)
  assert.equal(
    validateHandoffForm({ ...on, keywordsText: '转人工', webhookUrl: 'ftp://x' }),
    'agentEditor.im.handoff.badWebhook',
  )
  assert.equal(
    validateHandoffForm({ ...on, keywordsText: '转人工', webhookUrl: 'https://hooks.example.com/a' }),
    null,
  )
})
