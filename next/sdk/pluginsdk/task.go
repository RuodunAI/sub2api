package pluginsdk

import (
	"fmt"

	"github.com/Sub2API-Devs/sup2api/next/sdk/manifest"
)

// checkTaskManifest holds a plugin to what its manifest promises about
// tasks: platform.monitor.v1 needs a TaskMonitor, and task endpoints (or
// platform.tasks.v1) need both capabilities declared, a TaskMonitor to
// observe what Execute submits and, when ExecuteDefault runs Execute (no
// Executor of its own), a TaskSubmissionParser to describe the submission.
func checkTaskManifest(p any, m *manifest.Manifest, declared map[string]bool) error {
	_, platform := p.(Platform)
	_, monitor := p.(TaskMonitor)
	monitors := declared[manifest.CapPlatformAdapter] && declared[manifest.CapPlatformMonitor] && platform && monitor
	if declared[manifest.CapPlatformMonitor] && !monitors {
		return fmt.Errorf("%s requires %s, Platform and TaskMonitor", manifest.CapPlatformMonitor, manifest.CapPlatformAdapter)
	}
	wanted := declared[manifest.CapPlatformTasks]
	for _, platform := range m.Platforms {
		for _, e := range platform.Endpoints {
			wanted = wanted || e.Task != nil
		}
	}
	if wanted && !(declared[manifest.CapPlatformTasks] && monitors) {
		return fmt.Errorf("managed tasks require %s, %s, %s, Platform and TaskMonitor", manifest.CapPlatformTasks, manifest.CapPlatformAdapter, manifest.CapPlatformMonitor)
	}
	_, executor := p.(Executor)
	_, parser := p.(TaskSubmissionParser)
	if wanted && !executor && !parser {
		return fmt.Errorf("managed tasks submitted through ExecuteDefault (a Platform without Executor) require TaskSubmissionParser")
	}
	return nil
}
