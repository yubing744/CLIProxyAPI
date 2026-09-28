package logging

import (
	"context"
	"net"
	"net/netip"
	"strings"

	"github.com/google/uuid"
)

type transportConnectionKey struct{}

// TransportConnection identifies only an accepted connection, not a person.
// No address, port, forwarded header or persistent identifier is retained.
type TransportConnection struct {
	ID, Network, PeerClass string
}

// ConnectionContext is wired to http.Server.ConnContext at the accept boundary.
// It uses immutable context values, with no global connection registry.
func ConnectionContext(ctx context.Context, conn net.Conn) context.Context {
	meta := TransportConnection{ID: strings.ReplaceAll(uuid.NewString(), "-", ""), Network: "other", PeerClass: "non_ip"}
	addr := conn.RemoteAddr()
	if addr != nil {
		switch addr.Network() {
		case "tcp", "tcp4", "tcp6":
			meta.Network = "tcp"
		case "unix", "unixpacket":
			meta.Network = "unix"
		case "pipe":
			meta.Network = "pipe"
		}
		if host, _, err := net.SplitHostPort(addr.String()); err == nil {
			if ip, err := netip.ParseAddr(host); err == nil {
				ip = ip.Unmap()
				switch {
				case ip.IsLoopback():
					meta.PeerClass = "loopback"
				case ip.IsPrivate() || ip.IsLinkLocalUnicast():
					meta.PeerClass = "private"
				default:
					meta.PeerClass = "other_ip"
				}
			}
		}
	}
	return context.WithValue(ctx, transportConnectionKey{}, meta)
}

func GetTransportConnection(ctx context.Context) TransportConnection {
	if ctx != nil {
		if meta, ok := ctx.Value(transportConnectionKey{}).(TransportConnection); ok {
			return meta
		}
	}
	return TransportConnection{ID: "missing", Network: "missing", PeerClass: "missing"}
}
