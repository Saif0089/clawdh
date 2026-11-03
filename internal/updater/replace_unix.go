//go:build !windows

package updater

import (
	"context"
	"fmt"
	"os"
)

// replaceBinary moves the verified download over the running
// executable. On Unix that is a plain rename: the running process keeps
// the old inode it was loaded from, so nothing under it changes until
// it restarts.
func replaceBinary(_ context.Context, from, to string) error {
	if err := os.Rename(from, to); err != nil {
		return fmt.Errorf("installing the update over %s: %w", to, err)
	}
	return nil
}
