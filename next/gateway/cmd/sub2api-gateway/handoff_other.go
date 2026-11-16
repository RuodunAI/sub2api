//go:build !linux

package main

import (
	"errors"

	"github.com/Sub2API-Devs/sup2api/next/gateway/internal/release"
)

// execGateway: a gateway replaces itself only on Linux.
func execGateway(string, string, []string, []string) error {
	return errors.New("gateway self-update needs Linux")
}

// selfUpdatable: a gateway replaces itself only on Linux.
func selfUpdatable(*release.Manager) bool { return false }
