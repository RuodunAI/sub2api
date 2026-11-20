package dbschema

import (
	"context"
	"net"
	"net/url"

	"github.com/Sub2API-Devs/sup2api/next/runtime-contract/gormpg/pgproxy"
	"github.com/Sub2API-Devs/sup2api/next/sdk/protocol"
)

// tunnelDSN is the DSN every plugin gets: the virtual database address of
// the egress tunnel, no password. The user and database name only satisfy
// the client's DSN parser; the core logs in as the plugin's identity.
func tunnelDSN(schema string) string {
	u := url.URL{
		Scheme:   "postgres",
		User:     url.User("plugin"),
		Host:     protocol.DatabaseAddress,
		Path:     "/" + schema,
		RawQuery: "sslmode=disable",
	}
	return u.String()
}

// ServeTunnel serves one connection of plugin pluginKey to the virtual
// database address (egress.Options.DatabaseProxy): it ensures the plugin's
// database side, logs in to PostgreSQL as the plugin's identity with the
// core's host configuration and the core's schema as search_path, and
// relays the protocol (pgproxy).
func (m *Manager) ServeTunnel(ctx context.Context, conn net.Conn, pluginKey string) {
	m.proxyOnce.Do(func() { m.proxy, m.proxyErr = pgproxy.New(m.databaseURL) })
	err := m.proxyErr
	if err == nil {
		err = m.Ensure(ctx, pluginKey)
	}
	if err != nil {
		_ = conn.Close()
		m.log.Error("plugin database connection", "plugin", pluginKey, "err", err)
		return
	}
	err = m.proxy.Serve(ctx, conn, pgproxy.Session{
		Client: pluginKey, User: m.Identity(pluginKey), Password: m.password(pluginKey),
		Params: map[string]string{
			"search_path":      m.schema,
			"application_name": "sub2api plugin " + pluginKey,
		},
	})
	if err != nil && ctx.Err() == nil {
		m.log.Warn("plugin database connection", "plugin", pluginKey, "err", err)
	}
}
