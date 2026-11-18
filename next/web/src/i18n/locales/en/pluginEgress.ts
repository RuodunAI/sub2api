export default {
  title: 'Plugin network access',
  description: 'Which domains plugins may reach through the egress tunnel, for every plugin. A plugin still needs the net permission, and one set to "Allowlist only" still reaches only its approved domains.',
  defaultLabel: 'Default for all domains',
  defaultHint: 'Choose whether plugins may reach every domain by default',
  modes: {
    allow_all: 'Allow all domains',
    block_all: 'Block all domains'
  },
  info: {
    allow_all: 'Plugins may reach every domain except the ones you block below.',
    block_all: 'Plugins may reach only the domains you allow below.'
  },
  list: {
    allow_all: 'Blocked domains',
    block_all: 'Allowed domains'
  },
  listHint: {
    allow_all: 'Plugins cannot reach these domains or their subdomains.',
    block_all: 'Plugins may reach these domains and their subdomains.'
  },
  domain: 'Domain',
  empty: 'No domains added yet.',
  add: 'Add domain',
  egressTabHint: 'The domains every plugin may reach are set in Settings → Plugin network access; they apply together with the egress policy here.',
  egressTabLink: 'Open plugin network access',
  dialog: {
    title: {
      allow_all: 'Add domains to block',
      block_all: 'Add domains to allow'
    },
    addRow: 'Add another domain',
    bulk: 'Bulk add',
    single: 'One by one',
    bulkHint: 'One domain per line; blank lines are ignored.',
    continue: 'Continue',
    invalid: 'Not a valid domain or IP address (no https://, port or path)',
    lineInvalid: 'Line {line}: "{value}" is not a valid domain or IP address',
    empty: 'Enter at least one domain.'
  }
}
