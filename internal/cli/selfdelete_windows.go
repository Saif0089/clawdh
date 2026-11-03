//go:build windows

package cli

import (
	"clawdh/internal/winproc"
	"fmt"
	"os"
	"strings"
)

// deleteSelfBinary removes path, clawdh's own currently-running
// executable. A direct delete of a running exe's own file sometimes
// just works (the loader can open the main image with
// FILE_SHARE_DELETE), so try that first; if it doesn't, fall back to a
// detached PowerShell helper that waits for this process to exit and
// retries the delete for a while — the file's lock (this process's own
// exit, or a moment of AV scanning right after) can take a few seconds
// to clear, so the retry lives in the helper rather than as a single
// fixed delay.
func deleteSelfBinary(path string) error {
	if err := os.Remove(path); err == nil {
		return nil
	}

	script := fmt.Sprintf(
		`for ($i = 0; $i -lt 30; $i++) { Start-Sleep -Milliseconds 500; try { Remove-Item -LiteralPath '%s' -Force -ErrorAction Stop; break } catch {} }`,
		strings.ReplaceAll(path, "'", "''"),
	)
	cmd := winproc.Command("powershell", "-NoProfile", "-NonInteractive", "-WindowStyle", "Hidden", "-Command", script)
	return cmd.Start()
}
