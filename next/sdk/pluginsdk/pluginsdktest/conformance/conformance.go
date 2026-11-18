// Package conformance is the conformance suite every platform plugin runs
// from its own tests (DESIGN-2026-11-18-PLUGIN-FORWARDING-V2 §14.1): one
// call, Run, with a Plugin and its sample Cases. Each rule is a named
// subtest; a failure names the case, what was observed and the rule with
// the section of the design that states it. See README.md.
package conformance

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	pluginv1 "github.com/Sub2API-Devs/sup2api/next/sdk/gen/pluginv1"
	"github.com/Sub2API-Devs/sup2api/next/sdk/pluginsdk"
	"github.com/Sub2API-Devs/sup2api/next/sdk/pluginsdk/pluginsdktest"
)

// Plugin is the plugin under test and what the suite needs to know about it.
type Plugin struct {
	// Platform is the implementation under test. The SDK finds TaskMonitor,
	// UsageExtractor and the rest on it by type assertion, as in production.
	Platform pluginsdk.Platform
	// SDK are the options of pluginsdk.Register: WithManifest (or WithInfo).
	// The declared capabilities decide whether the WebSocket rule applies.
	SDK []pluginsdk.Option
	// Config is the plugin settings sent with Configure (nil = "{}").
	Config any
	// Protocols are the upstream protocols the plugin declares. Each needs a
	// stream and a non-stream HTTP case.
	Protocols []string
	// PassHeaders is the client header allow-list (the manifest's
	// passHeaders). The suite hands the plugin each of them plus one header
	// outside the list, which must not reach the upstream request.
	PassHeaders []string
	// Credentials are the credential and signed header names (lower-case):
	// they must be authoritative, never default headers.
	Credentials []string
	// Authoritative are further headers the plugin keeps authoritative, each
	// with the reason (e.g. today's code-plugin behaviour the conversion must
	// keep). Every other header must be a default header.
	Authoritative map[string]string
	// Fetch answers the plugin's auxiliary fetches (token exchange, upload).
	// nil answers UNIMPLEMENTED.
	Fetch func(context.Context, *pluginv1.ExecutionHTTPRequest) (*pluginv1.ExecutionHTTPResponse, error)
	// FixClock sets the plugin's clock to now; the suite calls it before
	// every Execute. Required when the request depends on the time (a
	// signature), or the determinism and failover rules fail.
	FixClock func(now time.Time)
	// BodyLimit is the request body size of the large-body rule
	// (default 8 MiB).
	BodyLimit int
	// CallTimeout is the platform call timeout of the cancellation rule
	// (default 2s, the setting's default).
	CallTimeout time.Duration
	// Exceptions are the rule violations the plugin justifies.
	Exceptions []Exception
}

// Endpoint is how the host executes the endpoint of a case.
type Endpoint int

const (
	// Ordinary relays without waiting and returns host_usage.
	Ordinary Endpoint = iota
	// PluginUsage is an endpoint with usageSource "plugin" (extract_usage):
	// relay and wait, return usage.
	PluginUsage
	// TaskSubmission is an endpoint with task.action "submit"
	// (task_submission): relay and wait, return task.
	TaskSubmission
)

// Case is one sample request of the plugin.
type Case struct {
	// Name names the case in failures and exceptions; default
	// "<protocol> stream|non-stream" (" websocket" for a turn).
	Name string
	// Account is a sample account of the type, with credentials and
	// settings JSON.
	Account *pluginv1.Account
	// Meta is the request meta: protocol, model, stream, endpoint ...
	Meta *pluginv1.RequestMeta
	// Body is the request body as the host hands it over (upstream
	// protocol, rewrites done); for a WebSocket turn the message.
	Body []byte
	// Endpoint is how the host executes the case.
	Endpoint Endpoint
	// WebSocket marks one turn of a WebSocket endpoint.
	WebSocket bool
	// UpstreamModel is the model the plugin is declared to map the incoming
	// one to; empty means unchanged.
	UpstreamModel string
	// Observed is the upstream answer of the waited relay (PluginUsage and
	// TaskSubmission cases), e.g. pluginsdktest.Observed(200, body).
	Observed *pluginv1.RelayUpstreamResponse
	// Failover is the account of the failover attempt; nil derives one from
	// Account (other id, every top-level credential string suffixed). Set it when
	// the credentials have structure a suffix breaks.
	Failover *pluginv1.Account
}

// Exception excuses one case (or suite item) from one rule. It needs a
// reason, and fails once the rule passes without it.
type Exception struct {
	// Check is the subtest name of the rule, e.g. "03_body".
	Check string
	// Case is the case or item name as the failure reports it.
	Case string
	// Reason says why the plugin cannot follow the rule there.
	Reason string
}

const (
	defaultBodyLimit   = 8 << 20
	defaultCallTimeout = 2 * time.Second
	// executeTimeout bounds every Execute of the suite.
	executeTimeout = 30 * time.Second
)

// fixedTime is the clock FixClock sets.
var fixedTime = time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)

// Run runs every rule against p with the sample cases, each as a subtest
// of t. Do not run it in parallel with tests that use the network: for its
// duration the process's default transport and resolver fail every dial.
func Run(t *testing.T, p Plugin, cases ...Case) {
	t.Helper()
	run(t, p, cases, func(name string, fn func(testing.TB)) {
		t.Run(name, func(t *testing.T) { fn(t) })
	})
}

// run is Run with the subtest runner supplied, so the self-test can record
// which rule failed instead of failing.
func run(t testing.TB, p Plugin, cases []Case, sub func(name string, fn func(testing.TB))) {
	t.Helper()
	s, err := newSuite(p, cases)
	if err != nil {
		t.Fatalf("conformance: %v", err)
	}
	defer s.sockets.install()()
	for _, c := range checks {
		sub(c.name, func(t testing.TB) { s.check(t, c) })
	}
}

// suite is one Run: the input, completed with defaults and checked.
type suite struct {
	p          Plugin
	cases      []*Case
	inbound    map[string]string
	exceptions map[itemKey]Exception
	sockets    *sockets
}

type itemKey struct{ check, item string }

func newSuite(p Plugin, cases []Case) (*suite, error) {
	s := &suite{p: p, inbound: inboundHeaders(p.PassHeaders), exceptions: map[itemKey]Exception{}, sockets: &sockets{}}
	if s.p.BodyLimit <= 0 {
		s.p.BodyLimit = defaultBodyLimit
	}
	if s.p.CallTimeout <= 0 {
		s.p.CallTimeout = defaultCallTimeout
	}
	errs := []error{s.checkPlugin()}
	names := map[string]bool{}
	for i := range cases {
		c := cases[i]
		if c.Name == "" {
			c.Name = defaultName(&c)
		}
		if names[c.Name] {
			errs = append(errs, fmt.Errorf("two cases are named %q", c.Name))
		}
		names[c.Name] = true
		errs = append(errs, checkCase(&c))
		s.cases = append(s.cases, &c)
	}
	errs = append(errs, s.indexExceptions())
	return s, errors.Join(errs...)
}

func (s *suite) checkPlugin() error {
	var errs []error
	if s.p.Platform == nil {
		errs = append(errs, errors.New("Plugin.Platform is required"))
	}
	if len(s.p.Protocols) == 0 {
		errs = append(errs, errors.New("Plugin.Protocols must list the declared upstream protocols"))
	}
	for name, reason := range s.p.Authoritative {
		if reason == "" {
			errs = append(errs, fmt.Errorf("authoritative header %q needs a reason", name))
		}
	}
	return errors.Join(errs...)
}

func checkCase(c *Case) error {
	var errs []error
	if c.Account == nil || c.Meta.GetProtocol() == "" || len(c.Body) == 0 {
		errs = append(errs, fmt.Errorf("case %q needs Account, Meta with a protocol and Body", c.Name))
	}
	if c.Endpoint != Ordinary && c.Observed == nil {
		errs = append(errs, fmt.Errorf("case %q: a %s case needs Observed, the upstream answer of its waited relay", c.Name, c.Endpoint))
	}
	if c.WebSocket && c.Endpoint != Ordinary {
		errs = append(errs, fmt.Errorf("case %q: a WebSocket turn is an Ordinary endpoint", c.Name))
	}
	return errors.Join(errs...)
}

func defaultName(c *Case) string {
	name := c.Meta.GetProtocol() + " non-stream"
	if c.Meta.GetStream() {
		name = c.Meta.GetProtocol() + " stream"
	}
	if c.WebSocket {
		name += " websocket"
	}
	return name
}

// indexExceptions checks every exception names a rule and one of its items.
func (s *suite) indexExceptions() error {
	known := map[itemKey]bool{}
	for _, c := range checks {
		for _, it := range c.items(s) {
			known[itemKey{c.name, it.name}] = true
		}
	}
	var errs []error
	for _, e := range s.p.Exceptions {
		k := itemKey{e.Check, e.Case}
		switch {
		case e.Reason == "":
			errs = append(errs, fmt.Errorf("exception %s/%s has no reason", e.Check, e.Case))
		case !known[k]:
			errs = append(errs, fmt.Errorf("exception %s/%s names no rule and case of the suite", e.Check, e.Case))
		}
		s.exceptions[k] = e
	}
	return errors.Join(errs...)
}

func (e Endpoint) String() string {
	return [...]string{"ordinary", "plugin-usage", "task-submission"}[e]
}

// foreignHeader is the client header outside passHeaders the suite hands
// over; it must not reach the upstream request.
const foreignHeader, foreignValue = "x-conformance-not-passed", "conformance-not-passed-value"

func inboundHeaders(pass []string) map[string]string {
	h := map[string]string{foreignHeader: foreignValue}
	for _, name := range pass {
		h[name] = passValue(name)
	}
	return h
}

func passValue(name string) string { return "conformance-client-" + name }

// request is the ExecuteRequest of c as the host builds it.
func (s *suite) request(c *Case) *pluginv1.ExecuteRequest {
	return &pluginv1.ExecuteRequest{
		Meta:           proto.Clone(c.Meta).(*pluginv1.RequestMeta),
		Account:        c.Account,
		Body:           c.Body,
		InboundHeaders: s.inbound,
		ExtractUsage:   c.Endpoint == PluginUsage,
		TaskSubmission: c.Endpoint == TaskSubmission,
	}
}

// execute makes one Execute of c (edit, when set, changes the request) at
// the fixed time.
func (s *suite) execute(ctx context.Context, h *pluginsdktest.Harness, c *Case, edit func(*pluginv1.ExecuteRequest)) *pluginsdktest.Execution {
	in := s.request(c)
	if edit != nil {
		edit(in)
	}
	if s.p.FixClock != nil {
		s.p.FixClock(fixedTime)
	}
	return h.Execute(ctx, in)
}

// once starts a harness for c and executes it once.
func (s *suite) once(t testing.TB, c *Case) *pluginsdktest.Execution {
	return s.run(s.start(t, c, upstream{}), c, nil)
}

// run executes c on h within executeTimeout.
func (s *suite) run(h *pluginsdktest.Harness, c *Case, edit func(*pluginv1.ExecuteRequest)) *pluginsdktest.Execution {
	ctx, cancel := context.WithTimeout(context.Background(), executeTimeout)
	defer cancel()
	return s.execute(ctx, h, c, edit)
}
