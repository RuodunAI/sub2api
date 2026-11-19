package pluginsdktest

import (
	"context"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	pluginv1 "github.com/Sub2API-Devs/sup2api/next/sdk/gen/pluginv1"
)

// currency is the form of BalanceResult.currency the host accepts.
var currency = regexp.MustCompile(`^[A-Z]{3}$`)

// Balance makes one balance query of account the way the host does
// (CONTRACTS §52): BuildBalanceRequest, then every request's URL checked
// against the hosts the manifest allows for the account
// (manifest.Manifest.UpstreamHostAllowed, as for a relay) before any is
// answered, then answer for each request in order (the host sends them
// all, even after a failure, and follows no redirect: what answer returns
// is handed over as it is; a nil answer is a transport error), then
// ParseBalanceResponse with the answers in the same order.
//
// The error is the plugin's (UNIMPLEMENTED for an account type without a
// balance), FAILED_PRECONDITION for an answer without requests or a result
// the host records as a transient error (a success without unlimited and
// without an ISO 4217 code), or PERMISSION_DENIED for a request the host
// refuses to send: then answer is never called and nothing is parsed. The
// SSRF policy and the account's proxy are not simulated. A request's
// method defaults to GET, as on the host.
func (h *Harness) Balance(ctx context.Context, account *pluginv1.Account, answer func(*pluginv1.BalanceRequest) *pluginv1.BalanceAnswer) (*pluginv1.BalanceResult, error) {
	built, err := h.Platform.BuildBalanceRequest(ctx, &pluginv1.BuildBalanceRequestRequest{Account: account})
	if err != nil {
		return nil, err
	}
	requests := built.GetRequests()
	if len(requests) == 0 {
		return nil, status.Error(codes.FailedPrecondition, "BuildBalanceRequest returned no request")
	}
	for _, r := range requests {
		if err := h.Host.admitBalance(r, account); err != nil {
			return nil, err
		}
	}
	answers := make([]*pluginv1.BalanceAnswer, len(requests))
	for i, r := range requests {
		r = proto.Clone(r).(*pluginv1.BalanceRequest)
		if r.Method == "" {
			r.Method = http.MethodGet
		}
		if answers[i] = answer(r); answers[i] == nil {
			answers[i] = &pluginv1.BalanceAnswer{TransportError: "pluginsdktest: no answer"}
		}
	}
	parsed, err := h.Platform.ParseBalanceResponse(ctx, &pluginv1.ParseBalanceResponseRequest{Account: account, Answers: answers})
	if err != nil {
		return nil, err
	}
	res := parsed.GetResult()
	if res.GetErrorType() == pluginv1.BalanceResult_ERROR_TYPE_UNSPECIFIED && !res.GetUnlimited() && !currency.MatchString(res.GetCurrency()) {
		return res, status.Errorf(codes.FailedPrecondition, "a balance without an ISO 4217 currency (%q): the host records it as a transient error", res.GetCurrency())
	}
	return res, nil
}

// admitBalance is the host's check of a balance request before it sends
// any: an absolute http(s) URL to a host the manifest allows for account.
func (f *FakeHost) admitBalance(r *pluginv1.BalanceRequest, account *pluginv1.Account) error {
	u, err := url.Parse(r.GetUrl())
	switch {
	case err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "":
		return status.Errorf(codes.PermissionDenied, "balance url %q is not an absolute http(s) URL", r.GetUrl())
	case !f.upstreamHostAllowed(strings.ToLower(u.Hostname()), account):
		return status.Errorf(codes.PermissionDenied, "balance host %q is neither one of the manifest's externalServices nor of the account's upstream addresses", u.Hostname())
	}
	return nil
}
