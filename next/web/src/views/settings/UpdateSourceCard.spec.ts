import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { enableAutoUnmount, flushPromises, mount } from '@vue/test-utils'
import { createPinia, setActivePinia } from 'pinia'
import { i18n } from '@/i18n'
import { updateCheck } from '@/stores/__fixtures__/updateCheck'
import UpdateSourceCard from './UpdateSourceCard.vue'

const api = vi.hoisted(() => ({ get: vi.fn(), put: vi.fn() }))
vi.mock('@sub2api/host', async orig => ({ ...(await orig<object>()), api }))
vi.mock('@/stores/auth', () => ({ useAuthStore: () => ({ has: () => true }) }))
enableAutoUnmount(afterEach)

const SECRET = 'github_pat_write-only'
const stored = { enabled: true, kind: 'github', repository: 'owner/private', url: '', has_secret: true, default_repository: 'Sub2API-Devs/sup2api', revision: 3 }
let check = updateCheck()

beforeEach(() => {
  setActivePinia(createPinia())
  i18n.global.locale.value = 'zh'
  check = updateCheck()
  api.get.mockReset().mockImplementation((path: string) => Promise.resolve(path === '/system/update-source' ? stored : check))
  api.put.mockReset().mockImplementation((_path, body) => Promise.resolve({ ...stored, ...body, secret: undefined, has_secret: body.secret === undefined ? stored.has_secret : !!body.secret }))
})

async function render() {
  const w = mount(UpdateSourceCard, { global: { plugins: [i18n] } })
  await flushPromises()
  return w
}
const save = async (w: Awaited<ReturnType<typeof render>>) => {
  await w.get('[data-testid="settings-save"]').trigger('click')
  await flushPromises()
}
const putBody = () => api.put.mock.calls.at(-1)![1]

describe('update source settings', () => {
  it('hides the secret for a public source and sends none', async () => {
    api.get.mockImplementation((path: string) => Promise.resolve(path === '/system/update-source' ? { ...stored, has_secret: false } : check))
    const w = await render()
    expect(w.find('input[name="secret"]').exists()).toBe(false)
    await w.get('input[name="repository"]').setValue('owner/public ')
    await save(w)
    expect(putBody()).toEqual({ enabled: true, kind: 'github', repository: 'owner/public', url: '' })
  })

  it('shows the secret field once the source is marked private and saves only with a secret', async () => {
    api.get.mockImplementation((path: string) => Promise.resolve(path === '/system/update-source' ? { ...stored, has_secret: false } : check))
    const w = await render()
    await w.get('[data-testid="update-source-private"]').trigger('click')
    expect(w.get('[data-testid="settings-save"]').attributes('disabled')).toBeDefined()
    await w.get('input[name="secret"]').setValue(SECRET)
    await save(w)
    expect(putBody()).toMatchObject({ secret: SECRET })
  })

  it('shows a stored secret as private, never its value, and keeps it when the field is empty', async () => {
    const w = await render()
    expect((w.get('input[name="secret"]').element as HTMLInputElement).placeholder).toContain('已设置')
    await w.get('[data-testid="update-source-enabled"]').trigger('click')
    await save(w)
    expect(putBody()).toEqual({ enabled: false, kind: 'github', repository: 'owner/private', url: '' })
  })

  it('sends a new secret once and forgets it after saving', async () => {
    const w = await render()
    await w.get('input[name="secret"]').setValue(SECRET)
    await save(w)
    expect(putBody()).toMatchObject({ secret: SECRET })
    expect((w.get('input[name="secret"]').element as HTMLInputElement).value).toBe('')
    expect(w.html()).not.toContain(SECRET)
  })

  it('clears the stored secret when private is turned off', async () => {
    const w = await render()
    const toggle = w.get('[data-testid="update-source-private"]')
    await toggle.trigger('click')
    expect(w.find('input[name="secret"]').exists()).toBe(false)
    expect(w.find('[data-testid="update-source-secret-clears"]').exists()).toBe(true)
    await toggle.trigger('click')
    expect(w.find('[data-testid="settings-save"]').attributes('disabled')).toBeDefined()
    await toggle.trigger('click')
    await save(w)
    expect(putBody()).toMatchObject({ secret: '' })
  })

  it('warns that changing the source drops the saved secret', async () => {
    const w = await render()
    expect(w.find('[data-testid="update-source-secret-dropped"]').exists()).toBe(false)
    await w.get('input[name="repository"]').setValue('owner/other')
    expect(w.find('[data-testid="update-source-secret-dropped"]').exists()).toBe(true)
    await save(w)
    expect(api.put).not.toHaveBeenCalled()
    expect(w.text()).toContain('请填写密钥')
    await w.get('input[name="secret"]').setValue(SECRET)
    expect(w.find('[data-testid="update-source-secret-dropped"]').exists()).toBe(false)
  })

  it('restores the default repository and switches to a URL source', async () => {
    const w = await render()
    await w.get('[data-testid="update-source-default"]').trigger('click')
    expect((w.get('input[name="repository"]').element as HTMLInputElement).value).toBe('Sub2API-Devs/sup2api')
    await w.get('[data-testid="update-source-kind"] input[value="url"]').setValue(true)
    expect(w.find('input[name="repository"]').exists()).toBe(false)
    await w.get('input[name="url"]').setValue('https://releases.example.com/s2a/')
    expect(w.text()).toContain('Bearer')
    await w.get('[data-testid="update-source-private"]').trigger('click')
    await save(w)
    expect(putBody()).toEqual({ enabled: true, kind: 'url', repository: 'Sub2API-Devs/sup2api', url: 'https://releases.example.com/s2a/', secret: '' })
  })

  it('checks again after saving and shows both latest versions or the error', async () => {
    const w = await render()
    expect(w.get('[data-testid="update-source-result"]').text()).toContain('最新核心 0.1.291，最新网关 0.1.291')
    check = updateCheck({ gateway: { error: 'GitHub API rate limited' } })
    await w.get('[data-testid="update-source-enabled"]').trigger('click')
    await save(w)
    expect(api.get).toHaveBeenLastCalledWith('/system/update-check?force=true')
    const result = w.get('[data-testid="update-source-result"]').text()
    expect(result).toContain('网关: GitHub API rate limited')
    expect(result).not.toContain('连接正常')
  })

  it('gives the reason of a line with no stable release, not a failure', async () => {
    check = updateCheck({ gateway: { latest_version: '', compatible: false, reason: 'the update source has no stable gateway release' } })
    const result = (await render()).get('[data-testid="update-source-result"]').text()
    expect(result).toContain('网关: the update source has no stable gateway release')
    expect(result).not.toContain('检测失败')
    expect(result).not.toContain('连接正常')
  })
})
