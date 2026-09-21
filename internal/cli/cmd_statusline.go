package cli

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"clawdh/internal/buildinfo"
	"clawdh/internal/config"
	"clawdh/internal/service"
	"clawdh/internal/statusline"
)

// cmdStatusLine is what Claude Code's status line runs once clawdh has wrapped
// it (see ensureStatusLine): the person's own status-line command, with the
// clawdh badge in front. Claude Code passes the session's JSON on stdin and
// shows whatever is printed; it runs on every redraw, so this does the least
// it can — read the saved command, run it, print.
//
// Run by hand outside a session it prints the person's line as Claude Code
// would see it, which is the way to check what clawdh wrapped.
func cmdStatusLine(_ []string) int {
	stdin, _ := io.ReadAll(os.Stdin)
	theirs, _ := statusline.Saved(statusLineSavePath())
	badge := statusline.Badge(os.Getenv, buildinfo.Compact())
	if out := statusline.Render(theirs, stdin, badge); out != "" {
		fmt.Println(out)
	}
	return 0
}

// statusLineSavePath is where the person's own status line is kept while
// clawdh's wraps it.
func statusLineSavePath() string {
	base, err := config.HomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(base, "statusline.json")
}

// ensureStatusLine wraps the person's status line with clawdh's badge, at
// every session launch, like the switch hook: idempotent, and a failure is a
// warning on stderr — the session runs without a badge, not not at all.
func ensureStatusLine(settingsPath string) {
	self, err := service.SelfPath()
	if err != nil {
		return
	}
	if err := statusline.Ensure(settingsPath, statusLineSavePath(), self); err != nil {
		fmt.Fprintln(os.Stderr, "clawdh: could not add the status-line badge:", err)
	}
}
