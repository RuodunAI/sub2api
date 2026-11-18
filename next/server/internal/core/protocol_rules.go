package core

import (
	"context"
	"slices"
)

// OtherModels is the models entry of the ModelProtocolRule covering every
// model no other rule of the account lists.
const OtherModels = "*"

// ModelProtocolRule is one entry of an account's model_protocols: its
// (client) models are served on its inbound protocols only, whether those
// belong to a platform the account type declares or are reached through a
// cross-platform conversion. Models holds complete model ids, or OtherModels
// alone.
type ModelProtocolRule struct {
	Models    []string `json:"models"`
	Protocols []string `json:"protocols"`
}

// ProtocolRules is the protocol binding of an account compiled for the
// scheduler: which inbound protocols it serves, and for which models. It is
// built once per snapshot, never changed afterwards and shared by every copy
// of the AccountRef holding it; nil is an account without a binding.
type ProtocolRules struct {
	// own are the protocols of the account type's own platforms the account
	// serves; all of them unless restricted.
	own        []string
	restricted bool
	// models are the protocols of each model a rule lists; other those of
	// the OtherModels rule, when listed has one.
	models map[string][]string
	other  []string
	listed bool
}

// NewProtocolRules compiles an account's protocols (nil = every protocol of
// its type's own platforms) and model_protocols; nil when it has neither.
func NewProtocolRules(protocols []string, rules []ModelProtocolRule) *ProtocolRules {
	if protocols == nil && len(rules) == 0 {
		return nil
	}
	r := &ProtocolRules{own: protocols, restricted: protocols != nil}
	n := 0
	for _, rule := range rules {
		n += len(rule.Models)
	}
	if n > 0 {
		r.models = make(map[string][]string, n)
	}
	for _, rule := range rules {
		for _, m := range rule.Models {
			if m == OtherModels {
				r.other, r.listed = rule.Protocols, true
				continue
			}
			r.models[m] = rule.Protocols
		}
	}
	return r
}

// Serves reports whether the account serves the (client) model on the
// inbound protocol, else the reason the scheduler skips it for. cross says
// the request reaches the account through a cross-platform conversion: its
// type does not declare the platform of the protocol. In order:
//
//   - a model a rule lists (by name, else under OtherModels) is served on
//     the rule's protocols only (SchedModelProtocolClosed);
//   - a model no rule lists is never converted (SchedConversionNotEnabled);
//   - a protocol of the type's own platforms must be one the account serves
//     (SchedProtocolClosed).
//
// A count_tokens protocol is judged as the generation protocol of its
// family. Serves does not allocate.
func (r *ProtocolRules) Serves(model, protocol string, cross bool) (reason string, ok bool) {
	if r == nil {
		if cross {
			return SchedConversionNotEnabled, false
		}
		return "", true
	}
	if family := CountTokensFamily(protocol); family != "" {
		protocol = family
	}
	rule, listed := r.models[model]
	if !listed {
		rule, listed = r.other, r.listed
	}
	switch {
	case listed && !slices.Contains(rule, protocol):
		return SchedModelProtocolClosed, false
	case !listed && cross:
		return SchedConversionNotEnabled, false
	case !cross && r.restricted && !slices.Contains(r.own, protocol):
		return SchedProtocolClosed, false
	}
	return "", true
}

// GroupServedProtocol is an inbound protocol the accounts of a group serve,
// with how many of them do.
type GroupServedProtocol struct {
	Protocol string `json:"protocol"`
	Accounts int    `json:"accounts"`
}

// GroupProtocols (owner: account) reports what the accounts of a group
// serve, for the group's page.
type GroupProtocols interface {
	// GroupServedProtocols lists the inbound protocols the live accounts of
	// the group serve for at least one model, in the order the generation
	// lists their endpoints; never nil.
	GroupServedProtocols(ctx context.Context, groupID int64) ([]GroupServedProtocol, error)
}
