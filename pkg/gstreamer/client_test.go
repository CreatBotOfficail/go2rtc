//go:build linux

package gstreamer

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/stretchr/testify/require"
)

// mjpegStart is the smallest frame that makes magic.Open return non-nil.
var mjpegStart = []byte{0xFF, 0xD8, 0xFF, 0xE0, 0x00, 0x10, 'M', 'J', 'P', 'G'}

type mockServer struct {
	addr     string
	listener *net.UnixListener
	gotFD    int
	gotReq   Request
}

func startMockServer(t *testing.T) *mockServer {
	t.Helper()
	dir := t.TempDir()
	sock := filepath.Join(dir, "gstreamer.sock")

	ul, err := net.ListenUnix("unix", &net.UnixAddr{Name: sock, Net: "unix"})
	require.NoError(t, err)

	ms := &mockServer{addr: sock, listener: ul}

	go func() {
		uc, err := ul.AcceptUnix()
		if err != nil {
			return
		}
		defer uc.Close()

		// Read the request: data + optional OOB with a single FD.
		buf := make([]byte, 4096)
		oob := make([]byte, syscall.CmsgLen(4))
		n, oobn, _, _, err := uc.ReadMsgUnix(buf, oob)
		if err != nil {
			return
		}
		_ = json.Unmarshal(buf[:n], &ms.gotReq)

		if oobn > 0 {
			msgs, err := syscall.ParseSocketControlMessage(oob[:oobn])
			if err == nil {
				for _, m := range msgs {
					fds, err := syscall.ParseUnixRights(&m)
					if err == nil && len(fds) > 0 {
						ms.gotFD = fds[0]
					}
				}
			}
		}

		// If we got an FD, write some mjpeg bytes to it so magic.Open succeeds.
		if ms.gotFD != 0 {
			_ = syscall.SetNonblock(ms.gotFD, false)
			f := os.NewFile(uintptr(ms.gotFD), "remote")
			_, _ = f.Write(mjpegStart)
			_ = f.Close()
		}

		// Reply ok.
		resp, _ := json.Marshal(response{Status: "ok"})
		_, _, _ = uc.WriteMsgUnix(resp, nil, nil)
	}()

	return ms
}

func (ms *mockServer) Close() {
	_ = ms.listener.Close()
	_ = os.Remove(ms.addr)
}

func TestNewProducer_HappyPath(t *testing.T) {
	ms := startMockServer(t)
	defer ms.Close()

	p, err := NewProducer(ms.addr, Request{
		Action: "start",
		Result: "fakesrc ! fdsink",
		Share:  map[string]string{"foo": "fakesrc ! interpipesink name=foo"},
	})
	require.NoError(t, err)
	require.NotNil(t, p)

	// Verify the request the server received.
	require.Equal(t, "start", ms.gotReq.Action)
	require.Equal(t, "fakesrc ! fdsink", ms.gotReq.Result)
	require.Equal(t, "fakesrc ! interpipesink name=foo", ms.gotReq.Share["foo"])

	// Verify the wrapped producer exposes the mjpeg codec.
	medias := p.GetMedias()
	require.NotEmpty(t, medias)
	require.NotNil(t, medias[0])
	hasJPEG := false
	for _, c := range medias[0].Codecs {
		if c.Name == "JPEG" {
			hasJPEG = true
		}
	}
	require.True(t, hasJPEG, "expected mjpeg codec to be detected")

	require.NoError(t, p.Stop())
}

func TestNewProducer_ServerError(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, "gstreamer.sock")

	ul, err := net.ListenUnix("unix", &net.UnixAddr{Name: sock, Net: "unix"})
	require.NoError(t, err)
	defer ul.Close()
	defer os.Remove(sock)
	go func() {
		uc, err := ul.AcceptUnix()
		if err != nil {
			return
		}
		defer uc.Close()

		buf := make([]byte, 4096)
		n, _, _, _, _ := uc.ReadMsgUnix(buf, nil)
		_ = n
		resp, _ := json.Marshal(response{Status: "error", Error: "boom"})
		_, _, _ = uc.WriteMsgUnix(resp, nil, nil)
	}()

	p, err := NewProducer(sock, Request{Action: "start", Result: "fakesrc ! fdsink"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "boom")
	require.Nil(t, p)
}

func TestNewProducer_EmptySocket(t *testing.T) {
	_, err := NewProducer("", Request{Action: "start", Result: "x"})
	require.Error(t, err)
}

func TestNewProducer_EmptyResult(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, "gstreamer.sock")
	ul, err := net.ListenUnix("unix", &net.UnixAddr{Name: sock, Net: "unix"})
	require.NoError(t, err)
	defer ul.Close()
	defer os.Remove(sock)

	_, err = NewProducer(sock, Request{Action: "start"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "empty result")
}

func TestRequest_Shape(t *testing.T) {
	// Sanity: the JSON shape is what the external service expects.
	req := Request{Action: "start", Result: "fdsink", Share: map[string]string{"a": "b"}}

	out, err := json.Marshal(req)
	require.NoError(t, err)
	require.Equal(t, `{"action":"start","result":"fdsink","share":{"a":"b"}}`, string(out))
}
