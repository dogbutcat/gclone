// gclone-local regression: rename must retire the overwritten cache lifetime.
package vfscache

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/rclone/rclone/fs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCacheRenameOverReadOnlyLifetime(t *testing.T) {
	for _, grace := range []bool{false, true} {
		t.Run(map[bool]string{false: "open-reader", true: "grace-reader"}[grace], func(t *testing.T) {
			r, c := newItemTestCache(t)
			c.opt.WriteBack = fs.Duration(time.Hour)
			oldTime, newTime := time.Unix(1791715320, 272000000), time.Unix(1791715322, 326000000)
			r.WriteObject(context.Background(), "target", "old data", oldTime)
			o, err := r.Fremote.NewObject(context.Background(), "target")
			require.NoError(t, err)
			old := c.Item("target")
			require.NoError(t, old.Open(o))
			buf := make([]byte, 8)
			_, err = old.ReadAt(buf, 0)
			require.NoError(t, err)
			if grace {
				c.opt.HandleCaching = fs.Duration(time.Hour)
				require.NoError(t, old.Close(nil))
			}
			fresh := c.Item("temporary")
			itemWrite(t, fresh, "new data")
			require.NoError(t, fresh.Close(nil))
			c.SetModTime("temporary", newTime)
			require.NoError(t, c.Rename("temporary", "target", nil))
			require.Same(t, fresh, c.Item("target"))
			before, err := os.Stat(c.toOSPath("target"))
			require.NoError(t, err)
			beforeMeta, err := os.ReadFile(c.toOSPathMeta("target"))
			require.NoError(t, err)
			if grace {
				old.closeAfterGrace()
			} else {
				_, err = old.ReadAt(buf, 0)
				require.NoError(t, err)
				assert.Equal(t, "old data", string(buf))
				require.NoError(t, old.Close(nil))
			}
			after, err := os.Stat(c.toOSPath("target"))
			require.NoError(t, err)
			afterMeta, err := os.ReadFile(c.toOSPathMeta("target"))
			require.NoError(t, err)
			assert.True(t, os.SameFile(before, after))
			assert.Equal(t, before.Size(), after.Size())
			assert.Equal(t, before.ModTime(), after.ModTime(), "old close must not change replacement mtime")
			var beforeInfo, afterInfo Info
			require.NoError(t, json.Unmarshal(beforeMeta, &beforeInfo))
			require.NoError(t, json.Unmarshal(afterMeta, &afterInfo))
			assert.Equal(t, beforeInfo, afterInfo, "old close must not overwrite the replacement sidecar")
			data, err := os.ReadFile(c.toOSPath("target"))
			require.NoError(t, err)
			assert.Equal(t, "new data", string(data))
			c.writeback.Remove(fresh.writeBackID)
		})
	}
}

func TestRenameUnsafeRollbackPreservesBothGenerations(t *testing.T) {
	_, c := newItemTestCache(t)
	c.opt.WriteBack = fs.Duration(time.Hour)
	source, target := c.Item("source"), c.Item("target")
	itemWrite(t, source, "source bytes")
	itemWrite(t, target, "target bytes")
	m, err := c.BeginRename("source", "target")
	require.NoError(t, err)
	committed, _, err := m.apply(func(fs.Object) (fs.Object, error) { return nil, nil }, false,
		func(oldName, newName string, _ fs.Object) error {
			// The first backing move succeeded; inject failure of the metadata
			// move AND source-data rollback at the filesystem operation boundary.
			require.NoError(t, os.Rename(c.toOSPath(oldName), c.toOSPath(newName)))
			return &renameRollbackError{os.ErrPermission}
		})
	m.Close()
	assert.False(t, committed, "there was no remote move or new namespace commit")
	require.Error(t, err)
	assert.False(t, target.unlinked, "successful source quarantine permits target rollback")
	assert.True(t, source.unlinked)
	assert.Same(t, target, c.Item("target"))
	data, readErr := os.ReadFile(c.toOSPath("target"))
	require.NoError(t, readErr)
	assert.Equal(t, "target bytes", string(data))
	_, writeErr := source.WriteAt([]byte("unpublishable"), 0)
	require.Error(t, writeErr)
	require.Error(t, source.Close(nil))
	_, writeErr = target.WriteAt([]byte("valid target"), 0)
	require.NoError(t, writeErr)
	require.NoError(t, target.Close(nil))
	c.writeback.Remove(target.writeBackID)
}

func TestRecoveryFencePartialWritePreservesMetadata(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metadata")
	original := []byte(`{"Dirty":true,"Size":12}`)
	require.NoError(t, os.WriteFile(path, original, 0600))
	err := writeRecoveryInfo(path, func(out *os.File) error {
		_, err := out.Write([]byte(`{"Recovery":`))
		require.NoError(t, err)
		return syscall.ENOSPC
	})
	require.ErrorIs(t, err, syscall.ENOSPC)
	actual, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, original, actual, "failed fence must not corrupt previous sidecar")
	temps, err := filepath.Glob(filepath.Join(filepath.Dir(path), ".gclone-recovery-*"))
	require.NoError(t, err)
	assert.Empty(t, temps)
}

func TestCacheRenameAbortPreservesBothLifetimes(t *testing.T) {
	_, c := newItemTestCache(t)
	c.opt.WriteBack = fs.Duration(time.Hour)
	source, target := c.Item("source"), c.Item("target")
	itemWrite(t, source, "source bytes")
	itemWrite(t, target, "target bytes")
	require.NoError(t, source.Close(nil))
	require.NoError(t, target.Close(nil))
	t.Cleanup(func() { c.writeback.Remove(source.writeBackID); c.writeback.Remove(target.writeBackID) })
	m, err := c.BeginRename("source", "target")
	require.NoError(t, err)
	moveErr := errors.New("remote rejected rename")
	committed, _, err := m.Apply(func(fs.Object) (fs.Object, error) { return nil, moveErr })
	m.Close()
	assert.False(t, committed)
	require.ErrorIs(t, err, moveErr)
	assert.Same(t, source, c.Item("source"))
	assert.Same(t, target, c.Item("target"))
	for _, name := range []string{"source", "target"} {
		data, err := os.ReadFile(c.toOSPath(name))
		require.NoError(t, err)
		assert.Equal(t, name+" bytes", string(data))
	}
	assert.Len(t, c.writeback.Queue(), 2)

	// Force the second cache rename to fail after the data file moved.
	meta := c.toOSPathMeta("source")
	metadata, err := os.ReadFile(meta)
	require.NoError(t, err)
	require.NoError(t, os.Remove(meta))
	require.NoError(t, os.Mkdir(meta, 0700))
	require.Error(t, c.Rename("source", "target", nil))
	assert.Equal(t, "source", source.name)
	assert.False(t, target.unlinked)
	for _, name := range []string{"source", "target"} {
		data, err := os.ReadFile(c.toOSPath(name))
		require.NoError(t, err)
		assert.Equal(t, name+" bytes", string(data))
	}
	require.NoError(t, os.Remove(meta))
	require.NoError(t, os.WriteFile(meta, metadata, 0600))
}

func TestCacheRenameGraceCloseDuringInstall(t *testing.T) {
	r, c := newItemTestCache(t)
	c.opt.WriteBack, c.opt.HandleCaching = fs.Duration(time.Hour), fs.Duration(time.Hour)
	_, o, old := newFile(t, r, c, "target")
	require.NoError(t, old.Open(o))
	require.NoError(t, old.Close(nil))
	source := c.Item("source")
	itemWrite(t, source, "new data")
	require.NoError(t, source.Close(nil))
	t.Cleanup(func() { c.writeback.Remove(source.writeBackID) })
	m, err := c.BeginRename("source", "target")
	require.NoError(t, err)
	defer m.Close()
	_, err = c.retire("target", func(fs.Object, string) (bool, error) {
		require.NoError(t, c.rename("source", "target", nil))
		before, err := os.Stat(c.toOSPath("target"))
		require.NoError(t, err)
		// Force the timer callback into the unlinking-but-not-yet-unlinked gap.
		old.closeAfterGrace()
		after, err := os.Stat(c.toOSPath("target"))
		require.NoError(t, err)
		assert.Equal(t, before.ModTime(), after.ModTime())
		return true, nil
	})
	require.NoError(t, err)
}

func TestCacheRenamePostCommitFailureFencesSource(t *testing.T) {
	r, c := newItemTestCache(t)
	c.opt.WriteBack = fs.Duration(time.Hour)
	_, o, target := newFile(t, r, c, "target")
	require.NoError(t, target.Open(o))
	source := c.Item("source")
	itemWrite(t, source, "pending source")
	meta := c.toOSPathMeta("source")
	require.NoError(t, os.Remove(meta))
	require.NoError(t, os.Mkdir(meta, 0700))
	m, err := c.BeginRename("source", "target")
	require.NoError(t, err)
	committed, _, err := m.Apply(func(fs.Object) (fs.Object, error) { return o, nil })
	m.Close()
	assert.True(t, committed)
	require.Error(t, err)
	assert.True(t, source.unlinked)
	assert.True(t, target.unlinked)
	assert.Empty(t, c.writeback.Queue())
	_, writeErr := source.WriteAt([]byte("lost"), 0)
	require.Error(t, writeErr)
	require.Error(t, source.Truncate(0))
	require.Error(t, source.Sync())
	require.Error(t, source.Close(nil))
	require.Error(t, target.Close(nil))
	assert.NoFileExists(t, c.toOSPath("source"))
	assert.NoFileExists(t, c.toOSPath("target"))
}

func TestRenameRecoveryFailureNeverResumesUpload(t *testing.T) {
	_, c := newItemTestCache(t)
	c.opt.WriteBack = fs.Duration(time.Hour)
	source := c.Item("source")
	itemWrite(t, source, "recovery bytes")
	require.NoError(t, source.Close(nil))
	// A non-directory forces quarantine failure, including after remote commit.
	stage := filepath.Join(t.TempDir(), "blocked")
	require.NoError(t, os.WriteFile(stage, nil, 0600))
	require.Error(t, c.quarantineRenameSource("source", "target", stage, errors.New("cache rename failed")))
	assert.Empty(t, c.writeback.Queue())
	require.Error(t, source.Open(nil))
	data, err := os.ReadFile(c.toOSPath("source"))
	require.NoError(t, err)
	assert.Equal(t, "recovery bytes", string(data))
	// A reloaded Item is also fenced and cannot enqueue old-name writeback.
	reloaded := newItem(c, "source")
	require.Error(t, reloaded.Open(nil))
	assert.True(t, reloaded.unlinked)
	assert.NotEmpty(t, reloaded.info.Recovery)
	assert.Empty(t, c.writeback.Queue())
}
