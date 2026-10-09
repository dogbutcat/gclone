// gclone-local VFS integration: copied from rclone v1.75.1; see vfs/UPSTREAM.txt.

// Exercise deletion commit/abort and delayed upload completion without FUSE.
package vfscache

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/dogbutcat/gclone/vfs/vfscommon"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/config"
	"github.com/rclone/rclone/fstest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestItemStoreMissingCacheDoesNotPublish(t *testing.T) {
	r, c := newItemTestCache(t)
	c.opt.WriteBack = fs.Duration(time.Hour)
	_, obj, item := newFile(t, r, c, "missing")
	require.NoError(t, item.Open(obj))
	require.NoError(t, item.Truncate(0))
	_, err := item.WriteAt([]byte("new data"), 0)
	require.NoError(t, err)
	require.NoError(t, item.Close(nil))
	c.writeback.Remove(item.writeBackID)
	require.NoError(t, os.Remove(c.toOSPath("missing")))
	called := false
	err = item.store(context.Background(), func(fs.Object) { called = true })
	assert.ErrorIs(t, err, fs.ErrorObjectNotFound)
	assert.False(t, called)
	assert.True(t, item.IsDirty(), "missing data must not be acknowledged as stored")
}

func TestCacheUnlinkFailurePreservesWriteback(t *testing.T) {
	r, c := newItemTestCache(t)
	c.opt.WriteBack = fs.Duration(time.Hour)
	_, obj, item := newFile(t, r, c, "failure")
	require.NoError(t, item.Open(obj))
	require.NoError(t, item.Truncate(0))
	_, err := item.WriteAt([]byte("retained write"), 0)
	require.NoError(t, err)
	done := make(chan fs.Object, 1)
	require.NoError(t, item.Close(func(o fs.Object) { done <- o }))
	deleteErr := errors.New("remote deletion rejected")
	removed, err := c.Unlink("failure", func(o fs.Object) error {
		assert.Same(t, obj, o)
		return deleteErr
	})
	require.ErrorIs(t, err, deleteErr)
	assert.False(t, removed)
	assert.Same(t, item, c.Item("failure"))
	contents, err := os.ReadFile(c.toOSPath("failure"))
	require.NoError(t, err)
	assert.Equal(t, "retained write", string(contents))
	assert.FileExists(t, c.toOSPathMeta("failure"))
	require.Len(t, c.writeback.Queue(), 1)
	require.NoError(t, c.writeback.SetExpiry(item.writeBackID, time.Now(), 0))
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("writeback was not resumed after failed unlink")
	}
	checkObject(t, r, "failure", "retained write")
}

type unlinkBlockingPut struct {
	fs.Fs
	entered, canceled, release chan struct{}
	created                    fs.Object
}

func (f *unlinkBlockingPut) Features() *fs.Features {
	features := *f.Fs.Features()
	features.Copy = nil
	features.PartialUploads = false
	return &features
}

func (f *unlinkBlockingPut) Put(ctx context.Context, in io.Reader, src fs.ObjectInfo, options ...fs.OpenOption) (fs.Object, error) {
	data, err := io.ReadAll(in)
	if err != nil {
		return nil, err
	}
	close(f.entered)
	<-ctx.Done()
	close(f.canceled)
	<-f.release
	// Deliberately finish after cancellation, like an uncancellable backend.
	o, err := f.Fs.Put(context.WithoutCancel(ctx), bytes.NewReader(data), src, options...)
	f.created = o
	return o, err
}

func TestCacheUnlinkDrainsUncancellableUpload(t *testing.T) {
	for _, reject := range []bool{false, true} {
		t.Run(map[bool]string{false: "commit", true: "abort"}[reject], func(t *testing.T) {
			r := fstest.NewRun(t)
			backend := &unlinkBlockingPut{Fs: r.Fremote, entered: make(chan struct{}), canceled: make(chan struct{}), release: make(chan struct{})}
			opt := vfscommon.Opt
			opt.WriteBack = fs.Duration(time.Hour)
			opt.HandleCaching, opt.CachePollInterval = 0, 0
			ctx, cancel := context.WithCancel(context.Background())
			c, err := New(ctx, backend, &opt, nil)
			require.NoError(t, err)
			t.Cleanup(func() { cancel(); require.NoError(t, c.CleanUp()) })
			item := c.Item("upload")
			require.NoError(t, item.Open(nil))
			_, err = item.WriteAt([]byte("late upload"), 0)
			require.NoError(t, err)
			require.NoError(t, item.Close(nil))
			require.NoError(t, c.writeback.SetExpiry(item.writeBackID, time.Now(), 0))
			select {
			case <-backend.entered:
			case <-time.After(5 * time.Second):
				t.Fatal("upload did not start")
			}
			result := make(chan error, 1)
			deleteErr := errors.New("delete denied")
			go func() {
				removed, err := c.Unlink("upload", func(o fs.Object) error {
					if o == nil || o != backend.created {
						return errors.New("unlink lost the completed upload object")
					}
					if reject {
						return deleteErr
					}
					return o.Remove(context.Background())
				})
				if removed == reject {
					err = errors.Join(err, errors.New("wrong deletion commit outcome"))
				}
				result <- err
			}()
			select {
			case <-backend.canceled:
			case <-time.After(5 * time.Second):
				t.Fatal("unlink did not cancel upload")
			}
			close(backend.release)
			select {
			case err := <-result:
				if reject {
					require.ErrorIs(t, err, deleteErr)
					assert.Same(t, backend.created, item.o)
					assert.True(t, item.IsDirty())
					require.Len(t, c.writeback.Queue(), 1)
					c.writeback.Remove(item.writeBackID)
				} else {
					require.NoError(t, err)
					assert.Empty(t, c.writeback.Queue())
					_, err = backend.Fs.NewObject(context.Background(), "upload")
					assert.ErrorIs(t, err, fs.ErrorObjectNotFound)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("unlink did not drain upload")
			}
		})
	}
}

func TestCacheUnlinkCannotStage(t *testing.T) {
	_, c := newItemTestCache(t)
	item := c.Item("stage-failure")
	require.NoError(t, item.Open(nil))
	_, err := item.WriteAt([]byte("preserved"), 0)
	require.NoError(t, err)
	// Use a regular file as cache parent to deterministically fail MkdirAll.
	oldCacheDir := config.GetCacheDir()
	blocked := filepath.Join(t.TempDir(), "not-a-directory")
	require.NoError(t, os.WriteFile(blocked, nil, 0600))
	require.NoError(t, config.SetCacheDir(blocked))
	t.Cleanup(func() { require.NoError(t, config.SetCacheDir(oldCacheDir)) })
	called := false
	removed, err := c.Unlink("stage-failure", func(fs.Object) error { called = true; return nil })
	require.Error(t, err)
	assert.False(t, called, "remote must not be deleted if staging failed")
	assert.False(t, removed)
	buf := make([]byte, 9)
	_, err = item.ReadAt(buf, 0)
	require.NoError(t, err)
	assert.Equal(t, "preserved", string(buf))
	require.NoError(t, item.Close(nil))
}

func TestCacheUnlinkCleanupFailureCannotReload(t *testing.T) {
	r, c := newItemTestCache(t)
	item := c.Item("retired")
	require.NoError(t, item.Open(nil))
	_, err := item.WriteAt([]byte("old bytes"), 0)
	require.NoError(t, err)
	var leftover string
	removed, err := c.Unlink("retired", func(fs.Object) error {
		stages, err := filepath.Glob(filepath.Join(config.GetCacheDir(), "vfs-unlinked", "unlink-*", "0"))
		if err != nil {
			return err
		}
		for _, path := range stages {
			data, err := os.ReadFile(path)
			if err == nil && string(data) == "old bytes" {
				leftover = path
				break
			}
		}
		if leftover == "" {
			return errors.New("staged bytes not found")
		}
		if err := os.Rename(leftover, leftover+".retained"); err != nil {
			return err
		}
		if err := os.Mkdir(leftover, 0700); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(leftover, "block-remove"), nil, 0600)
	})
	require.NoError(t, err)
	require.True(t, removed)
	t.Cleanup(func() { require.NoError(t, os.RemoveAll(filepath.Dir(leftover))) })
	require.NoError(t, item.Close(nil))
	assert.FileExists(t, leftover+".retained")
	assert.NoFileExists(t, c.toOSPath("retired"))
	assert.NoFileExists(t, c.toOSPathMeta("retired"))
	// A fresh Cache scans exactly the roots that a restarted process would.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	restarted, err := New(ctx, r.Fremote, c.opt, nil)
	require.NoError(t, err)
	assert.Empty(t, restarted.writeback.Queue())
	replacement := restarted.Item("retired")
	require.NoError(t, replacement.Open(nil))
	_, err = replacement.WriteAt([]byte("new bytes"), 0)
	require.NoError(t, err)
	require.NoError(t, replacement.Close(nil))
	checkObject(t, r, "retired", "new bytes")
}

func TestCacheUnlinkRollbackFailureRejectsWrites(t *testing.T) {
	_, c := newItemTestCache(t)
	item := c.Item("rollback-failure")
	require.NoError(t, item.Open(nil))
	_, err := item.WriteAt([]byte("recover me"), 0)
	require.NoError(t, err)
	removed, err := c.Unlink("rollback-failure", func(fs.Object) error {
		// A directory obstructs restoration of the staged data pathname.
		if err := os.Mkdir(c.toOSPath("rollback-failure"), 0700); err != nil {
			return err
		}
		return os.ErrPermission
	})
	require.ErrorIs(t, err, os.ErrPermission)
	assert.False(t, removed)
	_, err = item.WriteAt([]byte("must fail"), 0)
	require.ErrorIs(t, err, os.ErrPermission)
	require.ErrorIs(t, item.Truncate(0), os.ErrPermission)
	require.ErrorIs(t, item.Sync(), os.ErrPermission)
	buf := make([]byte, 10)
	_, err = item.ReadAt(buf, 0)
	require.NoError(t, err)
	assert.Equal(t, "recover me", string(buf))
	require.ErrorIs(t, item.Close(nil), os.ErrPermission)
	stages, err := filepath.Glob(filepath.Join(config.GetCacheDir(), "vfs-unlinked", "unlink-*", "0"))
	require.NoError(t, err)
	for _, path := range stages {
		if data, err := os.ReadFile(path); err == nil && string(data) == "recover me" {
			require.NoError(t, os.RemoveAll(filepath.Dir(path)))
		}
	}
}

func TestCacheUnlinkRetiresResetWait(t *testing.T) {
	_, c := newItemTestCache(t)
	item := c.Item("reset-failure")
	item.mu.Lock()
	item.beingReset = true
	item.mu.Unlock()
	c.errItems["reset-failure"] = errors.New("out of space")
	removed, err := c.Unlink("reset-failure", func(fs.Object) error { return nil })
	require.NoError(t, err)
	require.True(t, removed)
	assert.NotContains(t, c.errItems, "reset-failure")
	done := make(chan error, 1)
	go func() { done <- item.Open(nil) }()
	select {
	case err := <-done:
		require.ErrorIs(t, err, os.ErrNotExist)
	case <-time.After(time.Second):
		t.Fatal("retired cache item is still waiting for reset")
	}
}

func TestCacheUnlinkReservesRenameDestination(t *testing.T) {
	_, c := newItemTestCache(t)
	for _, name := range []string{"source", "target"} {
		item := c.Item(name)
		require.NoError(t, item.Open(nil))
		_, err := item.WriteAt([]byte(name+" bytes"), 0)
		require.NoError(t, err)
		require.NoError(t, item.Close(nil))
	}
	removed, err := c.Unlink("target", func(fs.Object) error {
		assert.ErrorIs(t, c.Rename("source", "target", nil), syscall.EBUSY)
		// A reservation is per pathname, not a global mutation lock.
		otherRemoved, err := c.Unlink("unrelated", func(fs.Object) error { return nil })
		assert.NoError(t, err)
		assert.True(t, otherRemoved)
		return os.ErrPermission
	})
	require.ErrorIs(t, err, os.ErrPermission)
	assert.False(t, removed)
	for _, name := range []string{"source", "target"} {
		data, err := os.ReadFile(c.toOSPath(name))
		require.NoError(t, err)
		assert.Equal(t, name+" bytes", string(data))
	}
	assert.Empty(t, c.mutations)
}
