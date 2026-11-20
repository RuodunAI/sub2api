package dbschema

// Password exposes the derived password of a plugin's identity to the
// tests, which log in as it.
func (m *Manager) Password(pluginKey string) string { return m.password(pluginKey) }

// LegacyConstraintName exposes the relocation's constraint naming.
var LegacyConstraintName = legacyConstraintName
