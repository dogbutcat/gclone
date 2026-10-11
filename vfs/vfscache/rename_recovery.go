// gclone-local recovery for a rename whose old namespace cannot be restored.
package vfscache

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// renameRollbackError means source data remains at the new pathname. Restoring
// the staged destination in this state would overwrite the source generation.
type renameRollbackError struct{ error }

func (e *renameRollbackError) Unwrap() error { return e.error }

// RenameRecoveryError means neither original namespace can safely resume.
// VFS must fence the old target File even though no remote rename committed.
type RenameRecoveryError struct{ error }

func (e *RenameRecoveryError) Unwrap() error { return e.error }

// quarantineRenameSource is mandatory retirement, never ordinary unlink abort.
// The destination's stage already exists, so a second staging-directory failure
// cannot reactivate a source upload after a committed remote move. Existing
// source handles receive an error instead of acknowledging unpublishable writes.
func (c *Cache) quarantineRenameSource(oldName, newName, stage string, cause error) error {
	c.mu.Lock()
	item, _ := c._get(oldName)
	item.mu.Lock()
	item.unlinking, item.unlinkErr = true, cause
	id := item.writeBackID
	item.mu.Unlock()
	c.mu.Unlock()
	c.writeback.Remove(id)
	item.mu.Lock()
	for item.pendingAccesses != 0 || item.closing != nil {
		if item.closing != nil {
			closing := item.closing
			item.mu.Unlock()
			<-closing
			item.mu.Lock()
		} else {
			item.cond.Wait()
		}
	}
	dls := item.downloaders
	item.downloaders = nil
	item.mu.Unlock()
	if dls != nil {
		_ = dls.Close(nil)
	}
	dataName := oldName
	var rollbackErr *renameRollbackError
	if errors.As(cause, &rollbackErr) {
		dataName = newName
	}
	paths := []string{c.toOSPath(dataName), c.toOSPathMeta(oldName)}
	var recoveryErr error
	for i, path := range paths {
		if err := os.Rename(path, filepath.Join(stage, fmt.Sprintf("source-%d", i))); err != nil && !os.IsNotExist(err) {
			recoveryErr = errors.Join(recoveryErr, err)
		}
	}
	item.mu.Lock()
	defer item.mu.Unlock()
	item.unlinked, item.unlinking = true, false
	item.beingReset = false
	item.unlinkErr = errors.Join(cause, recoveryErr)
	item.cond.Broadcast()
	if recoveryErr != nil {
		// If a filesystem failure leaves data in a reload root, persist a
		// recovery fence instead of allowing restart to upload it at either
		// name. Keep the terminal Item in the map for the running process.
		info := item.info
		info.Recovery = fmt.Sprintf("vfs cache: rename recovery required in %q: %v", stage, item.unlinkErr)
		err := saveRecoveryInfo(c.toOSPathMeta(dataName), info)
		recoveryErr = errors.Join(recoveryErr, err)
		item.unlinkErr = errors.Join(item.unlinkErr, err)
	}
	return recoveryErr
}

// Persist the fence without truncating the previous metadata on ENOSPC. If
// this fails, leave both previous metadata and bytes available for recovery;
// the returned error explicitly prevents a claim of safe automatic reload.
func saveRecoveryInfo(path string, info Info) (err error) {
	return writeRecoveryInfo(path, func(out *os.File) error {
		return json.NewEncoder(out).Encode(info)
	})
}

// Keep the file transaction separate from encoding so partial-write failures
// can be exercised without filling the user's filesystem.
func writeRecoveryInfo(path string, write func(*os.File) error) (err error) {
	out, err := os.CreateTemp(filepath.Dir(path), ".gclone-recovery-*")
	if err != nil {
		return fmt.Errorf("cannot persist rename recovery fence: %w", err)
	}
	defer func() {
		_ = out.Close()
		_ = os.Remove(out.Name())
	}()
	if err = write(out); err == nil {
		err = out.Sync()
	}
	if closeErr := out.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(out.Name(), path)
	}
	if err != nil {
		return fmt.Errorf("cannot persist rename recovery fence: %w", err)
	}
	return nil
}
