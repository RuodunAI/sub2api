package main

import (
	"errors"
	"log/slog"
	"os"
	"strings"

	"github.com/Sub2API-Devs/sup2api/next/gateway/internal/release"
)

// The environment a gateway hands to the gateway it execs (one-click
// gateway updates): the installed version it started, and the image's own
// binary with its version, which a restart after a swap goes back to.
const (
	handoffEnv        = "SUB2API_GATEWAY_HANDOFF"
	bundledEnv        = "SUB2API_GATEWAY_BUNDLED"
	bundledVersionEnv = "SUB2API_GATEWAY_BUNDLED_VERSION"
)

// execFunc replaces the process with the binary at path, checking its
// SHA-256 first when digest is set. It returns only on failure.
type execFunc func(path, digest string, argv, env []string) error

// startupHandoff runs first in serve: when <root>/gateway/current names an
// installed gateway newer than this binary, it replaces the process with
// that one. Anything else keeps this binary serving: no pointer, the
// bundled one selected, an older installed version (after an image upgrade,
// noted at info level), or, with a warning, a corrupt pointer, a binary that
// differs from its recorded digest, an exec that failed or an installed
// gateway that never reached serving (release.Manager NoteHandoff, so a
// crashing binary cannot loop through restarts).
func startupHandoff(root string, exec execFunc) {
	m := &release.Manager{Root: root}
	p, err := m.ReadGatewayPointer()
	version, err := release.HandoffTarget(p, err, Version, os.Getenv(handoffEnv))
	if err == nil && version != "" {
		if err = m.NoteHandoff(version); err == nil {
			env := setEnv(os.Environ(), handoffEnv, version)
			env = setEnv(env, bundledEnv, bundledPath())
			env = setEnv(env, bundledVersionEnv, bundledVersion())
			err = exec(m.GatewayBinary(version), p.Digest, os.Args, env)
		}
	}
	switch {
	case errors.Is(err, release.ErrStaleGateway):
		slog.Info("installed gateway is older; serving with this binary", "version", Version, "reason", err)
	case err != nil:
		slog.Warn("installed gateway not started; serving with this binary", "version", Version, "error", err)
	}
}

// restartGateway replaces the process with the bundled binary, whose
// startupHandoff starts the gateway the node selects now.
func restartGateway(exec execFunc) error {
	return exec(bundledPath(), "", os.Args, setEnv(os.Environ(), handoffEnv, ""))
}

// bundledPath is the image's own gateway binary.
func bundledPath() string {
	if p := os.Getenv(bundledEnv); p != "" {
		return p
	}
	p, err := os.Executable()
	if err != nil {
		return os.Args[0]
	}
	return p
}

// bundledVersion is the version of the image's own gateway binary.
func bundledVersion() string {
	if v := os.Getenv(bundledVersionEnv); v != "" {
		return v
	}
	return Version
}

// setEnv sets key in env, or removes it when value is empty.
func setEnv(env []string, key, value string) []string {
	out := make([]string, 0, len(env)+1)
	for _, kv := range env {
		if !strings.HasPrefix(kv, key+"=") {
			out = append(out, kv)
		}
	}
	if value != "" {
		out = append(out, key+"="+value)
	}
	return out
}
