/// <reference types="vite/client" />
import { describe } from 'vitest'
import { checkMessages } from '@sub2api/ui/testing'
import messages from './messages'

describe('messages', () => {
  checkMessages(messages, {
    sources: import.meta.glob<string>(['./*.vue', './*.ts', '!./*.spec.ts', '!./messages*.ts'], { query: '?raw', import: 'default', eager: true }),
    // Keys looked up with a computed suffix: enumOptions(group, …) (host.ts) and t(`scope.${…}`), t(`it.err.${…}`), t(`cfg.summary.${kind}Group`) and the like.
    dynamic: [
      ...['action', 'editMode', 'floor', 'it.mode', 'it.whenEmpty', 'join', 'kind', 'metric', 'op', 'scope'],
      ...['acct.card.state', 'acct.table.stateAria', 'cfg.field.short', 'cfg.floorValue', 'cfg.summary', 'it.d.basis', 'it.err', 'it.json', 'it.kind', 'it.ro', 'json', 'reason', 'stats', 'tabs', 'verdict']
    ].map((g) => `${g}.`)
  })
})
