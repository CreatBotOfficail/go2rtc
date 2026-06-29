package gstreamer

import (
	"errors"
	"strings"

	"github.com/AlexxIT/go2rtc/internal/app"
	"github.com/AlexxIT/go2rtc/internal/streams"
	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/AlexxIT/go2rtc/pkg/gstreamer"
	"github.com/rs/zerolog"
)

var log zerolog.Logger

func Init() {
	var cfg struct {
		Mod map[string]string `yaml:"gstreamer"`
	}

	cfg.Mod = defaults // initial values, overridden by yaml

	app.LoadConfig(&cfg)

	log = app.GetLogger("gstreamer")

	// ensure defaults["socket"] is non-empty for production callers
	if defaults["socket"] == "" {
		defaults["socket"] = defaultSocketPath()
	}

	log.Info().Str("socket", defaults["socket"]).Msg("[gstreamer] initialized")

	streams.RedirectFunc("gstreamer", gstreamerRedirect)
	streams.HandleFunc("gstreamer", gstreamerHandle)
}

// gstreamerRedirect turns a gstreamer: source into an exec: source, or
// signals HandleFunc to take over for socket mode.
func gstreamerRedirect(url string) (string, error) {
	args := parseArgs(url[len("gstreamer:"):])

	if args.needSocket {
		return "", nil
	}
	if err := checkBin(args.Bin); err != nil {
		return "", err
	}
	return "exec:" + args.String(), nil
}

// gstreamerHandle runs the pipeline graph on the external gstreamer service
// over a unix socket.
func gstreamerHandle(rawURL string) (core.Producer, error) {
	args := parseArgs(rawURL[len("gstreamer:"):])

	sa := args.socketArgs()
	if sa.Unixsocket == "" {
		return nil, errors.New("gstreamer: exec mode should have been redirected")
	}

	// strip query before the empty-body check so leading/trailing
	// whitespace from yaml doesn't break it
	body := strings.TrimSpace(strings.SplitN(rawURL, "#", 2)[0])
	if body == "" {
		return nil, errors.New("gstreamer: empty pipeline")
	}

	log.Debug().
		Str("socket", sa.Unixsocket).
		Str("result", sa.Result).
		Int("share", len(sa.Share)).
		Msg("[gstreamer] handle socket mode")

	// sa carries the unix socket + pipelines the wrapper needs to
	// dial the service and render the JSON output.
	return gstreamer.NewProducer(rawURL, &sa)
}
