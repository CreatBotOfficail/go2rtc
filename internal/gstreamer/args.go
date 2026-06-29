package gstreamer

import (
	"maps"
	"net/url"
	"os"
	"runtime"
	"strings"

	"github.com/AlexxIT/go2rtc/internal/streams"
	"github.com/AlexxIT/go2rtc/pkg/gstreamer"
)

// Default unix socket address when env/yaml don't specify one. Linux uses
// abstract namespace so the service needs no filesystem permissions.
const (
	defaultGstreamerSocketAbstract = "\x00GstreamerShareUnixSocket"
	defaultGstreamerSocketPath     = "/tmp/gstreamerShare.socket"
	envGstreamerSocket             = "GSTREAMER_SOCKET"
)

// defaultSocketPath returns the last-resort unix socket address: env wins,
// otherwise the built-in per-OS default.
func defaultSocketPath() string {
	if v := os.Getenv(envGstreamerSocket); v != "" {
		return v
	}
	if runtime.GOOS == "linux" {
		return defaultGstreamerSocketAbstract
	}
	return defaultGstreamerSocketPath
}

// configTemplate resolves a value against defaults, trying both the bare
// key ("foo") and the namespaced key ("typePrefix/foo").
func configTemplate(typePrefix, value string) string {
	if value == "" {
		return value
	}
	if s := defaults[value]; s != "" {
		return s
	}
	if typePrefix != "" {
		if s := defaults[typePrefix+"/"+value]; s != "" {
			return s
		}
	}
	return value
}

// Args is the parsed form of one gstreamer: source URL.
type Args struct {
	Bin         string // gst-launch binary
	Input       string // gst src element or streams? reference
	Caps        string // caps filter
	Process     string // processing chain between src and sink
	Output      string // gst sink element
	Socket      string // unix socket address of the external service
	Share       string // interpipe share name
	PostProcess string // optional chain appended after stream combination
	ShareAttrs  string // extra attributes for interpipesrc elements
	BinArgs     string // extra gst-launch command-line flags

	needSocket   bool                   // set by streams? body or #share / #socket in query
	cachedSocket *gstreamer.ShareSocket // memoised socketArgs() result
}

// defaults: bare keys are field names; namespaced ("output/rtsp") are
// resolved by configTemplate.
var defaults = map[string]string{
	"bin":     "gst-launch-1.0",
	"input":   "autovideosrc",
	"caps":    "video/x-raw",
	"process": "queue ! jpegenc",
	"output":  "fdsink",

	// presets
	"caps/jpeg":   "image/jpeg",
	"output/rtsp": "rtspclientsink location={output} protocols=tcp",

	// share / socket
	"socket":      "", // resolved at init from yaml / defaultSocketPath
	"share":       "interpipeshare",
	"postProcess": "",
	"shareAttrs":  "is-live=true allow-renegotiation=true stream-sync=compensate-ts",

	// other
	"binArgs": "-q",
}

// parseArgs parses the URL body+query after the "gstreamer:" scheme.
// Malformed query values fall back to literals.
func parseArgs(s string) *Args {
	body, query := s, url.Values(nil)
	if i := strings.IndexByte(s, '#'); i >= 0 {
		query = streams.ParseQuery(s[i+1:])
		body = s[:i]
	}

	a := &Args{
		Input:   defaults["input"],
		Bin:     defaults["bin"],
		Caps:    defaults["caps"],
		Process: defaults["process"],
		Output:  defaults["output"],
		BinArgs: defaults["binArgs"],
	}

	applyInputSpec(a, body)
	applyQueryOverrides(a, query)
	if a.needSocket {
		applySocketDefaults(a, query)
	}
	return a
}

// applyInputSpec recognises "device?..." and "streams?..." bodies; anything
// else is a literal gst src element.
func applyInputSpec(a *Args, body string) {
	if body == "" || body == "default" {
		return
	}
	idx := strings.IndexByte(body, '?')
	if idx < 0 {
		a.Input = body
		return
	}
	inQuery, err := url.ParseQuery(body[idx+1:])
	if err != nil {
		// malformed query: keep raw body so gst-launch sees the literal
		a.Input = body
		return
	}
	switch body[:idx] {
	case "device":
		if v := inQuery.Get("video"); v != "" {
			a.Input = "v4l2src device=" + v
		}
	case "streams":
		// streams? runs on the external service; drop default caps/process
		// so the upstream's own pipeline reaches gst-launch.
		if name := inQuery.Get("name"); name != "" {
			a.Input = body
			a.needSocket = true
			a.Caps = ""
			a.Process = ""
		}
	}
}

func applyQueryOverrides(a *Args, query url.Values) {
	if query == nil {
		return
	}
	if query.Has("bin") {
		a.Bin = configTemplate("bin", query.Get("bin"))
	}
	if query.Has("caps") {
		a.Caps = configTemplate("caps", query.Get("caps"))
	}
	if query.Has("process") {
		a.Process = configTemplate("process", query.Get("process"))
	}
	if query.Has("output") {
		a.Output = configTemplate("output", query.Get("output"))
	}
	if query.Has("binArgs") {
		a.BinArgs = configTemplate("binArgs", query.Get("binArgs"))
	}
	// bare "#share" / "#socket" (no '=') must still trigger socket mode,
	// hence Has() instead of Get() != "".
	if query.Has("share") {
		a.needSocket = true
	}
	if query.Has("socket") {
		a.needSocket = true
	}
}

func applySocketDefaults(a *Args, query url.Values) {
	a.Output = "fdsink"

	if query.Has("share") {
		a.Share = configTemplate("share", query.Get("share"))
		if a.Share == "" {
			a.Share = defaults["share"]
		}
		a.PostProcess = defaults["postProcess"]
		if query.Has("postProcess") {
			a.PostProcess = configTemplate("postProcess", query.Get("postProcess"))
		}
	}

	if query.Has("socket") {
		a.Socket = configTemplate("socket", query.Get("socket"))
	}
	if a.Socket == "" {
		a.Socket = defaults["socket"]
	}
	if a.Socket == "" {
		a.Socket = defaultSocketPath()
	}

	a.ShareAttrs = defaults["shareAttrs"]
	if query.Has("shareAttrs") {
		a.ShareAttrs = configTemplate("shareAttrs", query.Get("shareAttrs"))
	}
}

// socketArgs returns the topology the external service must run. Result is
// memoised on a.cachedSocket. Returns the zero value when a.Socket is empty.
func (a *Args) socketArgs() gstreamer.ShareSocket {
	if a.cachedSocket != nil {
		return *a.cachedSocket
	}
	if a.Socket == "" {
		a.cachedSocket = &gstreamer.ShareSocket{}
		return *a.cachedSocket
	}
	res := gstreamer.ShareSocket{
		Unixsocket: a.Socket,
		Share:      map[string]string{},
	}

	shareName := ""
	if sourceArgs, ok := findStreamSource(a.Input); ok {
		shareName = sourceArgs.Share
		// clone: writes below must not alias the upstream map
		res.Share = maps.Clone(sourceArgs.socketArgs().Share)
	}

	if a.Share != "" {
		sinkOutput := "interpipesink name=" + a.Share
		switch {
		case shareName != "" && shareName != a.Share:
			res.Share[a.Share] = a.interpipeChain(shareName, true, false, sinkOutput)
		case shareName == "":
			res.Share[a.Share] = a.pipeline(false) + " ! " + sinkOutput
		default:
			// shareName == a.Share: upstream already publishes this name
		}
		res.Result = a.interpipeChain(a.Share, false, true, a.Output)
	} else if shareName != "" {
		res.Result = a.interpipeChain(shareName, true, false, a.Output)
	} else {
		res.Result = a.pipeline(true)
	}
	a.cachedSocket = &res
	return res
}

// String builds the full gst-launch command line.
func (a *Args) String() string {
	return a.Bin + " " + a.BinArgs + " " + a.pipeline(true)
}

// pipeline joins parts with " ! ". withOutput appends post-process + sink.
func (a *Args) pipeline(withOutput bool) string {
	parts := collectPipelineParts(a)
	if withOutput {
		if a.PostProcess != "" {
			parts = append(parts, a.PostProcess)
		}
		if a.Output != "" {
			parts = append(parts, a.Output)
		}
	}
	return strings.Join(parts, " ! ")
}

// collectPipelineParts walks a chain of "streams?" references, replacing
// each by its upstream parts. Recursion is bounded by config nesting.
func collectPipelineParts(a *Args) []string {
	parts := []string{a.Input}
	if sourceArgs, ok := findStreamSource(a.Input); ok {
		parts = collectPipelineParts(sourceArgs)
	}
	if a.Caps != "" {
		parts = append(parts, a.Caps)
	}
	if a.Process != "" {
		parts = append(parts, a.Process)
	}
	return parts
}

// interpipeChain builds an interpipesrc-based chain. withCapsProcess appends
// Caps and Process (with a queue prefix unless Process already starts with
// one); withPostProcess appends PostProcess; output is the final element.
func (a *Args) interpipeChain(shareName string, withCapsProcess, withPostProcess bool, output string) string {
	parts := []string{"interpipesrc listen-to=" + shareName + " " + a.ShareAttrs}

	if withCapsProcess {
		if a.Caps != "" {
			parts = append(parts, a.Caps)
		}
		if strings.HasPrefix(a.Process, "queue") {
			parts = append(parts, a.Process)
		} else {
			parts = append(parts, "queue")
			if a.Process != "" {
				parts = append(parts, a.Process)
			}
		}
	} else {
		parts = append(parts, "queue")
	}

	if withPostProcess && a.PostProcess != "" {
		parts = append(parts, a.PostProcess)
	}
	parts = append(parts, output)
	return strings.Join(parts, " ! ")
}

// findStreamSource locates the gstreamer source within a named stream whose
// Share matches the request. The located source is assumed to be in socket
// mode so its share map can be inherited.
func findStreamSource(input string) (*Args, bool) {
	if !strings.HasPrefix(input, "streams?") {
		return nil, false
	}
	inQuery, err := url.ParseQuery(strings.TrimPrefix(input, "streams?"))
	if err != nil {
		return nil, false
	}
	name := inQuery.Get("name")
	if name == "" {
		return nil, false
	}
	requestShare := inQuery.Get("share")
	if requestShare == "" {
		requestShare = name
	}
	stream := streams.Get(name)
	if stream == nil {
		return nil, false
	}
	for _, source := range stream.Sources() {
		if !strings.HasPrefix(source, "gstreamer:") {
			continue
		}
		sourceArgs := parseArgs(strings.TrimPrefix(source, "gstreamer:"))
		if sourceArgs == nil || sourceArgs.Share != requestShare {
			continue
		}
		return sourceArgs, true
	}
	return nil, false
}
