// A supplier agent signed in to the dev mock (CONTRACTS §102.1): the users
// page, talking to the mock as it talks to the core, offers the agent the edit
// and role actions on a user below it, and the roles and changes it makes go
// through.
import { afterEach, beforeAll, describe, expect, it, vi } from 'vitest'
import { enableAutoUnmount, flushPromises, mount } from '@vue/test-utils'
import { createPinia, setActivePinia } from 'pinia'
import { session } from '@sub2api/host'
import { i18n } from '@/i18n'
import { useAuthStore } from '@/stores/auth'
import { mockServer, type MockServer } from '@/testing/mockServer'
import UsersView from './UsersView.vue'

vi.mock('vue-router', () => ({ useRoute: () => ({ query: {} }), useRouter: () => ({ push: vi.fn() }) }))
enableAutoUnmount(afterEach)

const SDropdown = { props: ['actions'], emits: ['select'], template: '<div class="row-actions"><button v-for="a in actions.filter(x => !x.hidden)" :key="a.key" :data-action="a.key" @click="$emit(\'select\', a.key)">{{ a.key }}</button></div>' }
const SFormModal = { props: ['open'], emits: ['submit'], template: '<form v-if="open" data-testid="modal" @submit.prevent="$emit(\'submit\')"><slot /></form>' }

let server: MockServer
let admin = ''
beforeAll(async () => {
  server = await mockServer('core', 'accounts', 'resources', 'billing')
  admin = await server.signIn('admin@example.com')
  const agent = await server.signIn('agent@example.com')
  vi.stubGlobal('fetch', server.fetch)
  session.set({ access_token: agent })
  setActivePinia(createPinia())
  useAuthStore().me = (await server.call('GET', '/me', undefined, agent)).body.data
  i18n.global.locale.value = 'zh'
})

async function render() {
  const w = mount(UsersView, { global: { plugins: [i18n], stubs: { SDropdown, SFormModal, Teleport: true }, directives: { permission: {} } } })
  await flushPromises()
  return w
}
const asAdmin = async (id: number) => (await server.call('GET', `/users/${id}`, undefined, admin)).body.data
/** The table row of user `id`. */
async function rowOf(w: Awaited<ReturnType<typeof render>>, id: number) {
  const { email } = await asAdmin(id)
  return w.findAll('tbody tr').find((tr) => tr.text().includes(email))!
}

describe('a supplier agent on the users page', () => {
  it('assigns a role it manages to a user below it', async () => {
    const w = await render()
    const row = await rowOf(w, 100)
    expect(row.findAll('.row-actions button').map((b) => b.attributes('data-action'))).toEqual(['edit', 'roles'])
    await row.get('[data-action="roles"]').trigger('click')
    await flushPromises()
    await w.get('[data-testid="modal"] input[value="supplier_agent"]').setValue(true)
    await w.get('[data-testid="modal"]').trigger('submit')
    await flushPromises()
    expect((await asAdmin(100)).roles).toEqual(['user', 'supplier_agent'])
  })

  it('edits a user below it', async () => {
    const w = await render()
    await (await rowOf(w, 101)).get('[data-action="edit"]').trigger('click')
    await flushPromises()
    await w.get('[data-testid="modal"] input').setValue('Below')
    await w.get('[data-testid="modal"]').trigger('submit')
    await flushPromises()
    expect(await asAdmin(101)).toMatchObject({ display_name: 'Below', owner_id: 11 })
  })
})
