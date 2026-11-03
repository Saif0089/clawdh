// Package notify shows a desktop notification.
//
// clawdh runs as a background service with no window of its own, so the
// one moment it has something to say — "I just replaced myself with a
// newer build" — has nowhere to appear. A notification is the only
// channel that reaches someone who is not looking at the page.
//
// Every platform's mechanism is best-effort: a missing tool, a headless
// session, or notifications turned off must never be worth failing an
// update over, so Send reports the error for the log and nothing else
// acts on it.
package notify

import (
	"clawdh/internal/config"
	"clawdh/internal/winproc"
	"context"
	"fmt"
	"strings"
	"time"
)

// Timeout bounds the helper process. The notification itself lingers on
// screen for the desktop to decide; this only stops a wedged helper
// from living as long as the service does.
const Timeout = 20 * time.Second

// Enabled reports whether notifications should be shown at all.
//
// CLAWDH_NOTIFY=0 turns them off, which is how the test suite runs the
// real update path without popping a notification on the desktop of
// whoever happens to be running it.
func Enabled() bool {
	switch strings.ToLower(strings.TrimSpace(config.Env("NOTIFY"))) {
	case "0", "false", "off", "no":
		return false
	}
	return true
}

// Send shows a desktop notification with clawdh's name as the title.
func Send(body string) error {
	ctx, cancel := context.WithTimeout(context.Background(), Timeout)
	defer cancel()
	return SendContext(ctx, body)
}

// SendContext is Send with a caller-supplied deadline.
func SendContext(ctx context.Context, body string) error {
	if !Enabled() {
		return nil
	}
	name, args, err := notifyCommand(body)
	if err != nil {
		return err
	}
	cmd := winproc.CommandContext(ctx, name, args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("showing a desktop notification via %s: %w: %s", name, err, out)
	}
	return nil
}

// title is what every platform's notification is headed with.
const title = "clawdh"
