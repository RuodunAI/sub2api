package routes

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/Sub2API-Devs/sup2api/next/sdk/manifest"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/httpapi"
)

var errTooManyAccounts = core.ErrInvalidArgument.WithMessage(fmt.Sprintf("at most %d accounts per request", manifest.MaxAccountBatch))

// authorizeAccounts checks that the caller may act on every account the
// request names (manifest.RouteAccounts) and answers the request when not.
func (h *Handler) authorizeAccounts(c *gin.Context, acc *manifest.RouteAccounts, params map[string]string, body []byte) bool {
	ids, err := accountIDs(acc.From, params, c.Request.URL.Query(), body)
	if err == nil {
		ctx := c.Request.Context()
		userID, _ := core.UserID(ctx)
		err = core.AuthorizeAccounts(ctx, h.accounts, userID, acc.Action, ids)
	}
	if err != nil {
		httpapi.Fail(c, err)
		return false
	}
	return true
}

// accountIDs collects the distinct account ids the request names under from
// (requestValues), each a decimal integer.
func accountIDs(from []string, params map[string]string, query url.Values, body []byte) ([]int64, error) {
	var ids []int64
	for _, f := range from {
		vals, err := requestValues(f, params, query, body)
		for _, v := range vals {
			id, perr := strconv.ParseInt(v, 10, 64)
			err = errors.Join(err, perr)
			ids = append(ids, id)
		}
		if err != nil {
			_, name, _ := manifest.ParseAccountsFrom(f)
			return nil, core.InvalidFields(core.FieldError{Field: name, Code: "invalid",
				Message: "must be an account id, a list of account ids or a comma-separated string of them"})
		}
	}
	slices.Sort(ids)
	if ids = slices.Compact(ids); len(ids) > manifest.MaxAccountBatch {
		return nil, errTooManyAccounts
	}
	return ids, nil
}

// requestValues reads the values the request names under one from entry
// (manifest.ParseAccountsFrom): a path parameter, every value of a query
// parameter, or top-level JSON body fields. Body field names match
// case-insensitively, as encoding/json does when the plugin decodes the
// body; a body that is no JSON object names none. Every value may be a
// comma-separated string; empty parts are skipped.
func requestValues(from string, params map[string]string, query url.Values, body []byte) ([]string, error) {
	source, name, _ := manifest.ParseAccountsFrom(from)
	switch source {
	case manifest.AccountsFromPath:
		return splitValues(params[name]), nil
	case manifest.AccountsFromQuery:
		return splitValues(query[name]...), nil
	case manifest.AccountsFromBody:
		return fieldValues(body, name)
	}
	return nil, nil
}

// fieldValues reads every top-level body field named name, in any case.
func fieldValues(body []byte, name string) ([]string, error) {
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(body, &fields)
	var out []string
	for k, raw := range fields {
		if !strings.EqualFold(k, name) {
			continue
		}
		more, err := rawValues(raw)
		if err != nil {
			return nil, err
		}
		out = append(out, more...)
	}
	return out, nil
}

var errBadValue = errors.New("not a string, a number or an array of them")

// rawValues reads a JSON string, number or array of them; null names none.
func rawValues(raw json.RawMessage) ([]string, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	items, isArray := v.([]any)
	if !isArray {
		items = []any{v}
	}
	var out []string
	for _, item := range items {
		switch x := item.(type) {
		case string:
			out = append(out, splitValues(x)...)
		case json.Number:
			out = append(out, x.String())
		case nil: // null names none
		default:
			return nil, errBadValue
		}
	}
	return out, nil
}

// splitValues splits comma-separated values; empty parts are skipped.
func splitValues(vals ...string) []string {
	var out []string
	for _, v := range vals {
		for _, part := range strings.Split(v, ",") {
			if part = strings.TrimSpace(part); part != "" {
				out = append(out, part)
			}
		}
	}
	return out
}
