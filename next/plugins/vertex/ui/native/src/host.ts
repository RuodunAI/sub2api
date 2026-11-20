import { definePluginHost } from '@sub2api/host'

// Plugin-scoped host handed to register(); the forms read their messages
// through it (plugin.vertex.*).
export const { setHost, useHost: useVertexHost } = definePluginHost('vertex')
