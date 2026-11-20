import { definePluginHost } from '@sub2api/host'

// Plugin-scoped host handed to register(); the settings form reads it.
export const { setHost, useHost: useMailerHost } = definePluginHost('mailer')
