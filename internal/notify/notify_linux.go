//go:build linux

package notify

import (
	"errors"
	"os/exec"
)

// notifyCommand uses notify-send, the freedesktop.org standard client
// that every desktop environment's notification daemon answers. It is
// not always installed (a server, a bare container), which is a reason
// to say nothing rather than to fail an update.
func notifyCommand(body string) (string, []string, error) {
	path, err := exec.LookPath("notify-send")
	if err != nil {
		return "", nil, errors.New("no notify-send on PATH, so there is nowhere to show a desktop notification")
	}
	return path, []string{"--app-name=clawdh", title, body}, nil
}
