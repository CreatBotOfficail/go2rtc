package gstreamer

// Request is the JSON payload sent to the external gstreamer service.
type Request struct {
	Action string            `json:"action"`
	Result string            `json:"result"`
	Share  map[string]string `json:"share,omitempty"`
}

type Response struct {
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
}

// ShareSocket is the bundle the external gstreamer service is asked to run.
type ShareSocket struct {
	Unixsocket string            `json:"unixsocket"`
	Result     string            `json:"result"`
	Share      map[string]string `json:"share,omitempty"`
}
