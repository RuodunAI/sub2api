import { afterEach, beforeEach, describe, expect, it } from 'vitest'
import { flushPromises } from '@vue/test-utils'
import { SECRET_MASK } from '@sub2api/ui'
import VertexApiKeyForm from './VertexApiKeyForm.vue'
import VertexKeyFileForm from './VertexKeyFileForm.vue'
import { setHost } from './host'
import { CASES, ORDER, type VertexType } from './form.cases'
import { editor, fakeHost, fill, saved } from './form.testkit'

const FORM: Record<VertexType, any> = { vertex_apikey: VertexApiKeyForm, vertex_sa: VertexKeyFileForm, vertex_authorized_user: VertexKeyFileForm }

beforeEach(() => setHost(fakeHost()))
afterEach(() => {
  setHost(null)
  document.body.innerHTML = ''
})

describe('vertex forms: v-model and verdict of every case', () => {
  for (const c of CASES) {
    it(c.name, async () => {
      const { w, credentials, validate } = editor(FORM[c.type], { type: c.type, mode: c.mode, modelValue: c.stored ? { ...c.stored } : undefined })
      await flushPromises()
      await fill(w, ORDER[c.type], c.input ?? {})
      expect(saved(credentials.value)).toEqual(c.out)
      expect(await validate()).toBe(c.ok)
      w.unmount()
    })
  }
})

// access_token, expires_at and token_for are written by the core when it
// renews the token. A save of the form must hand back exactly what the
// editor loaded for them, whatever else changes: a dropped or rewritten
// token would make the next request wait for a fresh one.
describe('VertexKeyFileForm: the token state the core writes', () => {
  const token = { access_token: SECRET_MASK, expires_at: 1800000000, token_for: 'fp' }
  for (const type of ['vertex_sa', 'vertex_authorized_user'] as const) {
    it(`${type}: never shown, kept through every change`, async () => {
      const field = ORDER[type][0]
      const stored = { [field]: SECRET_MASK, project_id: 'my-project-123456', location: 'global', ...token }
      const { w, credentials, validate } = editor(VertexKeyFileForm, { type, mode: 'edit', modelValue: { ...stored } })
      await flushPromises()
      expect(w.findAll('input').length).toBe(3)
      expect(w.html()).not.toContain('1800000000')
      await fill(w, ORDER[type], { [field]: '{"type":"x"}', project_id: 'other-project-1', location: '' })
      expect(credentials.value).toMatchObject(token)
      expect(saved(credentials.value)).toEqual({ ...stored, [field]: '{"type":"x"}', project_id: 'other-project-1', location: '' })
      expect(await validate()).toBe(true)
      w.unmount()
    })
  }
})

describe('vertex forms: fields and texts', () => {
  const labels = (form: any, type: string) => editor(form, { type }).w.findAll('label').map((l) => l.text())

  it('shows the key file of the type, then project and location', () => {
    expect(labels(VertexKeyFileForm, 'vertex_sa')).toEqual(['Service account key*', 'Project ID', 'Location'])
    expect(labels(VertexKeyFileForm, 'vertex_authorized_user')).toEqual(['User credentials file*', 'Project ID', 'Location'])
    expect(labels(VertexApiKeyForm, 'vertex_apikey')).toEqual(['API key*', 'Project ID (optional)', 'Location (optional)'])
  })

  it('key files are secret boxes with their own placeholders', () => {
    const sa = editor(VertexKeyFileForm, { type: 'vertex_sa' }).w.findAll('input')
    expect(sa.map((i) => i.attributes('type'))).toEqual(['password', 'text', 'text'])
    expect(sa.map((i) => i.attributes('placeholder'))).toEqual(['Paste the whole JSON key file', "Leave empty to use the key file's project", 'global'])
    const user = editor(VertexKeyFileForm, { type: 'vertex_authorized_user' }).w.findAll('input')
    expect(user.map((i) => i.attributes('placeholder'))).toEqual(['Paste the whole application_default_credentials.json', 'my-project-123456', 'global'])
  })

  it('the API key form names the optional project address', () => {
    const { w } = editor(VertexApiKeyForm, { type: 'vertex_apikey' })
    expect(w.find('h4').text()).toBe('Project address (optional)')
    expect(w.text()).toContain('or leave both empty to use the express address')
    expect(w.findAll('input').map((i) => i.attributes('placeholder'))).toEqual([undefined, 'Leave empty for the express address', 'Leave empty for the express address'])
  })

  it('shows the server errors at their fields until the field changes', async () => {
    const { w } = editor(VertexApiKeyForm, { type: 'vertex_apikey', errors: { location: 'location is required when the other of project ID and location is set' } })
    await flushPromises()
    expect(w.text()).toContain('location is required')
    await fill(w, ORDER.vertex_apikey, { location: 'global' })
    expect(w.text()).not.toContain('location is required')
  })
})
