/* global process, document, getComputedStyle, URL */
// The plugin permission screens in a browser, run against the mock app (no
// backend), like phone-layout.test.mjs:
//
//   npx vite --mode mock --port 5185 &
//   NODE_PATH=$(npm root -g) E2E_WEB_URL=http://localhost:5185 node --test e2e/plugin-permissions.test.mjs
//
// On a desktop screen, in English, it checks
// - the role editor: the guard's module lists its permissions under their
//   groups with risk badges (a removed one kept on the role that holds it),
//   compares a built-in role with the plugin's suggestion and restores it
//   (scopes included), asks before granting a sensitive permission, ticks a
//   group without its critical permission, and greys out what a non-super
//   editor does not hold;
// - the install / upgrade consent matrix: cells prefilled from the
//   suggestions, one cell changed sends just that permission, a non-super
//   viewer sees its cells held for review and the critical row locked, an
//   upgrade lists only the new permissions with the revived and removed ones
//   apart, and a renamed row starts from the old key's holders, editable;
// - the plugin detail "Permissions" tab: a cell changed shows the unsaved
//   bar and saves to the same role data the Roles page edits; a group ticked
//   leaves the critical row; granting a critical permission asks first and
//   then for step-up; a save after someone else's (409 version_conflict) is
//   sent again at once when the roles it saves are as they were, else
//   reloads with the edits kept to save again; a non-super editor offers
//   only the scopes it holds; a host permission grant narrowed, revoked and
//   granted again; the old ?tab=grants link lands on it;
// - in the dark theme the matrix's pinned first column is opaque, so cells
//   scrolled under it do not show through.
// The mock keeps its state while it runs: every test that saves puts the
// state back, so the file can run again against the same server.
import { createRequire } from 'node:module'
import { after, before, describe, test } from 'node:test'
import assert from 'node:assert/strict'

const { chromium } = createRequire(import.meta.url)('playwright')

const BASE = process.env.E2E_WEB_URL || 'http://localhost:5185'
const DESKTOP = { viewport: { width: 1280, height: 900 }, locale: 'en-US' }
const GUARD = 'plugin.guard'

let browser
before(async () => {
  browser = await chromium.launch()
})
after(() => browser?.close())

/** A new page signed in as email (mock password: admin). */
async function signIn(email = 'admin@example.com', options = {}) {
  const context = await browser.newContext({ ...DESKTOP, ...options })
  context.setDefaultTimeout(10_000)
  const page = await context.newPage()
  await page.goto(`${BASE}/login`)
  await page.fill('input[type=email]', email)
  await page.fill('input[type=password]', 'admin')
  await page.click('button[type=submit]')
  await page.waitForURL((url) => !url.pathname.startsWith('/login'))
  return page
}

/** Answers the step-up dialog a guarded request opens (mock: the password). */
async function stepUp(page) {
  const dialog = page.locator('[data-testid="step-up-dialog"]')
  await dialog.locator('input[type="password"]').fill('admin')
  await page.locator('[data-testid="step-up-submit"]').click()
}

/** The next successful call of method on path: the request's JSON body and the response's data. */
async function nextCall(page, method, path, act) {
  const [response] = await Promise.all([
    page.waitForResponse((r) => r.request().method() === method && new URL(r.url()).pathname === `/api/v1${path}` && r.status() < 400),
    act()
  ])
  return { body: response.request().postDataJSON(), data: (await response.json().catch(() => null))?.data }
}

const row = (scope, action) => scope.locator(`tr[data-permission="${action}"]`)
/** A matrix cell's control: the checkbox of an unscoped permission, the select of a scoped one. */
const cell = (scope, action, role) => row(scope, action).locator(`td[data-role="${role}"]`).locator('input[type=checkbox], select')
const checked = (locator) => locator.evaluate((el) => el.checked)

/** A role of the role list (its key is the second line of the button). */
async function openRole(page, key) {
  await page.goto(`${BASE}/roles`)
  await page.locator('main button', { has: page.locator('.font-mono', { hasText: new RegExp(`^${key}$`) }) }).click()
  const shown = page.locator('[data-testid="role-key"] input, input[data-testid="role-key"]').first()
  await page.waitForFunction(([el, k]) => el.value === k, [await shown.elementHandle(), key])
  return page.locator(`[data-module="${GUARD}"]`)
}
/** A permission of the role editor: the label around its checkbox (the input carries the key). */
const permission = (module, key) => module.locator(`input[data-permission="${GUARD}:${key}"]`).locator('xpath=..')

describe('role editor', () => {
  test('the plugin module: groups, risk badges, the suggestion restored with its scope, saved', async () => {
    const page = await signIn()
    const guard = await openRole(page, 'operator')
    await guard.waitFor()

    const groups = await guard.locator('[data-testid="permission-section"]').evaluateAll((sections) =>
      sections.map((s) => [s.dataset.group, Array.from(s.querySelectorAll('[data-permission]'), (p) => p.dataset.permission.split(':').slice(1).join(':'))])
    )
    assert.deepEqual(groups, [
      ['rules', ['rules:read', 'rules:manage', 'rules:test']],
      ['ops', ['stats:read', 'alerts:mute', 'bypass']]
    ])
    // The operator still holds rules:test, which the running version no longer declares.
    await permission(guard, 'rules:test').getByText('Removed').waitFor()
    const risk = async (key) => permission(guard, key).locator('[data-risk]').getAttribute('data-risk')
    assert.equal(await risk('rules:read'), 'low', 'a plugin module badges low risk too')
    assert.equal(await risk('rules:manage'), 'sensitive')
    assert.equal(await risk('bypass'), 'critical')

    // The operator holds stats:read only: rules:read and rules:manage (own) differ from the suggestion.
    const mark = (key) => permission(guard, key).locator('[data-testid="permission-mark"]')
    assert.equal(await mark('stats:read').textContent(), 'Plugin suggestion')
    assert.equal(await mark('rules:read').textContent(), 'Differs from suggestion')
    await guard.locator('[data-testid="module-restore"]').click()
    await guard.locator('[data-testid="module-differs"]').waitFor({ state: 'detached' })
    for (const key of ['rules:read', 'rules:manage', 'stats:read']) assert.equal(await mark(key).textContent(), 'Plugin suggestion', key)
    // rules:manage comes back at the suggested scope: created by them, no chosen rule set.
    const scope = guard.locator(`[data-testid="scope-${GUARD}:rules:manage"]`)
    assert.equal(await checked(scope.locator('[data-testid="scope-own"]')), true)
    assert.equal(await checked(scope.locator('[data-testid="scope-all"]')), false)

    // rules:manage is sensitive: the save lists it and asks first.
    const saved = await nextCall(page, 'PUT', '/roles/4/permissions', async () => {
      await page.locator('[data-testid="role-save"]').click()
      const dialog = page.locator('[role="dialog"]', { hasText: 'Grant sensitive or security-critical permissions?' })
      await dialog.getByText('Manage rules').waitFor()
      await page.locator('[data-testid="confirm-ok"]').click()
    })
    const mine = saved.body.permission_keys.filter((k) => k.startsWith(`${GUARD}:`)).sort()
    assert.deepEqual(mine, [`${GUARD}:rules:manage:own`, `${GUARD}:rules:read`, `${GUARD}:rules:test`, `${GUARD}:stats:read`])
    assert.equal(typeof saved.body.version, 'number', 'the save carries the version it read')

    // Back to the fixture: the rules group cleared, saved.
    for (const key of ['rules:read', 'rules:manage']) await permission(guard, key).locator('input').uncheck()
    const back = await nextCall(page, 'PUT', '/roles/4/permissions', () => page.locator('[data-testid="role-save"]').click())
    assert.deepEqual(back.body.permission_keys.filter((k) => k.startsWith(`${GUARD}:`)).sort(), [`${GUARD}:rules:test`, `${GUARD}:stats:read`])
    assert.equal(await mark('rules:read').textContent(), 'Differs from suggestion')
    await page.context().close()
  })

  test('a group ticked whole leaves its critical permission to be ticked alone', async () => {
    const page = await signIn()
    const guard = await openRole(page, 'vendor')
    await guard.locator('[data-group="ops"] [data-testid="group-select-all"]').click()
    assert.equal(await checked(permission(guard, 'stats:read').locator('input')), true)
    assert.equal(await checked(permission(guard, 'alerts:mute').locator('input')), true)
    assert.equal(await checked(permission(guard, 'bypass').locator('input')), false, 'the critical permission came with the group')
    // Not saved: discarded with the page.
    await page.context().close()
  })

  test('a plugin resource scope is chosen from the plugin options', async () => {
    const page = await signIn()
    const guard = await openRole(page, 'vendor')
    await permission(guard, 'rules:manage').locator('input').click()
    const scope = guard.locator(`[data-testid="scope-${GUARD}:rules:manage"]`)
    // Ticked, it takes the widest scope; narrowed to one rule set picked by name.
    await scope.locator('[data-testid="scope-all"]').uncheck()
    await scope.locator('[data-testid="scope-own"]').uncheck()
    await scope.locator('[data-testid="scope-ref-picker"] [role="combobox"]').click()
    await scope.locator('[data-testid="scope-ref-picker"] [role="combobox"]').fill('Prompt')
    await page.getByRole('option', { name: /^Prompt injection/ }).click()
    await scope.locator('[data-testid="scope-refs"]').getByText('Prompt injection').waitFor()
    // Not saved: discarded with the page.
    await page.context().close()
  })

  test('a non-super editor sees what it does not hold greyed out', async () => {
    const page = await signIn('manager@example.com')
    const guard = await openRole(page, 'vendor')
    for (const key of ['alerts:mute', 'bypass']) {
      assert.equal(await permission(guard, key).locator('input').isDisabled(), true, `${key} can be ticked`)
      await permission(guard, key).locator('[data-testid="permission-not-held"]').waitFor()
    }
    assert.equal(await permission(guard, 'rules:read').locator('input').isDisabled(), false)
    assert.equal(await permission(guard, 'rules:read').locator('[data-testid="permission-not-held"]').count(), 0)
    await page.context().close()
  })
})

describe('plugin detail permissions tab', () => {
  test('the old ?tab=grants link opens the permissions tab', async () => {
    const page = await signIn()
    await page.goto(`${BASE}/plugins/guard?tab=grants`)
    await page.waitForURL((url) => url.searchParams.get('tab') === 'permissions')
    assert.equal(await page.locator('main [role="tab"][aria-selected="true"]').getAttribute('data-key'), 'permissions')
    await page.locator('[data-testid="plugin-host-permissions"]').waitFor()
    await page.locator('[data-testid="plugin-user-permissions"] [data-testid="perm-matrix"]').waitFor()
    await page.context().close()
  })

  test('a cell changed shows the unsaved bar; saved, the Roles page has the same keys', async () => {
    const page = await signIn()
    await page.goto(`${BASE}/plugins/guard?tab=permissions`)
    const matrix = page.locator('[data-testid="plugin-user-permissions"]')
    const bar = matrix.locator('[data-testid="matrix-save"]')
    const target = cell(matrix, `${GUARD}:rules:read`, 'supplier')
    assert.equal(await checked(target), false)
    assert.equal(await target.isDisabled(), false)

    await target.check()
    await bar.waitFor()
    assert.match(await bar.textContent(), /1 unsaved change\s*Supplier · View rules: Not granted → Granted/)
    assert.equal(await row(matrix, `${GUARD}:rules:read`).locator('td[data-role="supplier"] [data-testid="cell-differs"]').count(), 1)
    // Reset drops it; ticked again and saved, only the supplier goes out, with all its guard keys.
    await bar.locator('[data-testid="settings-reset"]').click()
    await bar.waitFor({ state: 'detached' })
    assert.equal(await checked(target), false)
    await target.check()
    const saved = await nextCall(page, 'PUT', '/permissions/plugins/guard', () => bar.locator('[data-testid="settings-save"]').click())
    assert.deepEqual(saved.body.roles, { supplier: [`${GUARD}:rules:read`] })
    assert.equal(typeof saved.body.version, 'number', 'the save carries the version it read')
    await bar.waitFor({ state: 'detached' })
    assert.equal(await checked(target), true)

    const guard = await openRole(page, 'supplier')
    assert.equal(await checked(permission(guard, 'rules:read').locator('input')), true, 'the Roles page does not show the grant')

    // Back to the fixture.
    await page.goto(`${BASE}/plugins/guard?tab=permissions`)
    await target.uncheck()
    await nextCall(page, 'PUT', '/permissions/plugins/guard', () => bar.locator('[data-testid="settings-save"]').click())
    await page.context().close()
  })

  test('a scoped cell takes chosen plugin resources; saved, the Roles page has them', async () => {
    const page = await signIn()
    await page.goto(`${BASE}/plugins/guard?tab=permissions`)
    const matrix = page.locator('[data-testid="plugin-user-permissions"]')
    const select = cell(matrix, `${GUARD}:rules:manage`, 'operator')
    await select.selectOption({ label: 'Chosen…' })
    const panel = matrix.locator('[data-testid="matrix-scope-panel"]')
    await panel.waitFor()
    await panel.locator('[data-testid="scope-ref-picker"] [role="combobox"]').click()
    await panel.locator('[data-testid="scope-ref-picker"] [role="combobox"]').fill('PII')
    await page.getByRole('option', { name: /^PII/ }).click()
    await panel.locator('[data-testid="matrix-scope-done"]').click()
    assert.equal(await select.evaluate((el) => el.selectedOptions[0].textContent.trim()), '1 chosen')

    const bar = matrix.locator('[data-testid="matrix-save"]')
    // A sensitive permission newly granted: asked first.
    const saved = await nextCall(page, 'PUT', '/permissions/plugins/guard', async () => {
      await bar.locator('[data-testid="settings-save"]').click()
      await page.locator('[data-testid="confirm-ok"]').click()
    })
    assert.deepEqual(saved.body.roles.operator.filter((k) => k.startsWith(`${GUARD}:rules:manage`)), [`${GUARD}:rules:manage:ruleset/3`])

    const guard = await openRole(page, 'operator')
    const scope = guard.locator(`[data-testid="scope-${GUARD}:rules:manage"] [data-testid="scope-refs"]`)
    await scope.getByText('PII').waitFor()

    // Back to the fixture.
    await page.goto(`${BASE}/plugins/guard?tab=permissions`)
    await select.selectOption({ label: 'Not granted' })
    await nextCall(page, 'PUT', '/permissions/plugins/guard', () => bar.locator('[data-testid="settings-save"]').click())
    await page.context().close()
  })

  test('a group ticked for a role leaves the critical row; granting it asks first, then for step-up', async () => {
    const page = await signIn()
    await page.goto(`${BASE}/plugins/guard?tab=permissions`)
    const matrix = page.locator('[data-testid="plugin-user-permissions"]')
    await matrix.locator('[data-testid="matrix-fold"]').click()
    const bypass = cell(matrix, `${GUARD}:bypass`, 'supplier')
    await matrix.locator('tr[data-group="ops"] [data-group-role="supplier"]').locator('xpath=descendant-or-self::input').check()
    assert.equal(await checked(cell(matrix, `${GUARD}:stats:read`, 'supplier')), true)
    assert.equal(await checked(bypass), false, 'the critical row came with the group')
    const bar = matrix.locator('[data-testid="matrix-save"]')
    await bar.locator('[data-testid="settings-reset"]').click()
    await bar.waitFor({ state: 'detached' })

    // Ticked alone, the save names it and asks; cancelled, nothing is sent.
    await bypass.check()
    await bar.locator('[data-testid="settings-save"]').click()
    const dialog = page.locator('[role="dialog"]', { hasText: 'Grant sensitive or security-critical permissions?' })
    await dialog.getByText(/Bypass the rules \(Security-critical\) → Supplier/).waitFor()
    await page.locator('[data-testid="confirm-cancel"]').click()
    await dialog.waitFor({ state: 'detached' })
    assert.equal(await checked(bypass), true, 'cancelling keeps the edit, unsaved')
    // Confirmed, the server asks for step-up before granting it (C3).
    const saved = await nextCall(page, 'PUT', '/permissions/plugins/guard', async () => {
      await bar.locator('[data-testid="settings-save"]').click()
      await page.locator('[data-testid="confirm-ok"]').click()
      await stepUp(page)
    })
    assert.deepEqual(saved.body.roles, { supplier: [`${GUARD}:bypass`] })
    await bar.waitFor({ state: 'detached' })

    // Back to the fixture: taking it off asks nothing.
    await bypass.uncheck()
    await nextCall(page, 'PUT', '/permissions/plugins/guard', () => bar.locator('[data-testid="settings-save"]').click())
    await page.context().close()
  })

  /** Another admin's save on the guard matrix: role gets the key ticked. */
  async function otherSave(action, role) {
    const other = await signIn()
    await other.goto(`${BASE}/plugins/guard?tab=permissions`)
    const theirs = other.locator('[data-testid="plugin-user-permissions"]')
    await cell(theirs, action, role).check()
    await nextCall(other, 'PUT', '/permissions/plugins/guard', () => theirs.locator('[data-testid="matrix-save"] [data-testid="settings-save"]').click())
    await other.context().close()
  }
  const matrixPut = (r) => r.request().method() === 'PUT' && new URL(r.url()).pathname === '/api/v1/permissions/plugins/guard'

  test('a save after someone else\'s on another role is sent again at once with the new version', async () => {
    const page = await signIn()
    await page.goto(`${BASE}/plugins/guard?tab=permissions`)
    const matrix = page.locator('[data-testid="plugin-user-permissions"]')
    const mine = cell(matrix, `${GUARD}:stats:read`, 'supplier')
    await mine.check()
    // Meanwhile another admin grants the operator rules:read: the version moves, the supplier does not.
    await otherSave(`${GUARD}:rules:read`, 'operator')

    const bar = matrix.locator('[data-testid="matrix-save"]')
    const statuses = []
    const bodies = []
    page.on('response', (r) => matrixPut(r) && (statuses.push(r.status()), bodies.push(r.request().postDataJSON())))
    await bar.locator('[data-testid="settings-save"]').click()
    await bar.waitFor({ state: 'detached' })
    assert.deepEqual(statuses, [409, 200])
    assert.deepEqual(bodies[1].roles, { supplier: [`${GUARD}:stats:read`] })
    assert.ok(bodies[1].version > bodies[0].version, 'sent again with the version read again')
    assert.equal(await page.getByText('The grants changed while you were editing').count(), 0)
    await page.waitForFunction((el) => el.checked, await cell(matrix, `${GUARD}:rules:read`, 'operator').elementHandle())

    // Back to the fixture.
    await mine.uncheck()
    await cell(matrix, `${GUARD}:rules:read`, 'operator').uncheck()
    await nextCall(page, 'PUT', '/permissions/plugins/guard', () => bar.locator('[data-testid="settings-save"]').click())
    await page.context().close()
  })

  test('a save after someone else\'s on the same role reloads with the edits kept, and saves again', async () => {
    const page = await signIn()
    await page.goto(`${BASE}/plugins/guard?tab=permissions`)
    const matrix = page.locator('[data-testid="plugin-user-permissions"]')
    const mine = cell(matrix, `${GUARD}:stats:read`, 'supplier')
    await mine.check()
    // Meanwhile another admin grants the supplier rules:read.
    await otherSave(`${GUARD}:rules:read`, 'supplier')

    const bar = matrix.locator('[data-testid="matrix-save"]')
    const [conflict] = await Promise.all([page.waitForResponse(matrixPut), bar.locator('[data-testid="settings-save"]').click()])
    assert.equal(conflict.status(), 409)
    await page.getByText('The grants changed while you were editing').waitFor()
    // The other save shows, the edit is still there and still unsaved.
    await page.waitForFunction((el) => el.checked, await cell(matrix, `${GUARD}:rules:read`, 'supplier').elementHandle())
    assert.equal(await checked(mine), true)
    assert.match(await bar.textContent(), /1 unsaved change\s*Supplier · View stats/)
    const saved = await nextCall(page, 'PUT', '/permissions/plugins/guard', () => bar.locator('[data-testid="settings-save"]').click())
    assert.deepEqual(saved.body.roles, { supplier: [`${GUARD}:rules:read`, `${GUARD}:stats:read`] })
    await bar.waitFor({ state: 'detached' })

    // Back to the fixture.
    await mine.uncheck()
    await cell(matrix, `${GUARD}:rules:read`, 'supplier').uncheck()
    await nextCall(page, 'PUT', '/permissions/plugins/guard', () => bar.locator('[data-testid="settings-save"]').click())
    await page.context().close()
  })

  test('a non-super editor offers only the scopes it holds and edits only the roles it manages', async () => {
    const page = await signIn('manager@example.com')
    await page.goto(`${BASE}/plugins/guard?tab=permissions`)
    const matrix = page.locator('[data-testid="plugin-user-permissions"]')
    const select = cell(matrix, `${GUARD}:rules:manage`, 'operator')
    // It holds rules:manage at the own scope only (the grant view is read with the matrix).
    await select.locator('option', { hasText: 'Created by them' }).waitFor({ state: 'attached' })
    const options = (await select.locator('option').allTextContents()).map((o) => o.trim())
    assert.deepEqual(options, ['Not granted', 'Created by them'])
    // Its own role is not on its managed list.
    assert.equal(await row(matrix, `${GUARD}:rules:read`).locator('td[data-role="admin"]').getAttribute('data-lock'), 'role_readonly')
    await page.context().close()
  })

  test('a host permission grant narrowed, revoked and granted again', async () => {
    const page = await signIn()
    await page.goto(`${BASE}/plugins/guard?tab=permissions`)
    const host = page.locator('[data-testid="plugin-host-permissions"]')
    const hook = host.locator('tr', { hasText: 'gateway.hook' })
    const fields = () => hook.locator('td').nth(2).innerText()
    await hook.waitFor()
    assert.match(await fields(), /model[\s\S]*prompt_text/)

    await hook.locator('[data-testid="grant-narrow"]').click()
    const dialog = page.locator('[role="dialog"]', { hasText: 'gateway' })
    const list = dialog.locator('[data-list="fields"]')
    await list.getByRole('button', { name: 'prompt_text' }).click()
    // The last item of a list stays.
    assert.equal(await list.getByRole('button', { name: 'model' }).isDisabled(), true)
    const narrowed = await nextCall(page, 'PUT', '/plugins/guard/grants/gateway.hook', async () => {
      await page.locator('[data-testid="narrow-submit"]').click()
      await stepUp(page)
    })
    assert.deepEqual(narrowed.body, { scope: { points: ['gateway.request'], fields: ['model'] } })
    await page.locator('[data-testid="narrow-submit"]').waitFor({ state: 'detached' })
    await hook.locator('td').nth(2).filter({ hasNotText: 'prompt_text' }).waitFor()
    assert.equal(await hook.locator('[data-testid="grant-narrow"]').count(), 0, 'a list of one is offered for narrowing')

    await hook.getByRole('button', { name: 'Revoke' }).click()
    await nextCall(page, 'DELETE', '/plugins/guard/grants/gateway.hook', () => page.locator('[data-testid="confirm-ok"]').click())
    await hook.locator('[data-testid="grant-regrant"]').waitFor()

    // Granted again, it takes the scope the manifest requests.
    const regranted = await nextCall(page, 'PUT', '/plugins/guard/grants/gateway.hook', async () => {
      await hook.locator('[data-testid="grant-regrant"]').click()
      await page.locator('[data-testid="confirm-ok"]').click()
      await stepUp(page)
    })
    assert.deepEqual(regranted.body, {})
    assert.deepEqual(regranted.data.scope, { points: ['gateway.request'], fields: ['model', 'prompt_text'] })
    await hook.locator('[data-testid="grant-narrow"]').waitFor()
    await page.context().close()
  })
})

describe('consent matrix', () => {
  const INSTALL = '/plugins/guard/versions/0.1.0/consent'
  const UPGRADE = '/plugins/guard/versions/0.2.0/consent'

  test('install: prefilled from the suggestions; one cell changed sends just that permission', async () => {
    const page = await signIn()
    await page.goto(`${BASE}${INSTALL}`)
    const matrix = page.locator('[data-testid="permission-matrix"]')
    await matrix.locator('tr[data-permission]').first().waitFor()

    assert.equal(await checked(cell(matrix, `${GUARD}:rules:read`, 'admin')), true)
    assert.equal(await checked(cell(matrix, `${GUARD}:rules:read`, 'user')), false)
    assert.equal(await cell(matrix, `${GUARD}:rules:manage`, 'operator').evaluate((el) => el.selectedOptions[0].textContent.trim()), 'Created by them')
    assert.equal(await checked(cell(matrix, `${GUARD}:stats:read`, 'supplier')), true)
    // The super admin holds everything, fixed.
    const superCell = row(matrix, `${GUARD}:bypass`).locator('td[data-role="super_admin"]')
    assert.equal(await superCell.getAttribute('data-lock'), 'superuser')
    assert.equal(await checked(superCell.locator('input')), true)
    assert.equal(await superCell.locator('input').isDisabled(), true)
    // Custom roles fold under "more roles".
    assert.equal(await matrix.locator('th[data-role="vendor"]').count(), 0)
    await matrix.locator('[data-testid="matrix-more-roles"]').click()
    await matrix.locator('th[data-role="vendor"]').waitFor()
    assert.equal(await matrix.locator('[data-testid="cell-differs"]').count(), 0)
    assert.equal(await page.locator('[data-testid="consent-summary-pending"]').count(), 0)

    await cell(matrix, `${GUARD}:stats:read`, 'user').check()
    assert.equal(await row(matrix, `${GUARD}:stats:read`).locator('[data-testid="cell-differs"]').count(), 1)
    // The host permissions of high risk are ticked by hand.
    for (const box of await page.locator('[data-testid="host-permissions"] input[type=checkbox]').all()) await box.check()
    const sent = await nextCall(page, 'POST', `/plugins/guard/versions/0.1.0/consent`, async () => {
      await page.locator('[data-testid="consent-approve"]').click()
      await stepUp(page)
    })
    // The changed row lists every role the viewer may name; [] grants it nothing.
    const roles = sent.body.new_permission_roles
    assert.deepEqual(Object.keys(roles), ['stats:read'])
    const granted = Object.entries(roles['stats:read']).filter(([, g]) => g === true || g.length)
    assert.deepEqual(Object.fromEntries(granted), { admin: true, operator: true, supplier: true, user: true })
    await page.waitForURL((url) => url.pathname === '/plugins/guard')
    await page.context().close()
  })

  test('as a non-super admin: suggestions held for review, the critical row locked', async () => {
    const page = await signIn('manager@example.com')
    await page.goto(`${BASE}${INSTALL}`)
    const matrix = page.locator('[data-testid="permission-matrix"]')
    await matrix.locator('tr[data-permission]').first().waitFor()

    const adminCell = row(matrix, `${GUARD}:rules:read`).locator('td[data-role="admin"]')
    assert.equal(await adminCell.getAttribute('data-lock'), 'pending_review')
    assert.equal(await adminCell.locator('input').isDisabled(), true)
    assert.equal(await checked(adminCell.locator('input')), true, 'the held suggestion does not show')
    await adminCell.getByText('Awaiting super admin').waitFor()
    for (const role of ['admin', 'operator', 'supplier', 'user']) {
      const locked = row(matrix, `${GUARD}:bypass`).locator(`td[data-role="${role}"]`)
      assert.equal(await locked.getAttribute('data-lock'), 'critical', role)
      assert.equal(await locked.locator('input').isDisabled(), true, role)
    }
    await row(matrix, `${GUARD}:bypass`).getByText('Only a super admin can grant this').waitFor()
    // The footer says how many suggestions go to review.
    const pending = page.locator('[data-testid="consent-summary-pending"]')
    await pending.waitFor()
    assert.match(await pending.textContent(), /^\s*\d+/)
    await page.context().close()
  })

  test('upgrade: only the new permissions in the matrix; revived and removed apart', async () => {
    const page = await signIn()
    await page.goto(`${BASE}${UPGRADE}`)
    const matrix = page.locator('[data-testid="permission-matrix"]')
    await matrix.locator('tr[data-permission]').first().waitFor()
    const rows = await matrix.locator('tr[data-permission]').evaluateAll((trs) => trs.map((tr) => tr.dataset.permission))
    assert.deepEqual(rows.sort(), [`${GUARD}:alerts:silence`, `${GUARD}:reports:export`, `${GUARD}:rules:publish`])
    // The new sensitive one needs confirming.
    await row(matrix, `${GUARD}:rules:publish`).locator('[data-testid="consent-need"]').waitFor()
    const changes = page.locator('[data-testid="upgrade-permission-changes"]')
    await changes.locator('[data-testid="upgrade-revived"]').getByText('Dry-run rules').waitFor()
    await changes.locator('[data-testid="upgrade-removed"]').getByText('Mute alerts').waitFor()
    await page.context().close()
  })

  test('upgrade: a renamed row starts from the old key\'s holders and is edited like any other', async () => {
    const page = await signIn()
    await page.goto(`${BASE}${UPGRADE}`)
    const matrix = page.locator('[data-testid="permission-matrix"]')
    const renamed = row(matrix, `${GUARD}:alerts:silence`)
    await renamed.locator('[data-testid="matrix-inherited"]').getByText('Inherited from alerts:mute').waitFor()
    const admin = cell(matrix, `${GUARD}:alerts:silence`, 'admin')
    assert.equal(await admin.isDisabled(), false)
    assert.notEqual(await admin.evaluate((el) => el.selectedOptions[0].textContent.trim()), 'Not granted', 'the holder does not start granted')
    await admin.selectOption({ label: 'Not granted' })
    await cell(matrix, `${GUARD}:alerts:silence`, 'operator').selectOption({ label: 'All' })

    // The consent is answered here, so the upgrade stays pending for the next run.
    let body = null
    await page.route('**/api/v1/plugins/guard/versions/0.2.0/consent', async (route) => {
      body = route.request().postDataJSON()
      await route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify({ data: { ok: true, withheld: [] } }) })
    })
    for (const box of await page.locator('[data-testid="host-permissions"] input[type=checkbox]').all()) await box.check()
    await page.locator('[data-testid="consent-approve"]').click()
    await page.waitForURL((url) => url.pathname === '/plugins/guard')
    const sent = body.new_permission_roles['alerts:silence']
    const granted = Object.fromEntries(Object.entries(sent).filter(([, g]) => g === true || g.length))
    assert.deepEqual(granted, { operator: ['*'] })
    assert.deepEqual(sent.admin, [])
    await page.context().close()
  })
})

describe('dark theme', () => {
  for (const [name, path] of [
    ['permissions tab', '/plugins/guard?tab=permissions'],
    ['consent page', '/plugins/guard/versions/0.2.0/consent']
  ]) {
    test(`the matrix's pinned first column is opaque on the ${name}`, async () => {
      const page = await signIn('admin@example.com', { colorScheme: 'dark' })
      await page.goto(`${BASE}${path}`)
      await page.locator('[data-testid="perm-matrix"] tr[data-permission]').first().waitFor()
      const backgrounds = await page.evaluate(() =>
        Array.from(document.querySelectorAll('[data-testid="perm-matrix"] th.sticky'), (th) => getComputedStyle(th).backgroundColor)
      )
      assert.ok(backgrounds.length > 1)
      const alpha = (c) => (c.startsWith('rgba') ? Number(c.slice(c.lastIndexOf(',') + 1, -1)) : 1)
      for (const c of backgrounds) assert.equal(alpha(c), 1, `a pinned cell is ${c}`)
      assert.ok(await page.evaluate(() => document.documentElement.classList.contains('dark')), 'the dark theme is not on')
      await page.context().close()
    })
  }
})
