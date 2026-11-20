import { onScopeDispose } from 'vue'

/**
 * Requests the user can stop (a console account test has no time limit of
 * its own, CONTRACTS §15.9). `run` hands one request its own controller:
 * aborting it stops just that request; `stopAll` stops every request still
 * running, and so does leaving the component (its effect scope). A stopped
 * request rejects with its signal aborted: check `ctrl.signal.aborted` to
 * show it as canceled rather than failed.
 */
export function useAbortScope() {
  const live = new Set<AbortController>()
  async function run<T>(fn: (ctrl: AbortController) => Promise<T>): Promise<T> {
    const ctrl = new AbortController()
    live.add(ctrl)
    try {
      return await fn(ctrl)
    } finally {
      live.delete(ctrl)
    }
  }
  function stopAll() {
    for (const c of live) c.abort()
  }
  onScopeDispose(stopAll)
  return { run, stopAll }
}
