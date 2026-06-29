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
	resp     Response
}

func startMockServer(t *testing.T, opts ...func(*mockServer)) *mockServer {
	t.Helper()
	dir := t.TempDir()
	sock := filepath.Join(dir, "gstreamer.sock")

	ul, err := net.ListenUnix("unix", &net.UnixAddr{Name: sock, Net: "unix"})
	require.NoError(t, err)

	ms := &mockServer{addr: sock, listener: ul, resp: Response{Status: "ok"}}
	for _, opt := range opts {
		opt(ms)
	}

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

		// Only on success do we feed the read pipe with mjpeg bytes; on
		// error responses, magic.Open should not be able to produce.
		if ms.gotFD != 0 && ms.resp.Status == "ok" {
			_ = syscall.SetNonblock(ms.gotFD, false)
			f := os.NewFile(uintptr(ms.gotFD), "remote")
			_, _ = f.Write(mjpegStart)
			_ = f.Close()
		}

		resp, _ := json.Marshal(ms.resp)
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

	rawURL := "gstreamer:device?video=/dev/video0#socket"
	shareSocket := &ShareSocket{
		Unixsocket: ms.addr,
		Result:     "fakesrc ! fdsink",
		Share:      map[string]string{"foo": "fakesrc ! interpipesink name=foo"},
	}
	p, err := NewProducer(rawURL, shareSocket)
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
	ms := startMockServer(t, func(m *mockServer) {
		m.resp = Response{Status: "error", Error: "boom"}
	})
	defer ms.Close()

	p, err := NewProducer("", &ShareSocket{Unixsocket: ms.addr, Result: "fakesrc ! fdsink"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "boom")
	require.Nil(t, p)
}

// Regression: /api/streams used to return producers: [{}] for gstreamer
// sources because the wrapper had no MarshalJSON. MarshalJSON must
// delegate to the wrapped producer so medias/format/etc. are visible.
func TestProducer_MarshalJSON_DelegatesToWrapped(t *testing.T) {
	ms := startMockServer(t)
	defer ms.Close()

	rawURL := "gstreamer:device?video=/dev/video0#socket"
	shareSocket := &ShareSocket{
		Unixsocket: ms.addr,
		Result:     "fakesrc ! fdsink",
		Share:      map[string]string{"foo": "fakesrc ! interpipesink name=foo"},
	}
	p, err := NewProducer(rawURL, shareSocket)
	require.NoError(t, err)
	require.NotNil(t, p)
	defer p.Stop()

	out, err := json.Marshal(p)
	require.NoError(t, err)
	s := string(out)
	require.Contains(t, s, `"format_name":"mjpeg"`)
	require.Contains(t, s, `"medias":["video, recvonly, JPEG"]`)
	require.Contains(t, s, `"protocol":"unixsocket+pipe"`)
	require.Contains(t, s, `"source":"`+rawURL+`"`)
	require.Contains(t, s, `"shareSocket":{`)
	require.Contains(t, s, `"unixsocket":"`+ms.addr+`"`)
	require.Contains(t, s, `"result":"fakesrc ! fdsink"`)
	require.Contains(t, s, `"share":{"foo":"fakesrc ! interpipesink name=foo"}`)
	require.NotContains(t, s, `"url":`)
	require.NotContains(t, s, `"remote_addr":`)
	require.NotEqual(t, "{}", s)
}
