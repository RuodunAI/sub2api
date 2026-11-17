import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { enableAutoUnmount, flushPromises, mount } from '@vue/test-utils'
import { i18n } from '@/i18n'
import type { NotificationRecord } from '@/api/types'
import NotificationsSettingsCard from './NotificationsSettingsCard.vue'

const api = vi.hoisted(() => ({ get: vi.fn(), list: vi.fn(), post: vi.fn() }))
const toast = vi.hoisted(() => vi.fn())
vi.mock('@sub2api/host', async original => ({ ...(await original<object>()), api }))
vi.mock('@sub2api/ui', async original => ({ ...(await original<object>()), toast }))
vi.mock('@/stores/auth', () => ({ useAuthStore: () => ({ has: () => true }) }))
enableAutoUnmount(afterEach)

const lastError =
  'SMTP host mail.example.invalid does not exist, correct it in the mailer settings / 找不到 SMTP 服务器 mail.example.invalid（域名不存在），请在邮件插件设置里改正: dial tcp mail.example.invalid:465: no such host'

beforeEach(() => {
  vi.clearAllMocks()
  i18n.global.locale.value = 'zh'
  api.get.mockResolvedValue([])
  api.list.mockResolvedValue({
    items: [
      { id: 2, source: 'core:test', channel: 'email', recipients: ['ops@example.com'], subject: 'test', body: '', severity: 'info',
        status: 'pending', attempts: 3, last_error: lastError, next_attempt_at: '', created_at: '2026-11-17T10:47:00Z', sent_at: null }
    ],
    page: { total: 1 }
  })
})

describe('notification records (CONTRACTS §74.4)', () => {
  it('shows the whole delivery error, wrapped rather than cut off', async () => {
    const w = mount(NotificationsSettingsCard, { global: { plugins: [i18n] } })
    await flushPromises()
    const error = w.get('[data-testid="notification-error"]')
    expect(error.text()).toBe(lastError)
    expect(error.classes()).toContain('break-words')
    expect(error.classes()).not.toContain('truncate')
  })
})

describe('test send (CONTRACTS §74.4)', () => {
  const record: Omit<NotificationRecord, 'status' | 'last_error'> = { id: 5, source: 'core:test', channel: 'email', recipients: ['ops@example.com'], subject: 'test', body: '', severity: 'info',
    attempts: 1, next_attempt_at: '', created_at: '2026-11-17T10:47:00Z', sent_at: null }

  async function send(answer: Promise<NotificationRecord>) {
    api.get.mockResolvedValue([{ id: 'email', plugin_key: 'mailer', plugin_name: { en: 'Mailer' }, label: { en: 'Email' }, description: {}, recipient: 'email' }])
    api.post.mockReturnValue(answer)
    const w = mount(NotificationsSettingsCard, { global: { plugins: [i18n] } })
    await flushPromises()
    await w.get('input[name="recipients"]').setValue('ops@example.com')
    await w.get('[data-testid="notification-test"]').trigger('click')
    await flushPromises()
    return w
  }

  it('reads the record when the row is queued and again once the first attempt is known', async () => {
    let answer!: (r: NotificationRecord) => void
    await send(new Promise(r => (answer = r)))
    expect(api.post).toHaveBeenCalledWith('/notifications/test', { channel: 'email', recipients: ['ops@example.com'] })
    expect(api.list).toHaveBeenCalledTimes(2)
    expect(toast).not.toHaveBeenCalled()

    answer({ ...record, status: 'sent', last_error: '' })
    await flushPromises()
    expect(api.list).toHaveBeenCalledTimes(3)
    expect(toast).toHaveBeenCalledWith('测试通知已发送', 'success')
  })

  it('shows the whole error of a failed first attempt', async () => {
    await send(Promise.resolve({ ...record, status: 'failed', last_error: lastError }))
    expect(toast).toHaveBeenCalledWith(`测试通知发送失败：${lastError}`, 'error')
    expect(api.list).toHaveBeenCalledTimes(3)
  })

  it('says the send is still going when the wait ran out', async () => {
    await send(Promise.resolve({ ...record, status: 'sending', last_error: '' }))
    expect(toast).toHaveBeenCalledWith('测试通知仍在发送，结果稍后见投递记录', 'info')
  })
})
