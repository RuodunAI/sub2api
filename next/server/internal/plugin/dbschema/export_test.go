package dbschema

import (
	"context"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/store"
)

// Password exposes the derived password of a plugin's identity to the
// tests, which log in as it.
func (m *Manager) Password(pluginKey string) string { return m.password(pluginKey) }

// LegacyConstraintName exposes the relocation's constraint naming.
var LegacyConstraintName = legacyConstraintName

// Strays lists what a plugin's identity owns that it may not.
func (m *Manager) Strays(ctx context.Context, pluginKey string) ([]string, error) {
	found, err := m.strays(store.G(ctx, m.db.Gorm), pluginKey)
	names := make([]string, len(found))
	for i, s := range found {
		names[i] = s.String()
	}
	return names, err
}

// EnsureAgain is Ensure past this process's cache.
func (m *Manager) EnsureAgain(ctx context.Context, pluginKey string) error {
	return m.ensure(ctx, pluginKey, true)
}
