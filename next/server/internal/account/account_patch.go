package account

import (
	"context"
	"encoding/json"
	"maps"
	"slices"

	pluginv1 "github.com/Sub2API-Devs/sup2api/next/sdk/gen/pluginv1"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
)

// factsJSON is the resource_facts_json of save hooks (CONTRACTS §59): ""
// when the preparation observed nothing.
func factsJSON(f core.AccountPreparationFacts) string {
	if f == (core.AccountPreparationFacts{}) {
		return ""
	}
	raw, _ := json.Marshal(f)
	return string(raw)
}

// patchTarget holds the account fields about to be saved that a plugin's
// account_patch may change. A nil field is not offered to the plugin: its
// change is ignored.
type patchTarget struct {
	Models         *[]string
	Mapping        *map[string]string
	Priority       *int
	Weight         *int
	MaxConcurrency *int
	RPMLimit       *int
	TPMLimit       *int64
	SPMLimit       *int
	ModelLimits    *[]core.ModelLimit
}

// applyAccountPatch applies the account_patch of a before_create/update answer to
// the fields about to be saved, validated like the caller's own input
// (CONTRACTS §59, §68). It returns the names of the fields the plugin changed.
func applyAccountPatch(ctx context.Context, out *pluginv1.AccountLifecycleResponse, to patchTarget) ([]string, error) {
	patch := out.GetAccountPatch()
	if patch == nil {
		return nil, nil
	}
	var changed []string
	if list := patch.GetModels(); list != nil && to.Models != nil {
		next, fe := normalizeModels(ctx, list.GetIds())
		if len(fe) > 0 {
			return nil, invalidPatch(fe)
		}
		if !slices.Equal(next, *to.Models) {
			*to.Models = next
			changed = append(changed, "models")
		}
	}
	if m := patch.GetModelMapping(); m != nil && to.Mapping != nil {
		next, fe := normalizeMapping(ctx, m.GetEntries())
		if len(fe) > 0 {
			return nil, invalidPatch(fe)
		}
		if !maps.Equal(next, *to.Mapping) {
			*to.Mapping = next
			changed = append(changed, "model_mapping")
		}
	}
	// Scheduling and limits are checked with the caller's own rules.
	var probe input
	if patch.Priority != nil && to.Priority != nil {
		probe.Priority = new(int(patch.GetPriority()))
	}
	if patch.Weight != nil && to.Weight != nil {
		probe.Weight = new(int(patch.GetWeight()))
	}
	if patch.MaxConcurrency != nil && to.MaxConcurrency != nil {
		probe.MaxConcurrency = new(int(patch.GetMaxConcurrency()))
	}
	if patch.RpmLimit != nil && to.RPMLimit != nil {
		probe.RPMLimit = new(int(patch.GetRpmLimit()))
	}
	if patch.TpmLimit != nil && to.TPMLimit != nil {
		probe.TPMLimit = new(patch.GetTpmLimit())
	}
	if patch.SpmLimit != nil && to.SPMLimit != nil {
		probe.SPMLimit = new(int(patch.GetSpmLimit()))
	}
	if list := patch.GetModelLimits(); list != nil && to.ModelLimits != nil {
		probe.ModelLimits = new(coreModelLimits(list.GetItems()))
	}
	if fe := probe.validateScheduling(ctx); len(fe) > 0 {
		return nil, invalidPatch(fe)
	}
	if probe.ModelLimits != nil {
		keepSPMWindows(*probe.ModelLimits, *to.ModelLimits)
	}
	for _, f := range []struct {
		name     string
		from, to *int
	}{
		{"priority", probe.Priority, to.Priority}, {"weight", probe.Weight, to.Weight},
		{"max_concurrency", probe.MaxConcurrency, to.MaxConcurrency}, {"rpm_limit", probe.RPMLimit, to.RPMLimit},
		{"spm_limit", probe.SPMLimit, to.SPMLimit},
	} {
		if f.from != nil && *f.from != *f.to {
			*f.to = *f.from
			changed = append(changed, f.name)
		}
	}
	if probe.TPMLimit != nil && *probe.TPMLimit != *to.TPMLimit {
		*to.TPMLimit = *probe.TPMLimit
		changed = append(changed, "tpm_limit")
	}
	if probe.ModelLimits != nil && !slices.EqualFunc(*probe.ModelLimits, *to.ModelLimits, sameModelLimit) {
		*to.ModelLimits = *probe.ModelLimits
		changed = append(changed, "model_limits")
	}
	return changed, nil
}

func invalidPatch(fe []core.FieldError) error {
	return core.ErrPluginUnavailable.WithMessage("Plugin returned an invalid account change").
		WithDetails(map[string]any{"fields": fe})
}

// createAudit is the account.create audit detail: the subscription type the
// preparation observed and the fields the plugin changed, when any.
func createAudit(name string, bt core.AccountTypeBinding, facts core.AccountPreparationFacts, patched []string) map[string]any {
	detail := map[string]any{"name": name, "plugin_key": bt.Plugin.Key, "type": bt.Type.ID}
	if facts.SubscriptionType != "" {
		detail["subscription_type"] = facts.SubscriptionType
	}
	if len(patched) > 0 {
		detail["plugin_changed"] = patched
	}
	return detail
}
