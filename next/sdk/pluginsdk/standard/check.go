package standard

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/Sub2API-Devs/sup2api/next/sdk/paramoverride"
	"github.com/Sub2API-Devs/sup2api/next/sdk/pluginsdk/apikey"
)

// Check reports what makes types inconsistent: an empty or repeated id, a
// base URL that is neither valid by default nor required, a header name
// that is not a lower-case HTTP name or that only the credential may use, a
// default or test protocol without a route, an edit on a websocket route,
// an incomplete route, test or model listing. New calls it; a plugin's
// tests may too.
func Check(types ...Type) error {
	if len(types) == 0 {
		return errors.New("no account type")
	}
	var errs []error
	seen := map[string]bool{}
	for i := range types {
		t := &types[i]
		if t.ID == "" || seen[t.ID] {
			errs = append(errs, fmt.Errorf("account type %d: id %q is empty or used twice", i, t.ID))
		}
		seen[t.ID] = true
		if err := t.check(); err != nil {
			errs = append(errs, fmt.Errorf("account type %s: %w", t.ID, err))
		}
	}
	return errors.Join(errs...)
}

func (t *Type) check() error {
	var errs []error
	add := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }
	if t.Spec.DefaultBaseURL == "" && !t.Spec.RequireBaseURL {
		add("a default base URL is required unless the base URL is required")
	} else if t.Spec.DefaultBaseURL != "" {
		if _, err := apikey.NormalizeBaseURL(t.Spec.DefaultBaseURL, nil); err != nil {
			add("default base URL %v", err)
		}
	}
	if err := t.Auth.check(); err != nil {
		errs = append(errs, err)
	}
	errs = append(errs, checkHeaders("headers", t.Headers))
	if len(t.Routes) == 0 {
		add("no route")
	}
	for _, protocol := range slices.Sorted(maps.Keys(t.Routes)) {
		if err := t.checkRoute(t.Routes[protocol]); err != nil {
			errs = append(errs, fmt.Errorf("route %s: %w", protocol, err))
		}
	}
	if _, ok := t.Routes[t.DefaultProtocol]; t.DefaultProtocol != "" && !ok {
		add("default protocol %q has no route", t.DefaultProtocol)
	}
	if slices.Contains(t.FilterFields, "") {
		add("an empty filter field")
	}
	if test := t.Test; test != nil {
		if r, ok := t.Routes[test.Protocol]; !ok || r.WebSocket {
			add("test protocol %q has no http route", test.Protocol)
		}
		if strings.TrimSpace(test.Model) == "" || test.Body == nil {
			add("the test needs a model and a body")
		}
	}
	if m := t.Models; m != nil && (!strings.HasPrefix(m.Path, "/") || m.IdsPath == "") {
		add("the model listing needs a path from / and an ids path")
	}
	return errors.Join(errs...)
}

func (a Auth) check() error {
	switch {
	case a.Header != "" && a.Query != "":
		return errors.New("auth is a header or a query parameter, not both")
	case a.Header != "" && (!paramoverride.ValidHeaderName(a.Header) || strings.ToLower(a.Header) != a.Header):
		return fmt.Errorf("auth header %q is not a lower-case header name", a.Header)
	case a.Header != "" && paramoverride.HeaderBlocked(a.Header) && !paramoverride.CredentialHeader(a.Header):
		return fmt.Errorf("auth header %q cannot carry a credential", a.Header)
	}
	return nil
}

// checkHeaders: lower-case names, none a credential, hop-by-hop or client
// address header (paramoverride.HeaderBlocked) - the key is written by Auth
// only - and every write has a value.
func checkHeaders(where string, hs []Header) error {
	var errs []error
	for _, h := range hs {
		switch {
		case !paramoverride.ValidHeaderName(h.Name) || strings.ToLower(h.Name) != h.Name:
			errs = append(errs, fmt.Errorf("%s: %q is not a lower-case header name", where, h.Name))
		case paramoverride.HeaderBlocked(h.Name):
			errs = append(errs, fmt.Errorf("%s: %s cannot be written; the credential goes in Auth", where, h.Name))
		case h.Value == nil:
			errs = append(errs, fmt.Errorf("%s: %s has no value", where, h.Name))
		}
	}
	return errors.Join(errs...)
}

func (t *Type) checkRoute(r Route) error {
	var errs []error
	if r.Path == nil {
		errs = append(errs, errors.New("no path"))
	}
	if r.WebSocket && (r.Edit != nil || (r.Method != "" && r.Method != "GET")) {
		errs = append(errs, errors.New("a websocket route is opened with GET and has no body edit"))
	}
	return errors.Join(append(errs, checkHeaders("headers", r.Headers))...)
}
