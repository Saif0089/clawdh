package cli

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"clawdh/internal/config"
	"clawdh/internal/editors"
	"clawdh/internal/service"
	"clawdh/internal/shellrc"
	"clawdh/internal/statusline"
	"clawdh/internal/switching"
)

func cmdUninstall(args []string) int {
	fs := flag.NewFlagSet("uninstall", flag.ContinueOnError)
	port := fs.Int("port", config.DefaultPort, "port the service was installed on")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	binaryPath, err := resolveBinaryPath()
	if err != nil {
		fmt.Fprintln(os.Stderr, "clawdh:", err)
		return 1
	}

	svc := service.New(binaryPath, *port)
	if err := svc.Uninstall(); err != nil {
		fmt.Fprintln(os.Stderr, "clawdh: removing autostart registration failed:", err)
		return 1
	}
	fmt.Println("Stopped the service and removed autostart registration.")

	home, err := os.UserHomeDir()
	if err == nil {
		if err := shellrc.NewSyncer(home).RemoveAll(); err != nil {
			fmt.Fprintln(os.Stderr, "clawdh: warning: removing shell aliases failed:", err)
		} else {
			fmt.Println("Removed generated shell aliases.")
		}
		// Remove the in-session switch hook from the shared settings.json,
		// leaving every other hook the user has untouched.
		settings := filepath.Join(home, ".claude", "settings.json")
		if err := switching.RemoveUserPromptSubmitHook(settings); err != nil {
			fmt.Fprintln(os.Stderr, "clawdh: warning: removing the switch hook failed:", err)
		}
		// And give the person their own status line back.
		if err := statusline.Restore(settings, statusLineSavePath()); err != nil {
			fmt.Fprintln(os.Stderr, "clawdh: warning: restoring your status line failed:", err)
		}
		// Take clawdh back out of every editor's launch path. That setting names
		// this binary by absolute path, so leaving it behind would have the
		// Claude Code extension launching a file that is about to be deleted —
		// every conversation failing with "Claude Code process exited with
		// code 1", and no clawdh left to say why.
		removed := 0
		for _, ed := range editors.Installed(home) {
			if editors.WrapperPath(ed.Settings) == "" {
				continue
			}
			if err := editors.UnsetWrapper(ed.Settings); err != nil {
				fmt.Fprintf(os.Stderr, "clawdh: warning: leaving %s pointed at clawdh failed: %v\n", ed.Name, err)
				continue
			}
			removed++
		}
		if removed > 0 {
			fmt.Printf("Removed clawdh from %d editor(s); they launch Claude Code directly again.\n", removed)
		}
	}

	// Only remove a binary we installed. Running `./clawdh uninstall` from
	// a build tree should clean up the service, not delete someone's
	// build output.
	if !isInstalledBinary(binaryPath) {
		fmt.Printf("\nLeft %s in place (not in clawdh's install directory).\n", binaryPath)
		fmt.Println("Account data was left in place: newer accounts under ~/.clawdh/accounts, and any carried over from ccam still under ~/.ccam/accounts. Remove those directories yourself if you want a full wipe.")
		return 0
	}

	if err := deleteSelfBinary(binaryPath); err != nil {
		fmt.Fprintln(os.Stderr, "clawdh: warning: could not remove the installed binary at", binaryPath, "-", err)
		fmt.Println("You can delete it yourself; nothing else references it.")
	} else {
		fmt.Println("Removed", binaryPath)
	}

	fmt.Println("\nAccount data was left in place: newer accounts under ~/.clawdh/accounts, and any carried over from ccam still under ~/.ccam/accounts. Remove those directories yourself if you want a full wipe.")
	return 0
}

// isInstalledBinary reports whether path is the copy an installer put in
// clawdh's own per-user install directory.
func isInstalledBinary(path string) bool {
	installDir, err := config.InstallDir()
	if err != nil {
		return false
	}
	resolvedInstallDir, err := filepath.EvalSymlinks(installDir)
	if err != nil {
		resolvedInstallDir = installDir
	}
	return filepath.Dir(path) == resolvedInstallDir || filepath.Dir(path) == installDir
}
