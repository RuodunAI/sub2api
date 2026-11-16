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
  it('shows a stored secret as set, never its value, and leaves it unchanged when the field is empty', async () => {
    const w = await render()
    expect(w.get('[data-testid="update-source-secret-state"]').text()).toContain('已设置')
    await w.get('input[name="repository"]').setValue('owner/other ')
    await save(w)
    expect(putBody()).toEqual({ enabled: true, kind: 'github', repository: 'owner/other', url: '' })
  })

  it('sends a new secret once and forgets it after saving', async () => {
    const w = await render()
    await w.get('input[name="secret"]').setValue(SECRET)
    await save(w)
    expect(putBody()).toMatchObject({ secret: SECRET })
    expect((w.get('input[name="secret"]').element as HTMLInputElement).value).toBe('')
    expect(w.html()).not.toContain(SECRET)
  })

  it('clears the secret with an empty string and can undo it', async () => {
    const w = await render()
    const clear = w.get('[data-testid="update-source-clear-secret"]')
    await clear.trigger('click')
    expect(w.find('input[name="secret"]').exists()).toBe(false)
    await clear.trigger('click')
    expect(w.find('[data-testid="settings-save"]').attributes('disabled')).toBeDefined()
    await clear.trigger('click')
    await save(w)
    expect(putBody()).toMatchObject({ secret: '' })
  })

  it('warns that changing the source drops the saved secret', async () => {
    const w = await render()
    expect(w.find('[data-testid="update-source-secret-dropped"]').exists()).toBe(false)
    await w.get('input[name="repository"]').setValue('owner/other')
    expect(w.find('[data-testid="update-source-secret-dropped"]').exists()).toBe(true)
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
    await save(w)
    expect(putBody()).toEqual({ enabled: true, kind: 'url', repository: 'Sub2API-Devs/sup2api', url: 'https://releases.example.com/s2a/' })
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
})
