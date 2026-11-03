//go:build windows

package updater

import (
	"clawdh/internal/winproc"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf16"
)

// OldBinarySuffix starts the name of the executable an update displaced.
// Windows will not let a running image be overwritten, but it will let it be
// *renamed*, so the old binary is moved aside and deleted later — by the next
// start, once nothing has it open.
//
// Every update uses a name of its own (clawdh.exe.old-<time>), never a shared
// clawdh.exe.old. An image displaced by an earlier update is often still
// running — a `clawdh run` session keeps one open for hours — and a running
// image can be neither deleted nor renamed over, so a fixed name made the NEXT
// update fail with "Access is denied", which was then taken for "this folder
// needs administrator rights".
const OldBinarySuffix = ".old"

// displacedName is where this update moves the running executable.
func displacedName(to string) string {
	return fmt.Sprintf("%s%s-%d", to, OldBinarySuffix, time.Now().UnixNano())
}

// elevatedReplaceTimeout bounds the UAC round trip. A prompt that is
// never answered must not wedge the update loop: the supervisor calls
// this on a timer, so a hang here is worse than a failed update.
const elevatedReplaceTimeout = 2 * time.Minute

// elevatedReplace is a variable so tests can observe whether the
// unprivileged path was enough without ever raising a real UAC prompt.
var elevatedReplace = replaceBinaryElevated

// swapInPlace and dirWritable are variables so tests can make Windows refuse
// the swap, and say a folder is not writable, without needing either to be true
// of a real folder.
var (
	swapInPlace = replaceBinaryInPlace
	dirWritable = canWriteDir
)

// canWriteDir reports whether this account can create a file in dir, which is
// what "needs administrator rights" actually means for an update.
func canWriteDir(dir string) bool {
	f, err := os.CreateTemp(dir, ".clawdh-write-check-*")
	if err != nil {
		return false
	}
	name := f.Name()
	f.Close()
	_ = os.Remove(name)
	return true
}

// replaceBinary swaps the verified download in on Windows: rename the
// running executable out of the way, move the new one into its place,
// and put the old one back if that second step fails, so a failed
// update can never leave the service with no binary at all.
//
// clawdh installs per-user and needs no elevation for any of that — see
// install.ps1, which writes to %LOCALAPPDATA%. But it can also be put
// somewhere the user cannot write: an administrator deploying it to
// Program Files, or an enterprise image. There the renames fail with
// ERROR_ACCESS_DENIED and the update would never land.
//
// So: try unprivileged, and ask for administrator rights only when the folder
// really is one this account cannot write, and only when a person asked for the
// update (the page's Update now, `clawdh update`). A refusal in a folder the
// account CAN write is something holding a file, which administrator rights do
// not fix, and the timer's update must never raise a prompt nobody is there to
// answer. Nothing prompts on an ordinary per-user install, which is what keeps
// CI (and every normal machine) prompt-free.
func replaceBinary(ctx context.Context, from, to string) error {
	err := swapInPlace(from, to)
	if err == nil || !isAccessDenied(err) {
		return err
	}
	if dirWritable(filepath.Dir(to)) {
		return err
	}
	if !interactive(ctx) {
		return fmt.Errorf("installing the update over %s needs administrator rights (clawdh is installed "+
			"somewhere this account cannot write). Press Update now on the clawdh page, or run "+
			"`clawdh update`, to approve it: %w", to, err)
	}
	if elevErr := elevatedReplace(from, to); elevErr != nil {
		return fmt.Errorf("installing the update over %s needs administrator rights "+
			"(clawdh is installed somewhere this account cannot write) and the elevated "+
			"retry did not complete: %w", to, elevErr)
	}
	return nil
}

// replaceBinaryInPlace is the swap as an ordinary user, and is the only
// path an ordinary install ever takes.
func replaceBinaryInPlace(from, to string) error {
	old := displacedName(to)

	if err := os.Rename(to, old); err != nil {
		return fmt.Errorf("moving the running %s aside: %w", to, err)
	}
	if err := os.Rename(from, to); err != nil {
		if restoreErr := os.Rename(old, to); restoreErr != nil {
			return fmt.Errorf("installing the update over %s: %w (and the previous binary could not be put back: %v)", to, err, restoreErr)
		}
		return fmt.Errorf("installing the update over %s: %w", to, err)
	}
	// Fails while this process is still running its own image; the
	// CleanupOldBinary call at the next start finishes the job.
	_ = os.Remove(old)
	return nil
}

// isAccessDenied reports whether Windows refused the operation for want
// of rights, as opposed to the file being missing, locked by another
// process, or on a full disk — none of which elevation would fix, and
// all of which would turn into a pointless UAC prompt.
func isAccessDenied(err error) bool {
	if errors.Is(err, fs.ErrPermission) {
		return true
	}
	// ERROR_ACCESS_DENIED. Checked as well as fs.ErrPermission because
	// the mapping is only guaranteed for the errors os itself wraps.
	var errno interface{ Errno() uintptr }
	if errors.As(err, &errno) && errno.Errno() == 5 {
		return true
	}
	return false
}

// replaceBinaryElevated performs the same swap through a UAC prompt.
//
// The work runs in a second PowerShell that Start-Process launches with
// -Verb RunAs, which is what raises the prompt. The script is passed as
// -EncodedCommand: the paths are attacker-irrelevant but they do carry
// spaces and quotes routinely (C:\Users\John Smith\...), and base64
// removes every layer of quoting between here and there rather than
// getting it right by inspection.
func replaceBinaryElevated(from, to string) error {
	old := displacedName(to)
	// Same sequence as the unprivileged path, and the same restore on
	// failure, so an elevated attempt cannot leave the machine without
	// a binary either. $ErrorActionPreference makes a failed Move-Item
	// terminate rather than continue into the next line.
	inner := fmt.Sprintf(`$ErrorActionPreference = 'Stop'
Remove-Item -LiteralPath %[1]s -Force -ErrorAction SilentlyContinue
Move-Item -LiteralPath %[2]s -Destination %[1]s -Force
try { Move-Item -LiteralPath %[3]s -Destination %[2]s -Force }
catch { Move-Item -LiteralPath %[1]s -Destination %[2]s -Force; throw }
Remove-Item -LiteralPath %[1]s -Force -ErrorAction SilentlyContinue
`, psQuote(old), psQuote(to), psQuote(from))

	outer := fmt.Sprintf(
		`$p = Start-Process powershell -Verb RunAs -Wait -PassThru -WindowStyle Hidden `+
			`-ArgumentList '-NoProfile','-NonInteractive','-EncodedCommand',%s; exit $p.ExitCode`,
		psQuote(encodePowerShellCommand(inner)))

	ctx, cancel := context.WithTimeout(context.Background(), elevatedReplaceTimeout)
	defer cancel()

	cmd := winproc.CommandContext(ctx, "powershell", "-NoProfile", "-NonInteractive", "-Command", outer)
	out, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		// Almost always an unanswered prompt. Say so, rather than
		// reporting a bare timeout that reads like a hung download.
		return fmt.Errorf("the administrator prompt was not answered within %s", elevatedReplaceTimeout)
	}
	if err != nil {
		detail := strings.TrimSpace(string(out))
		if detail == "" {
			// The user dismissing the UAC dialog is a refusal, not a bug.
			detail = "the prompt was declined, or elevation is not available for this account"
		}
		return fmt.Errorf("%w: %s", err, detail)
	}
	return nil
}

// encodePowerShellCommand renders s for -EncodedCommand: UTF-16LE, then
// base64, which is the format PowerShell documents for it.
func encodePowerShellCommand(s string) string {
	units := utf16.Encode([]rune(s))
	buf := make([]byte, 0, len(units)*2)
	for _, u := range units {
		buf = append(buf, byte(u), byte(u>>8))
	}
	return base64.StdEncoding.EncodeToString(buf)
}

// psQuote single-quotes s for PowerShell, where doubling is the only
// escape inside a literal string.
func psQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// CleanupOldBinary removes the executables previous updates displaced. Called at
// startup, when the ones nobody is running any more can finally be deleted; one
// a `clawdh run` session still has open is left for a later start.
func CleanupOldBinary(binaryPath string) {
	dir, base := filepath.Split(binaryPath)
	if dir == "" {
		dir = "."
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	prefix := strings.ToLower(base + OldBinarySuffix)
	for _, e := range entries {
		if !e.IsDir() && strings.HasPrefix(strings.ToLower(e.Name()), prefix) {
			_ = os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}
