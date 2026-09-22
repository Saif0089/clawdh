package sessions

import (
	"os"
	"strings"
)

// bundles maps the macOS launcher's bundle id to the name people call the
// editor by. The extension host inherits __CFBundleIdentifier from the app
// that started it, which is the one reliable way to tell a Cursor chat from a
// VS Code chat — the extension itself reports neither.
var bundles = map[string]string{
	"com.microsoft.vscode":          "VS Code",
	"com.microsoft.vscodeinsiders":  "VS Code Insiders",
	"com.todesktop.230313mzl4w4u92": "Cursor",
	"com.visualstudio.code.oss":     "VSCodium",
	"com.exafunction.windsurf":      "Windsurf",
	"com.codeium.windsurf":          "Windsurf",
}

// EditorName is the editor hosting this process, as well as it can be told
// from the environment, or "" when it cannot. It is a label for a person to
// recognise their own window by, never something to make a decision on — an
// editor clawdh cannot name is still an editor, and is shown as one.
func EditorName() string {
	if id := strings.ToLower(strings.TrimSpace(os.Getenv("__CFBundleIdentifier"))); id != "" {
		if name, ok := bundles[id]; ok {
			return name
		}
	}
	// The integrated terminal and, on Linux and Windows, the extension host
	// itself set these. Cursor and Windsurf are VS Code forks and keep the
	// VSCODE_ names, so their own variables are checked first.
	switch {
	case os.Getenv("CURSOR_TRACE_ID") != "", strings.Contains(strings.ToLower(os.Getenv("VSCODE_CWD")), "cursor"):
		return "Cursor"
	case strings.Contains(strings.ToLower(os.Getenv("VSCODE_CWD")), "windsurf"):
		return "Windsurf"
	case os.Getenv("VSCODE_PID") != "", os.Getenv("VSCODE_CWD") != "", os.Getenv("TERM_PROGRAM") == "vscode":
		return "VS Code"
	}
	return ""
}
