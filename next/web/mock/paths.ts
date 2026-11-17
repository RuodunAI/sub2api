import { dirname, join } from 'node:path'
import { fileURLToPath } from 'node:url'

/**
 * A path under next/ (the plugins' sources), joined rather than made with
 * new URL(…, import.meta.url), which a page spec's web-mode transform takes
 * for an asset import.
 */
export const nextPath = (...segments: string[]) => join(dirname(fileURLToPath(import.meta.url)), '..', '..', ...segments)
