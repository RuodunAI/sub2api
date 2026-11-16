//go:build linux

package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"syscall"

	"github.com/Sub2API-Devs/sup2api/next/gateway/internal/release"
)

// execGateway is the execFunc of Linux. The binary is hashed through the
// descriptor that is then executed (/proc/self/fd), so the file checked is
// the file run, whatever happens to its path in between.
func execGateway(path, digest string, argv, env []string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	if digest != "" {
		h := sha256.New()
		if _, err = io.Copy(h, f); err != nil {
			return err
		}
		if hex.EncodeToString(h.Sum(nil)) != digest {
			return errors.New("installed gateway binary differs from its recorded digest")
		}
	}
	return syscall.Exec(fmt.Sprintf("/proc/self/fd/%d", f.Fd()), argv, env)
}

// selfUpdatable reports whether this node can install gateway binaries and
// exec them.
func selfUpdatable(m *release.Manager) bool { return m.GatewayWritable() }
