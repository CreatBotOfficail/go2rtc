package gstreamer

import (
	"os/exec"
	"sync"
)

var binMu sync.Mutex
var binChecked = map[string]error{} // process-lifetime cache; not invalidated

func checkBin(bin string) error {
	binMu.Lock()
	defer binMu.Unlock()

	if err, ok := binChecked[bin]; ok {
		return err
	}

	_, err := exec.LookPath(bin)
	binChecked[bin] = err
	return err
}
