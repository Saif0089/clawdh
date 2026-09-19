package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"clawdh/internal/accounts"
	"clawdh/internal/claudebin"
	"clawdh/internal/config"
	"clawdh/internal/httpserver"
	"clawdh/internal/notify"
	"clawdh/internal/service"
	"clawdh/internal/shellrc"
	"clawdh/internal/updater"
)

func cmdServe(args []string) int {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	port := fs.Int("port", config.DefaultPort, "port to listen on")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	// Only bail out if the *requested* port is the one already being
	// served; asking for a different port is a legitimate request, not
	// a duplicate start.
	if info, err := service.Running(); err == nil && info != nil && info.Port == *port {
		fmt.Fprintf(os.Stdout, "clawdh is already running on http://127.0.0.1:%d\n", info.Port)
		return 0
	}

	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintln(os.Stderr, "clawdh: resolving home directory:", err)
		return 1
	}
	accountsFile, err := config.AccountsFile()
	if err != nil {
		fmt.Fprintln(os.Stderr, "clawdh:", err)
		return 1
	}
	accountsDir, err := config.AccountsDir()
	if err != nil {
		fmt.Fprintln(os.Stderr, "clawdh:", err)
		return 1
	}

	capLogFile()
	// And keep capping it: every request is logged, so a service left
	// running with a page open would otherwise grow the log without
	// bound until the next restart.
	go func() {
		for range time.Tick(5 * time.Minute) {
			capLogFile()
		}
	}()

	manager := accounts.NewManager(accounts.NewStore(accountsFile), accountsDir)
	syncer := shellrc.NewSyncer(home)

	// Snapshot the default account's identity once, while ~/.claude.json still
	// cleanly names it, so switching back to it later can restore /status. Safe
	// on first boot after upgrade: managed accounts were isolated until now, so
	// nothing had ever rewritten ~/.claude.json.
	// The managed accounts' own directories are passed so an identity a switch
	// left in ~/.claude.json is recognised as theirs and refused. A store that
	// cannot be read means we cannot tell, so nothing is captured this boot.
	var managedDirs []string
	if list, err := accounts.NewStore(accountsFile).Load(); err == nil {
		for _, a := range list {
			if !a.IsDefault() && a.ConfigDir != "" {
				managedDirs = append(managedDirs, a.ConfigDir)
			}
		}
		if err := accounts.SnapshotDefaultIdentity(
			filepath.Join(home, ".claude.json"),
			filepath.Join(accountsDir, "default"),
			managedDirs,
		); err != nil {
			log.Printf("warning: could not snapshot the default account identity: %v", err)
		}
	} else {
		log.Printf("warning: not snapshotting the default identity, accounts unreadable: %v", err)
	}

	// Resolved once, here, rather than relying on PATH at spawn time:
	// started by launchd/systemd at login this process has almost no
	// PATH, so "claude" alone would not be found (see internal/claudebin).
	claude := claudebin.Resolve()
	log.Printf("using claude binary: %s", claude)
	srv := httpserver.New(manager, syncer, claude)

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	startAutoUpdate(ctx, srv)

	// Remote-help requests that ship data off this machine wait in a queue for
	// the owner's decision (on the clawdh page); wire what runs and reports them.
	wireRemoteQueue()

	// If this machine answers to a panel, keep asking it what it is entitled
	// to. This is what makes taking an account back work without the panel
	// having to reach the machine: nothing is pushed, the machine asks.
	go watchPanel(ctx)

	if err := httpserver.Serve(ctx, srv, *port); err != nil {
		fmt.Fprintln(os.Stderr, "clawdh:", err)
		// An update that installed but could not restart leaves no
		// server running, and this process is about to be gone. The
		// notification is the only thing that will tell anyone.
		if errors.Is(err, httpserver.ErrRestartFailed) {
			_ = notify.Send("Updated, but couldn't restart on its own. Run `clawdh start` to bring it back.")
		}
		return 1
	}
	return 0
}

// maxLogBytes caps ~/.clawdh/clawdh.log. Every request is logged there and
// nothing ever rotated it, so a service left running with a page open
// grew it without bound — on the one file that also carries the only
// diagnostics when something goes wrong.
const maxLogBytes = 5 << 20

// capLogFile truncates the log if it has grown past maxLogBytes.
// Truncating is safe while launchd/systemd hold it open, because they
// opened it O_APPEND and will simply continue at the new end.
func capLogFile() {
	path, err := config.LogFile()
	if err != nil {
		return
	}
	info, err := os.Stat(path)
	if err != nil || info.Size() <= maxLogBytes {
		return
	}
	_ = os.Truncate(path, 0)
}

// resolveBinaryPath returns the absolute, symlink-resolved path to the
// currently running clawdh executable — what autostart registration and
// the manual start/stop lifecycle should point at, and what an update
// replaces.
func resolveBinaryPath() (string, error) {
	return service.SelfPath()
}

// startAutoUpdate keeps this installation current in the background.
//
// The service is the only part of clawdh that is always running, so it is
// the only place an update can happen without waiting for someone to
// remember. When one lands, the person gets a desktop notification —
// the binary changing underneath them is not something to discover by
// accident — and the server restarts into it.
func startAutoUpdate(ctx context.Context, srv *httpserver.Server) {
	// A server someone is watching in their own terminal must not
	// replace its binary and hand over to a detached process: that
	// would look exactly like `clawdh serve` quitting on its own, with a
	// daemon left behind that they never asked for. Updating belongs to
	// the copy started at login, whose output goes to the log file.
	if isTerminal(os.Stdout) {
		log.Print("automatic updates are off while running in a terminal")
		return
	}

	binaryPath, err := service.SelfPath()
	if err != nil {
		log.Printf("automatic updates are off: %v", err)
		return
	}
	// Finish whatever the last update could not: on Windows the
	// displaced executable can only be deleted once it is nobody's
	// running image, which is now.
	updater.CleanupOldBinary(binaryPath)

	up := updater.New(binaryPath)
	if err := up.Validate(); err != nil {
		log.Printf("automatic updates are off: %v", err)
		return
	}

	go up.Run(ctx, func(release updater.Release) {
		if err := notify.Send("Updated to " + release.Name + " — restarting in the background. Nothing you need to do."); err != nil {
			// A desktop that shows nothing is not a reason to keep
			// running the old binary.
			log.Printf("update: %v", err)
		}
		srv.RequestRestart()
	})
}

// isTerminal reports whether f is a character device — a real terminal
// rather than the log file the service's output is redirected to.
func isTerminal(f *os.File) bool {
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}
