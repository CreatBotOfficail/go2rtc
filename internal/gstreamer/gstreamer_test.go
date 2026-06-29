package gstreamer

import (
	"testing"

	"github.com/AlexxIT/go2rtc/internal/streams"
	"github.com/stretchr/testify/require"
)

type testStruct struct {
	name   string
	source string
	cmd    string
	socket string
	result string
	share  map[string]string
}

func TestParseArgs_Defaults(t *testing.T) {
	tests := []testStruct{
		{
			name:   "default",
			source: "default",
			cmd:    "gst-launch-1.0 -q autovideosrc ! video/x-raw ! queue ! jpegenc ! fdsink",
			//       gst-launch-1.0 -q autovideosrc ! video/x-raw ! queue ! autovideosink
		},
		{
			name:   "default with caps",
			source: "default#caps=video/x-raw,width=1280,height=720",
			cmd:    "gst-launch-1.0 -q autovideosrc ! video/x-raw,width=1280,height=720 ! queue ! jpegenc ! fdsink",
			//       gst-launch-1.0 -q autovideosrc ! video/x-raw,width=1280,height=720 ! queue ! autovideosink
		},
		{
			name:   "default with process",
			source: "default#process=queue leaky = downstream ! mppjpegenc",
			cmd:    "gst-launch-1.0 -q autovideosrc ! video/x-raw ! queue leaky = downstream ! mppjpegenc ! fdsink",
			//       gst-launch-1.0 -q autovideosrc ! video/x-raw ! queue leaky = downstream ! autovideosink
		},
		{
			name:   "default with output/rtsp",
			source: "default#output=rtsp",
			cmd:    "gst-launch-1.0 -q autovideosrc ! video/x-raw ! queue ! jpegenc ! rtspclientsink location={output} protocols=tcp",
			//       gst-launch-1.0 -q autovideosrc ! video/x-raw ! queue ! autovideosink
		},
		{
			name:   "default with other output",
			source: "default#output=fakesink",
			cmd:    "gst-launch-1.0 -q autovideosrc ! video/x-raw ! queue ! jpegenc ! fakesink",
			//       gst-launch-1.0 -q autovideosrc ! video/x-raw ! queue ! autovideosink
		},
		{
			name:   "default with other bin",
			source: "default#bin=gst-launch-2.0",
			cmd:    "gst-launch-2.0 -q autovideosrc ! video/x-raw ! queue ! jpegenc ! fdsink",
			//       gst-launch-2.0 -q autovideosrc ! video/x-raw ! queue ! autovideosink
		},
		{
			name:   "default with socket",
			source: "default#socket",
			cmd:    "gst-launch-1.0 -q autovideosrc ! video/x-raw ! queue ! jpegenc ! fdsink",
			socket: defaultSocketPath(),
			result: "autovideosrc ! video/x-raw ! queue ! jpegenc ! fdsink",
			share:  map[string]string{},
		},
		{
			name:   "default with other socket",
			source: "default#socket=/tmp/gstreamer.socket",
			cmd:    "gst-launch-1.0 -q autovideosrc ! video/x-raw ! queue ! jpegenc ! fdsink",
			socket: "/tmp/gstreamer.socket",
			result: "autovideosrc ! video/x-raw ! queue ! jpegenc ! fdsink",
			share:  map[string]string{},
		},
		{
			name:   "default with share",
			source: "default#process=mppjpegenc#share=video0_share",
			cmd:    "gst-launch-1.0 -q autovideosrc ! video/x-raw ! mppjpegenc ! fdsink",
			//       gst-launch-1.0 -q autovideosrc ! video/x-raw ! mppjpegenc ! jpegparse ! mppjpegdec ! autovideosink
			//       gst-launch-1.0 -q autovideosrc ! video/x-raw ! autovideosink
			socket: defaultSocketPath(),
			result: "interpipesrc listen-to=video0_share is-live=true allow-renegotiation=true stream-sync=compensate-ts ! queue ! fdsink",
			share: map[string]string{
				"video0_share": "autovideosrc ! video/x-raw ! mppjpegenc ! interpipesink name=video0_share",
			},
		},
		{
			name:   "default with share and postProcess",
			source: "default#process=mppjpegenc#share=video0_share#postProcess=jpegparse ! mppjpegdec dma-feature=true width=1920 height=1080 ! mppjpegenc",
			cmd:    "gst-launch-1.0 -q autovideosrc ! video/x-raw ! mppjpegenc ! jpegparse ! mppjpegdec dma-feature=true width=1920 height=1080 ! mppjpegenc ! fdsink",
			//       gst-launch-1.0 -q autovideosrc ! video/x-raw ! mppjpegenc ! jpegparse ! mppjpegdec dma-feature=true width=1920 height=1080 ! mppjpegenc ! jpegparse ! mppjpegdec ! autovideosink
			//       gst-launch-1.0 -q autovideosrc ! video/x-raw ! mppjpegenc ! jpegparse ! mppjpegdec dma-feature=true width=1920 height=1080 ! autovideosink
			socket: defaultSocketPath(),
			result: "interpipesrc listen-to=video0_share is-live=true allow-renegotiation=true stream-sync=compensate-ts ! queue ! jpegparse ! mppjpegdec dma-feature=true width=1920 height=1080 ! mppjpegenc ! fdsink",
			share: map[string]string{
				"video0_share": "autovideosrc ! video/x-raw ! mppjpegenc ! interpipesink name=video0_share",
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			args := parseArgs(test.source)
			require.Equal(t, test.cmd, args.String())

			socketArgs := args.socketArgs()
			require.Equal(t, test.socket, socketArgs.Unixsocket)
			require.Equal(t, test.result, socketArgs.Result)
			require.Equal(t, test.share, socketArgs.Share)
		})
	}
}

func TestParseArgs_BareShareKey(t *testing.T) {
	// #share (no '=') should still trigger socket mode and fall back to
	// the default share name, not silently drop the share setting.
	args := parseArgs("default#process=mppjpegenc#share")
	require.True(t, args.needSocket, "bare #share must set needSocket")
	require.Equal(t, "interpipeshare", args.Share, "expected default share name fallback")
	require.NotEmpty(t, args.socketArgs().Unixsocket, "socket mode should resolve a socket address")
}

func TestParseArgs_SocketKeyTriggersSocketMode(t *testing.T) {
	// #socket (no '=') should still put the args in socket mode, even
	// when no streams? body and no #share is present.
	args := parseArgs("default#socket")
	sa := args.socketArgs()
	require.NotEmpty(t, sa.Unixsocket, "expected socket mode to resolve a socket address")
}

func TestParseArgs_StreamReferenceRequiresSocketMode(t *testing.T) {
	// "streams?..." body must trigger socket mode (interpipesink/src
	// only work in one process) and the resulting pipeline must drop
	// the default caps/process so the upstream's own pipeline is the
	// only thing reaching gst-launch.
	args := parseArgs("streams?name=does-not-exist")
	require.True(t, args.needSocket, "streams? body must set needSocket")
	require.Equal(t, "", args.Caps, "streams? mode should clear default caps")
	require.Equal(t, "", args.Process, "streams? mode should clear default process")
	require.NotEmpty(t, args.socketArgs().Unixsocket, "socket mode should resolve a socket address")
}

func TestParseArgs_Device(t *testing.T) {
	tests := []testStruct{
		{
			name:   "device",
			source: "device?video=/dev/video0",
			cmd:    "gst-launch-1.0 -q v4l2src device=/dev/video0 ! video/x-raw ! queue ! jpegenc ! fdsink",
			//       gst-launch-1.0 -q v4l2src device=/dev/video0 ! video/x-raw ! queue ! autovideosink
		},
		{
			name:   "device with caps and process",
			source: "device?video=/dev/video0#caps=image/jpeg,width=1920,height=1080#process=jpegparse",
			cmd:    "gst-launch-1.0 -q v4l2src device=/dev/video0 ! image/jpeg,width=1920,height=1080 ! jpegparse ! fdsink",
			//       gst-launch-1.0 -q v4l2src device=/dev/video0 ! image/jpeg,width=1920,height=1080 ! jpegparse ! mppjpegdec ! autovideosink
		},
		{
			name:   "device with caps and process and output/rtsp",
			source: "device?video=/dev/video0#caps=image/jpeg,width=1920,height=1080#process=queue#output=rtsp",
			cmd:    "gst-launch-1.0 -q v4l2src device=/dev/video0 ! image/jpeg,width=1920,height=1080 ! queue ! rtspclientsink location={output} protocols=tcp",
			//       gst-launch-1.0 -q v4l2src device=/dev/video0 ! image/jpeg,width=1920,height=1080 ! queue ! mppjpegdec ! autovideosink
		},
		{
			name:   "device with share",
			source: "device?video=/dev/video0#caps=image/jpeg,width=1920,height=1080#process=jpegparse#share=video0_share",
			cmd:    "gst-launch-1.0 -q v4l2src device=/dev/video0 ! image/jpeg,width=1920,height=1080 ! jpegparse ! fdsink",
			//       gst-launch-1.0 -q v4l2src device=/dev/video0 ! image/jpeg,width=1920,height=1080 ! jpegparse ! mppjpegdec ! autovideosink
			socket: defaultSocketPath(),
			result: "interpipesrc listen-to=video0_share is-live=true allow-renegotiation=true stream-sync=compensate-ts ! queue ! fdsink",
			share: map[string]string{
				"video0_share": "v4l2src device=/dev/video0 ! image/jpeg,width=1920,height=1080 ! jpegparse ! interpipesink name=video0_share",
			},
		},
		{
			name:   "device with share and postProcess",
			source: "device?video=/dev/video0#caps=image/jpeg,width=1920,height=1080#process=jpegparse#share=video0_share#postProcess=mppjpegdec dma-feature=true width=1280 height=720 ! mpph264enc ! h264parse config-interval=-1",
			cmd:    "gst-launch-1.0 -q v4l2src device=/dev/video0 ! image/jpeg,width=1920,height=1080 ! jpegparse ! mppjpegdec dma-feature=true width=1280 height=720 ! mpph264enc ! h264parse config-interval=-1 ! fdsink",
			//       gst-launch-1.0 -q v4l2src device=/dev/video0 ! image/jpeg,width=1920,height=1080 ! jpegparse ! mppjpegdec dma-feature=true width=1280 height=720 ! mpph264enc ! h264parse config-interval=-1 ! mppvideodec ! autovideosink
			socket: defaultSocketPath(),
			result: "interpipesrc listen-to=video0_share is-live=true allow-renegotiation=true stream-sync=compensate-ts ! queue ! mppjpegdec dma-feature=true width=1280 height=720 ! mpph264enc ! h264parse config-interval=-1 ! fdsink",
			share: map[string]string{
				"video0_share": "v4l2src device=/dev/video0 ! image/jpeg,width=1920,height=1080 ! jpegparse ! interpipesink name=video0_share",
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			args := parseArgs(test.source)
			require.Equal(t, test.cmd, args.String())

			socketArgs := args.socketArgs()
			require.Equal(t, test.socket, socketArgs.Unixsocket)
			require.Equal(t, test.result, socketArgs.Result)
			require.Equal(t, test.share, socketArgs.Share)
		})
	}
}

func TestParseArgs_Streams(t *testing.T) {

	var shareSources = map[string][]string{
		"stream0_share": {
			"gstreamer:default#caps=video/x-raw,width=1920,height=1080",
			"gstreamer:device?video=/dev/video0#caps=image/jpeg,width=1920,height=1080#process=jpegparse#share=video0_share",
		},
		"stream1_share": {
			"gstreamer:default#process=mppjpegenc#share=video0_share",
		},
		"stream2_share": {
			"gstreamer:streams?name=stream0_share&share=video0_share#process=mppjpegdec#share=video1_share#postProcess=mppjpegenc",
		},
	}

	for name, sources := range shareSources {
		streams.New(name, sources...)
		t.Cleanup(func() { streams.Delete(name) })
	}

	tests := []testStruct{
		{
			name:   "stream",
			source: "streams?name=stream0_share&share=video0_share",
			cmd:    "gst-launch-1.0 -q v4l2src device=/dev/video0 ! image/jpeg,width=1920,height=1080 ! jpegparse ! fdsink",
			//       gst-launch-1.0 -q v4l2src device=/dev/video0 ! image/jpeg,width=1920,height=1080 ! jpegparse ! mppjpegdec ! autovideosink
			socket: defaultSocketPath(),
			result: "interpipesrc listen-to=video0_share is-live=true allow-renegotiation=true stream-sync=compensate-ts ! queue ! fdsink",
			share: map[string]string{
				"video0_share": "v4l2src device=/dev/video0 ! image/jpeg,width=1920,height=1080 ! jpegparse ! interpipesink name=video0_share",
			},
		},
		{
			name:   "stream with caps and process",
			source: "streams?name=stream0_share&share=video0_share#caps=jpeg#process=mppjpegdec dma-feature=true width=800 height=600 ! mppjpegenc",
			cmd:    "gst-launch-1.0 -q v4l2src device=/dev/video0 ! image/jpeg,width=1920,height=1080 ! jpegparse ! image/jpeg ! mppjpegdec dma-feature=true width=800 height=600 ! mppjpegenc ! fdsink",
			//       gst-launch-1.0 -q v4l2src device=/dev/video0 ! image/jpeg,width=1920,height=1080 ! jpegparse ! image/jpeg ! mppjpegdec dma-feature=true width=800 height=600 ! mppjpegenc ! jpegparse ! mppjpegdec ! autovideosink
			//       gst-launch-1.0 -q v4l2src device=/dev/video0 ! image/jpeg,width=1920,height=1080 ! jpegparse ! image/jpeg ! mppjpegdec dma-feature=true width=800 height=600 ! autovideosink
			socket: defaultSocketPath(),
			result: "interpipesrc listen-to=video0_share is-live=true allow-renegotiation=true stream-sync=compensate-ts ! image/jpeg ! queue ! mppjpegdec dma-feature=true width=800 height=600 ! mppjpegenc ! fdsink",
			share: map[string]string{
				"video0_share": "v4l2src device=/dev/video0 ! image/jpeg,width=1920,height=1080 ! jpegparse ! interpipesink name=video0_share",
			},
		},
		{
			name:   "stream with process, then share again and postProcess",
			source: "streams?name=stream0_share&share=video0_share#process=mppjpegdec dma-feature=true width=800 height=600#share=video1_share#postProcess=mppjpegenc",
			cmd:    "gst-launch-1.0 -q v4l2src device=/dev/video0 ! image/jpeg,width=1920,height=1080 ! jpegparse ! mppjpegdec dma-feature=true width=800 height=600 ! mppjpegenc ! fdsink",
			//       gst-launch-1.0 -q v4l2src device=/dev/video0 ! image/jpeg,width=1920,height=1080 ! jpegparse ! mppjpegdec dma-feature=true width=800 height=600 ! mppjpegenc ! jpegparse ! mppjpegdec ! autovideosink
			//       gst-launch-1.0 -q v4l2src device=/dev/video0 ! image/jpeg,width=1920,height=1080 ! jpegparse ! mppjpegdec dma-feature=true width=800 height=600 ! autovideosink
			socket: defaultSocketPath(),
			result: "interpipesrc listen-to=video1_share is-live=true allow-renegotiation=true stream-sync=compensate-ts ! queue ! mppjpegenc ! fdsink",
			share: map[string]string{
				"video0_share": "v4l2src device=/dev/video0 ! image/jpeg,width=1920,height=1080 ! jpegparse ! interpipesink name=video0_share",
				"video1_share": "interpipesrc listen-to=video0_share is-live=true allow-renegotiation=true stream-sync=compensate-ts ! queue ! mppjpegdec dma-feature=true width=800 height=600 ! interpipesink name=video1_share",
			},
		},
		{
			name:   "stream with two levels of share",
			source: "streams?name=stream2_share&share=video1_share#process=mpph264enc#share=video2_share#postProcess=h264parse config-interval=-1",
			cmd:    "gst-launch-1.0 -q v4l2src device=/dev/video0 ! image/jpeg,width=1920,height=1080 ! jpegparse ! mppjpegdec ! mpph264enc ! h264parse config-interval=-1 ! fdsink",
			//       gst-launch-1.0 -q v4l2src device=/dev/video0 ! image/jpeg,width=1920,height=1080 ! jpegparse ! mppjpegdec ! mpph264enc ! h264parse config-interval=-1 ! mppvideodec ! autovideosink
			socket: defaultSocketPath(),
			result: "interpipesrc listen-to=video2_share is-live=true allow-renegotiation=true stream-sync=compensate-ts ! queue ! h264parse config-interval=-1 ! fdsink",
			share: map[string]string{
				"video0_share": "v4l2src device=/dev/video0 ! image/jpeg,width=1920,height=1080 ! jpegparse ! interpipesink name=video0_share",
				"video1_share": "interpipesrc listen-to=video0_share is-live=true allow-renegotiation=true stream-sync=compensate-ts ! queue ! mppjpegdec ! interpipesink name=video1_share",
				"video2_share": "interpipesrc listen-to=video1_share is-live=true allow-renegotiation=true stream-sync=compensate-ts ! queue ! mpph264enc ! interpipesink name=video2_share",
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			args := parseArgs(test.source)
			require.Equal(t, test.cmd, args.String())

			socketArgs := args.socketArgs()
			require.Equal(t, test.socket, socketArgs.Unixsocket)
			require.Equal(t, test.result, socketArgs.Result)
			require.Equal(t, test.share, socketArgs.Share)
		})
	}
}
