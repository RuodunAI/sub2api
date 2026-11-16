package authz

// The shadow comparison of the services newService makes, and the wait for
// the comparisons in flight (settle): authztest's, which TestMain
// (main_test.go, an external test file: authztest imports this package)
// hands over before the run.
var (
	testShadow ShadowOptions
	testSettle = func() {}
)

// SetTestShadow sets testShadow and testSettle.
func SetTestShadow(o ShadowOptions, wait func()) { testShadow, testSettle = o, wait }
