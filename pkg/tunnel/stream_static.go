package tunnel

import (
	"context"
	"fmt"

	bridgev1 "github.com/vercel/bridge/api/go/bridge/v1"
)

type rawStream interface {
	Send(*bridgev1.TunnelNetworkMessage) error
	Recv() (*bridgev1.TunnelNetworkMessage, error)
}

type staticStream struct {
	stream rawStream
}

// NewStaticStream wraps a non-refreshable stream so it satisfies Stream.
func NewStaticStream(stream rawStream) Stream {
	return &staticStream{stream: stream}
}

func (s *staticStream) Send(msg *bridgev1.TunnelNetworkMessage) error {
	return s.stream.Send(msg)
}

func (s *staticStream) Recv() (*bridgev1.TunnelNetworkMessage, error) {
	return s.stream.Recv()
}

func (s *staticStream) Refresh(context.Context) error {
	return fmt.Errorf("stream refresh not supported")
}
