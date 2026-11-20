import { definePluginHost } from '@sub2api/host'

// Plugin-scoped host handed to register(); the form reads its messages
// through it (plugin.bedrock.*).
export const { setHost, useHost: useBedrockHost } = definePluginHost('bedrock')
