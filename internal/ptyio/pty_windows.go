//go:build windows

package ptyio

import (
	"clawdh/internal/winproc"
	"context"
	"os/exec"
	"strconv"
	"strings"
	"syscall"

	"github.com/UserExistsError/conpty"
)

type windowsSession struct {
	cpty *conpty.ConPty
	pid  int
}

func start(name string, args []string, env []string, dir string) (Session, error) {
	// conpty hands the command line straight to CreateProcess, which
	// appends ".exe" but does not consult PATHEXT — so a `claude.cmd`
	// shim (what `npm i -g` installs on Windows) would never be found,
	// even though it resolves fine in a shell. exec.LookPath does
	// honour PATHEXT, so resolve before handing it over.
	if resolved, err := exec.LookPath(name); err == nil {
		name = resolved
	}

	cmdLine := buildCommandLine(name, args)
	opts := []conpty.ConPtyOption{
		conpty.ConPtyDimensions(DefaultCols, DefaultRows),
		conpty.ConPtyEnv(env),
	}
	if dir != "" {
		opts = append(opts, conpty.ConPtyWorkDir(dir))
	}
	cpty, err := conpty.Start(cmdLine, opts...)
	if err != nil {
		return nil, err
	}
	return &windowsSession{cpty: cpty, pid: cpty.Pid()}, nil
}

// buildCommandLine renders name+args as a single Windows command line,
// escaped per the same rules Go's own os/exec uses on Windows.
func buildCommandLine(name string, args []string) string {
	parts := make([]string, 0, len(args)+1)
	parts = append(parts, syscall.EscapeArg(name))
	for _, a := range args {
		parts = append(parts, syscall.EscapeArg(a))
	}
	return strings.Join(parts, " ")
}

func (s *windowsSession) Read(p []byte) (int, error)  { return s.cpty.Read(p) }
func (s *windowsSession) Write(p []byte) (int, error) { return s.cpty.Write(p) }

// Close ends the session and the process it started.
//
// conpty's own Close only closes the pseudo-console and the handles —
// it never terminates the child, and it discards the process handle, so
// nothing can kill it afterwards. Without this, cancelling a login or
// shutting the server down left `claude` running on Windows for its
// full timeout (and holding an open handle on the account directory,
// which then blocks deleting it). The child is killed as a tree because
// the real claude may be a claude.cmd shim, i.e. cmd.exe with claude
// underneath it.
func (s *windowsSession) Close() error {
	if s.pid > 0 {
		kill := winproc.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(s.pid))
		_ = kill.Run()
	}
	return s.cpty.Close()
}
func (s *windowsSession) Resize(cols, rows int) error { return s.cpty.Resize(cols, rows) }

func (s *windowsSession) Wait(ctx context.Context) (int, error) {
	code, err := s.cpty.Wait(ctx)
	return int(code), err
}
