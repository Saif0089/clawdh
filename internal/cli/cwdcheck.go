package cli

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"clawdh/internal/sessions"
)

// cwdProblem says why Claude Code could not run in the current folder, or ""
// when it can. Claude Code dies at startup in a folder it cannot read and
// reports it as "An unknown error occurred (Unexpected)", which says nothing
// about the cause. On a Mac the cause is almost always macOS privacy protection
// refusing the terminal access to Documents, Desktop or Downloads — seen on
// 2026-09-29, when only Full Disk Access brought it back.
//
// It is checked before a session starts, and before a switch relaunches one:
// relaunching into a folder Claude Code cannot read would end a session that is
// still running.
func cwdProblem() string {
	f, err := os.Open(".")
	if err == nil {
		_, err = f.Readdirnames(1)
		f.Close()
		if errors.Is(err, io.EOF) {
			err = nil // an empty folder is readable
		}
	}
	if err == nil || !errors.Is(err, fs.ErrPermission) {
		return ""
	}
	dir := os.Getenv("PWD")
	if dir == "" {
		dir, _ = os.Getwd()
	}
	if home, herr := os.UserHomeDir(); herr == nil && dir != "" {
		if rel, rerr := filepath.Rel(home, dir); rerr == nil && !strings.HasPrefix(rel, "..") {
			dir = filepath.Join("~", rel)
		}
	}
	if runtime.GOOS == "darwin" {
		app := hostAppName()
		return fmt.Sprintf("macOS is not letting %s read this folder (%s), so Claude Code cannot run in it.\n"+
			"Turn %s on in System Settings → Privacy & Security → Full Disk Access (Claude Code too, if it is listed),\n"+
			"then run this again. The Files & Folders switch for Documents is not always enough.", app, dir, app)
	}
	return fmt.Sprintf("This folder (%s) cannot be read (%v), so Claude Code cannot run in it.", dir, err)
}

// hostAppName names the app this runs in, for telling someone which one to
// give access to.
func hostAppName() string {
	if name := sessions.EditorName(); name != "" {
		return name
	}
	switch os.Getenv("TERM_PROGRAM") {
	case "Apple_Terminal":
		return "Terminal"
	case "iTerm.app":
		return "iTerm"
	case "WarpTerminal":
		return "Warp"
	case "ghostty":
		return "Ghostty"
	case "WezTerm":
		return "WezTerm"
	case "vscode":
		return "VS Code"
	}
	return "your terminal app"
}
