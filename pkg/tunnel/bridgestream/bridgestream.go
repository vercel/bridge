// Package bridgestream carries a tunnel over a bridge TunnelNetwork stream,
// translating its TunnelNetworkMessages to and from tunnel Messages.
//
// Every TunnelNetworkMessage addresses its conn, the first one to an unknown
// conn opens it, and one with an error ends it. The stream has no half-close
// and no flow control.
package bridgestream

import (
	"log/slog"
	"sync"

	"github.com/puzpuzpuz/xsync/v3"
	bridgev1 "github.com/vercel/bridge/api/go/bridge/v1"
	"github.com/vercel/bridge/pkg/tunnel"
)

// GRPCStream is a TunnelNetwork stream. Both the server-side
// BidiStreamingServer and the client-side TunnelNetworkClient satisfy it.
type GRPCStream interface {
	Send(*bridgev1.TunnelNetworkMessage) error
	Recv() (*bridgev1.TunnelNetworkMessage, error)
}

// New adapts stream to a tunnel.Stream.
func New(stream GRPCStream) tunnel.Stream {
	return &adapter{stream: stream, conns: xsync.NewMapOf[string, *tunnel.ConnInfo]()}
}

type adapter struct {
	stream GRPCStream
	sendMu sync.Mutex
	// Each open conn's addresses, as this side sends them.
	conns *xsync.MapOf[string, *tunnel.ConnInfo]
	// The Data that came with an Open, for the next Recv. Only Recv touches it.
	pending *tunnel.Message
}

func (a *adapter) Features() tunnel.Features { return tunnel.Features{} }

func (a *adapter) Recv() (*tunnel.Message, error) {
	if m := a.pending; m != nil {
		a.pending = nil
		return m, nil
	}
	for {
		msg, err := a.stream.Recv()
		if err != nil {
			return nil, err
		}
		id := msg.GetConnectionId()
		_, open := a.conns.Load(id)
		if msg.GetError() != "" {
			if open {
				a.conns.Delete(id)
				return &tunnel.Message{Kind: tunnel.MessageReset, ConnID: id, Reason: msg.GetError()}, nil
			}
			continue
		}
		data := &tunnel.Message{Kind: tunnel.MessageData, ConnID: id, Payload: msg.GetData()}
		if open {
			return data, nil
		}
		dest := msg.GetDest()
		if dest == nil {
			slog.Info("Tunnel: ignoring message with no dest", "conn_id", id)
			continue
		}
		info := &tunnel.ConnInfo{
			Source:   address(msg.GetSource()),
			Dest:     address(dest),
			Hostname: msg.GetHostname(),
		}
		a.conns.Store(id, info.Reply())
		if len(data.Payload) > 0 {
			a.pending = data
		}
		return &tunnel.Message{Kind: tunnel.MessageOpen, ConnID: id, Conn: info}, nil
	}
}

func (a *adapter) Send(m *tunnel.Message) error {
	switch m.Kind {
	case tunnel.MessageOpen:
		// The conn's first Data opens it.
		a.conns.Store(m.ConnID, m.Conn)
		return nil
	case tunnel.MessageData:
		info, ok := a.conns.Load(m.ConnID)
		if !ok {
			return nil
		}
		return a.send(&bridgev1.TunnelNetworkMessage{
			ConnectionId: m.ConnID,
			Source:       tunnelAddress(info.Source),
			Dest:         tunnelAddress(info.Dest),
			Hostname:     info.Hostname,
			Data:         m.Payload,
		})
	case tunnel.MessageCloseWrite:
		// No half-close: the conn ends here, and the peer isn't told.
		a.conns.Delete(m.ConnID)
		return nil
	case tunnel.MessageReset:
		a.conns.Delete(m.ConnID)
		return a.send(&bridgev1.TunnelNetworkMessage{ConnectionId: m.ConnID, Error: m.Reason})
	default:
		return nil
	}
}

// send serializes Sends, which the stream doesn't take concurrently.
func (a *adapter) send(msg *bridgev1.TunnelNetworkMessage) error {
	a.sendMu.Lock()
	defer a.sendMu.Unlock()
	return a.stream.Send(msg)
}

func address(a *bridgev1.TunnelAddress) tunnel.Addr {
	return tunnel.Addr{Host: a.GetIp(), Port: int(a.GetPort())}
}

func tunnelAddress(a tunnel.Addr) *bridgev1.TunnelAddress {
	return &bridgev1.TunnelAddress{Ip: a.Host, Port: int32(a.Port)}
}
