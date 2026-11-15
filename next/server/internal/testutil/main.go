package testutil

import (
	"fmt"
	"os"
)

// Exit ends a test binary (TestMain) with code, the result of m.Run (or
// registrytest.Run), or 1 when the run passed but a check reports an error:
// a condition the tests leave behind that no single test sees, such as
// authz.ShadowCheck.
func Exit(code int, checks ...func() error) {
	for _, check := range checks {
		if err := check(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			if code == 0 {
				code = 1
			}
		}
	}
	os.Exit(code)
}
