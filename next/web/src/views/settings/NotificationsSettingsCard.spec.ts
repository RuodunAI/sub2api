import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { enableAutoUnmount, flushPromises, mount } from '@vue/test-utils'
import { i18n } from '@/i18n'
import NotificationsSettingsCard from './NotificationsSettingsCard.vue'

const api = vi.hoisted(() => ({ get: vi.fn(), list: vi.fn(), post: vi.fn() }))
vi.mock('@sub2api/host', async original => ({ ...(await original<object>()), api }))
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
