//go:build !linux

package gstreamer

import (
	"errors"
	"net"
	"sync"

	"github.com/AlexxIT/go2rtc/pkg/core"
)

// Producer is a stub on non-linux; NewProducer always errors here.
type Producer struct {
	wrapped core.Producer
	conn    *net.UnixConn
	mu      sync.Mutex
}

func (*Producer) Start() error                { return errors.New("gstreamer: not supported on this platform") }
func (*Producer) Stop() error                 { return nil }
func (*Producer) GetMedias() []*core.Media    { return nil }
func (*Producer) GetTrack(*core.Media, *core.Codec) (*core.Receiver, error) {
	return nil, errors.New("gstreamer: not supported on this platform")
}

func NewProducer(string, *ShareSocket) (*Producer, error) {
	return nil, errors.New("gstreamer: socket mode is only supported on linux")
}
