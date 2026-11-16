package testutil

import (
	"strconv"
	"strings"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
)

// SessionUser reads a test console token "<prefix><uid>" as a session of
// user uid, for the core.TokenVerifier fakes. Any other token is
// unauthenticated, as iam answers a token it does not know: a plain error
// would be a server error, which httpapi.Authenticate passes through.
func SessionUser(token, prefix string) (int64, string, error) {
	uid, err := strconv.ParseInt(strings.TrimPrefix(token, prefix), 10, 64)
	if err != nil {
		return 0, "", core.ErrUnauthenticated
	}
	return uid, "", nil
}
