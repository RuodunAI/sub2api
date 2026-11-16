package testpg

import (
	"os"
	"path/filepath"
	"testing"
)

// A transient server stops with the release of its last Acquire, not with
// the first (review 1116 F7). No server runs here: stop finds none and only
// forgets the users, which is what the test watches.
func TestLeaveStopsAfterLastUser(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	dir, err := Dir()
	if err != nil {
		t.Fatal(err)
	}
	users := filepath.Join(dir, usersDir)
	if err := os.MkdirAll(users, 0o700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(dir, transientFile)
	if err := os.WriteFile(marker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	a, b := filepath.Join(users, "1-a"), filepath.Join(users, "2-b")
	for _, u := range []string{a, b} {
		if err := os.WriteFile(u, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := leave(a); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatal("stopped while another Acquire still uses the server")
	}
	if err := leave(b); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("the last release did not stop the server")
	}
	// A server a test used is no longer transient: a release leaves it.
	if err := os.MkdirAll(users, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(a, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := leave(a); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(users); err != nil {
		t.Fatal("stopped a server that is not transient")
	}
}
