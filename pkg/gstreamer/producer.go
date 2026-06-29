//go:build linux

package gstreamer

import (
	"encoding/json"
	"errors"
	"io"
	"net"
	"sync"

	"github.com/AlexxIT/go2rtc/pkg/core"
)

// ConnectionAccessor is a wrapped core.Producer that exposes its embedded
// *core.Connection. All magic.Open producers satisfy this through method
// promotion; producers that don't (e.g. nil, future types) fail MarshalJSON.
type ConnectionAccessor interface {
	core.Producer
	GetConnection() *core.Connection
}

// Producer wraps a core.Producer (from magic.Open) and owns the unix socket
// + pipe lifetime the gstreamer service writes to.
type Producer struct {
	wrapped     core.Producer
	conn        *net.UnixConn
	readPipe    io.Closer
	shareSocket *ShareSocket
	mu     sync.Mutex
	closed bool
}

// MarshalJSON flattens the embedded *core.Connection fields of the wrapped
// producer and appends shareSocket under "shareSocket".
func (p *Producer) MarshalJSON() ([]byte, error) {
	var conn *core.Connection

	if c, ok := p.wrapped.(ConnectionAccessor); ok {
		conn = c.GetConnection()
	} else {
		return nil, errors.New("gstreamer: wrapped producer does not expose *core.Connection")
	}

	info := &struct {
		*core.Connection
		*ShareSocket `json:"shareSocket"`
	}{
		Connection:  conn,
		ShareSocket: p.shareSocket,
	}
	return json.Marshal(info)
}

// Start is a passthrough. If the gstreamer service disappears, the pipe
// returns EOF on the first packet read, surfacing as a Start failure.
// The streams layer handles reconnect.
func (p *Producer) Start() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return errors.New("gstreamer: producer closed")
	}
	return p.wrapped.Start()
}

func (p *Producer) Stop() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil
	}
	p.closed = true

	err := p.wrapped.Stop()
	if p.readPipe != nil {
		_ = p.readPipe.Close()
	}
	if p.conn != nil {
		_ = p.conn.Close()
	}
	return err
}

func (p *Producer) GetMedias() []*core.Media {
	return p.wrapped.GetMedias()
}

func (p *Producer) GetTrack(media *core.Media, codec *core.Codec) (*core.Receiver, error) {
	return p.wrapped.GetTrack(media, codec)
}

// Compile-time check that *Producer satisfies core.Producer.
var _ core.Producer = (*Producer)(nil)
