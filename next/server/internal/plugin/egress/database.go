package egress

import (
	"context"
	"net"
)

// serveDatabase hands one tunnelled connection to the virtual database
// address (protocol.DatabaseAddress, the DSN of HostService.GetDSN) to the
// database proxy and returns the end the tunnel pumps. The core logs in to
// PostgreSQL as the plugin's database identity and relays the protocol
// (Options.DatabaseProxy, CONTRACTS §130), so the plugin holds no database
// credential.
func (s *server) serveDatabase(ctx context.Context) net.Conn {
	tunnel, proxy := net.Pipe()
	go s.p.opts.DatabaseProxy(ctx, proxy, s.key)
	return tunnel
}
