// Preserve namespace ownership across legacy removal and delayed captured opens.
//go:build linux || darwin

package vfscache

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/dogbutcat/gclone/vfs/vfscommon"
	"github.com/rclone/rclone/fs"
	"github.com/stretchr/testify/require"
)

func ownershipCache(t *testing.T, writeBack time.Duration) *Cache {
	t.Helper()
	opt := vfscommon.Opt
	opt.CachePollInterval, opt.HandleCaching = 0, 0
	opt.WriteBack = fs.Duration(writeBack)
	_, c := newTestCacheOpt(t, opt)
	return c
}

func TestInitializerAfterLegacyRemoveCannotOverwriteRename(t *testing.T) {
	c := ownershipCache(t, 0)
	source := c.Item("source")
	require.NoError(t, source.Open(nil))
	_, err := source.WriteAt([]byte("source-payload"), 0)
	require.NoError(t, err)
	require.NoError(t, source.Close(nil))
	target := c.Item("destination")
	require.NoError(t, target.Open(nil))
	_, err = target.WriteAt([]byte("old-target"), 0)
	require.NoError(t, err)
	require.NoError(t, target.Close(nil))

	// The transaction owns the names before a cold destination lookup starts.
	mutation, err := c.BeginRename("source", "destination")
	require.NoError(t, err)
	defer mutation.Close()
	moveEntered, allowMove := make(chan struct{}), make(chan struct{})
	renamed := make(chan error, 1)
	go func() {
		_, _, moveErr := mutation.Apply(func(fs.Object) (fs.Object, error) {
			close(moveEntered)
			<-allowMove
			return nil, nil
		})
		renamed <- moveErr
	}()
	<-moveEntered
	moveReleased := false
	releaseMove := func() {
		if !moveReleased {
			moveReleased = true
			close(allowMove)
		}
	}
	defer releaseMove()
	// A legacy removal currently detaches the staged target despite the
	// outstanding explicit reservation, exposing the name to initialization.
	c.Remove("destination")
	c.mu.Lock()
	protected := c.item["destination"] == target
	c.mu.Unlock()
	if protected {
		// Removal must not expose a name already owned by the rename. This
		// closes the race before a destructive cold constructor can start.
		releaseMove()
		require.NoError(t, <-renamed)
		require.Same(t, source, c.Item("destination"))
		data, readErr := os.ReadFile(c.toOSPath("destination"))
		require.NoError(t, readErr)
		require.Equal(t, "source-payload", string(data))
		return
	}
	data, meta := c.toOSPath("destination"), c.toOSPathMeta("destination")
	require.NoError(t, os.MkdirAll(filepath.Dir(meta), 0700))
	require.NoError(t, os.WriteFile(data, []byte("old"), 0600))
	require.NoError(t, syscall.Mkfifo(meta, 0600))
	loaded := make(chan *Item, 1)
	go func() { loaded <- c.Item("destination") }()

	var writer *os.File
	require.Eventually(t, func() bool {
		var openErr error
		writer, openErr = os.OpenFile(meta, os.O_WRONLY|syscall.O_NONBLOCK, 0600)
		if errors.Is(openErr, syscall.ENXIO) {
			return false
		}
		require.NoError(t, openErr)
		return openErr == nil
	}, time.Second, time.Millisecond)

	released := false
	release := func() {
		if !released {
			released = true
			_ = json.NewEncoder(writer).Encode(Info{Size: 3, ModTime: time.Now(), ATime: time.Now()})
			_ = writer.Close()
		}
	}
	defer release()
	// Keep the blocked reader on the old FIFO inode while the real rename
	// sees a regular destination metadata file, as its contract requires.
	readyMeta, err := json.Marshal(Info{Size: 3, ModTime: time.Now(), ATime: time.Now()})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(meta+".regular", readyMeta, 0600))
	require.NoError(t, os.Rename(meta+".regular", meta))
	releaseMove()

	earlyCommit := false
	select {
	case err := <-renamed:
		require.NoError(t, err)
		earlyCommit = true
	case <-time.After(time.Second):
	}
	release()
	cold := <-loaded
	if !earlyCommit {
		require.NoError(t, <-renamed)
	}
	current := c.Item("destination")
	t.Logf("rename completed before initializer release=%v; source=%p cold=%p current=%p", earlyCommit, source, cold, current)
	require.Same(t, source, current, "late cold initialization displaced the committed renamed Item")
}

func TestCapturedOpenAfterEvictionPreservesDirtyReplacement(t *testing.T) {
	c := ownershipCache(t, time.Hour)
	captured := c.Item("file")
	c.purgeOld(0)
	replacement := c.Item("file")
	require.NotSame(t, captured, replacement)
	require.NoError(t, replacement.Open(nil))
	_, err := replacement.WriteAt([]byte("replacement-dirty-bytes"), 0)
	require.NoError(t, err)
	require.NoError(t, replacement.Close(nil))
	defer c.writeback.Remove(replacement.writeBackID)

	openErr := captured.Open(nil)
	if openErr == nil {
		defer captured.Close(nil)
	}
	current := c.Item("file")
	got, readErr := os.ReadFile(c.toOSPath("file"))
	t.Logf("captured open=%v; captured=%p replacement=%p current=%p bytes=%q replacementDirty=%v currentDirty=%p", openErr, captured, replacement, current, got, replacement.IsDirty(), c.DirtyItem("file"))
	require.NoError(t, readErr)
	if current != replacement {
		t.Error("a delayed captured opener replaced the dirty current binding")
	}
	if string(got) != "replacement-dirty-bytes" {
		t.Errorf("dirty replacement bytes changed to %q", got)
	}
}
