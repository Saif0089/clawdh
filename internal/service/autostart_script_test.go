package service

import (
	"strings"
	"testing"
)

// The Startup script starts the server through `clawdh start`, which returns at
// once, and never through `start /min`, which left a console window on screen
// for the server's whole life.
func TestAutostartScriptLeavesNoWindowBehind(t *testing.T) {
	script := autostartScript(`C:\Users\me\AppData\Local\clawdh\bin\clawdh.exe`, 8788)

	if strings.Contains(strings.ToLower(script), "/min") || strings.Contains(script, `start ""`) {
		t.Errorf("the script opens a window for the server again:\n%s", script)
	}
	want := `"C:\Users\me\AppData\Local\clawdh\bin\clawdh.exe" start --port 8788 --no-wait`
	if !strings.Contains(script, want) {
		t.Errorf("the script does not run %q:\n%s", want, script)
	}
	if !isCurrentAutostartScript(script) {
		t.Error("a script this build wrote is not recognised as current, so it would be rewritten on every start")
	}
}

// A script an earlier build wrote is replaced: it is the one that opens the
// window, and a machine updated onto this build must not keep it.
func TestAnEarlierBuildsAutostartScriptIsNotCurrent(t *testing.T) {
	legacy := "@echo off\r\nset \"PATH=C:\\x\"\r\ncd /d \"C:\\Users\\me\"\r\nstart \"\" /min \"C:\\clawdh.exe\" serve --port 8788\r\n"
	if isCurrentAutostartScript(legacy) {
		t.Error("the old script that leaves a console window open counts as current, so it would never be replaced")
	}
}

// Percent signs and quotes in a path must not survive into the .cmd, where
// cmd.exe would eat them at parse time and start clawdh from the wrong place.
func TestAutostartScriptEscapesWhatCmdWouldEat(t *testing.T) {
	script := autostartScript(`C:\Users\R%D user\clawdh.exe`, 1)
	if strings.Contains(script, `R%D user`) {
		t.Errorf("a bare %% survived into the script:\n%s", script)
	}
	if !strings.Contains(script, `R%%D user`) {
		t.Errorf("the path lost its escaped %%:\n%s", script)
	}
}
