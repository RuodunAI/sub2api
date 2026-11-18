package conformance

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/Sub2API-Devs/sup2api/next/sdk/manifest"
	"github.com/Sub2API-Devs/sup2api/next/sdk/platforms"
)

// declared is what the suite reads from the plugin's manifest: its account
// types, and the platforms their protocols belong to (the built-in ones
// and the manifest's own).
type declared struct {
	m         manifest.Manifest
	platforms []manifest.Platform
}

func readManifest(raw []byte) (*declared, error) {
	if len(raw) == 0 {
		return nil, errors.New("Plugin.Manifest is required: the plugin's manifest.json")
	}
	d := &declared{}
	if err := json.Unmarshal(raw, &d.m); err != nil {
		return nil, fmt.Errorf("Plugin.Manifest: %w", err)
	}
	d.platforms = append(platforms.Builtin(), d.m.Platforms...)
	return d, nil
}

// platformOf is the platform with an endpoint of protocol, nil for none.
func (d *declared) platformOf(protocol string) *manifest.Platform {
	for i := range d.platforms {
		for _, e := range d.platforms[i].Endpoints {
			if e.Protocol == protocol {
				return &d.platforms[i]
			}
		}
	}
	return nil
}

// clientHeaders are the client headers the core hands over with a request
// of protocol on an account of accountType: the passHeaders of the type's
// entry for the protocol's platform, else the platform's
// (manifest.AccountPlatform.ClientHeaders, as the gateway filters them).
func (d *declared) clientHeaders(accountType, protocol string) ([]string, error) {
	p := d.platformOf(protocol)
	if p == nil {
		return nil, fmt.Errorf("protocol %q belongs to no endpoint of a built-in platform or of the manifest", protocol)
	}
	for _, at := range d.m.AccountTypes {
		if at.ID != accountType {
			continue
		}
		for _, ap := range at.Platforms {
			if ap.Platform == p.ID {
				return ap.ClientHeaders(p), nil
			}
		}
		return nil, fmt.Errorf("account type %q of the manifest does not serve platform %s (protocol %q)", accountType, p.ID, protocol)
	}
	return nil, fmt.Errorf("the manifest declares no account type %q", accountType)
}
