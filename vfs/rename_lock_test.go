// Exercise pathname revalidation and lock ordering during atomic replacement.
package vfs

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dogbutcat/gclone/vfs/vfscache/writeback"
	"github.com/dogbutcat/gclone/vfs/vfscommon"
	"github.com/rclone/rclone/fs"
	"github.com/stretchr/testify/require"
	"golang.org/x/text/unicode/norm"
)

func newRenameContentionVFS(t *testing.T, insensitive ...bool) (*VFS, func(string, string) *File) {
	t.Helper()
	opt := vfscommon.Opt
	opt.CacheMode, opt.WriteBack = vfscommon.CacheModeFull, fs.Duration(time.Hour)
	if len(insensitive) != 0 {
		opt.CaseInsensitive = insensitive[0]
	}
	_, v := newTestVFSOpt(t, &opt)
	t.Cleanup(func() {
		for _, entry := range v.cache.Queue()["queue"].([]writeback.QueueInfo) {
			_ = v.cache.QueueSetExpiry(entry.ID, time.Now(), 0)
		}
	})
	return v, func(name, data string) *File {
		t.Helper()
		h, err := v.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		require.NoError(t, err)
		_, err = h.Write([]byte(data))
		require.NoError(t, err)
		require.NoError(t, h.Sync())
		require.NoError(t, h.Close())
		return h.Node().(*File)
	}
}

func awaitRename(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("rename did not complete after releasing the lifecycle operation")
	}
}

func TestRenameWaitsForDestinationOperation(t *testing.T) {
	for _, targetFirst := range []bool{false, true} {
		t.Run(fmt.Sprint(targetFirst), func(t *testing.T) {
			v, create := newRenameContentionVFS(t)
			var target *File
			if targetFirst {
				target = create("target", "old")
			}
			create("source", "new")
			if !targetFirst {
				target = create("target", "old")
			}
			target.muRW.Lock()
			var once sync.Once
			release := func() { once.Do(target.muRW.Unlock) }
			defer release()
			done := make(chan error, 1)
			go func() { done <- v.Rename("source", "target") }()
			select {
			case err := <-done:
				t.Fatalf("returned during destination operation: %v", err)
			case <-time.After(100 * time.Millisecond):
			}
			// Waiting on this file must not reserve the cache path or block
			// operations on independent names.
			mutation, err := v.cache.BeginRename("probe", "target")
			require.NoError(t, err)
			mutation.Close()
			create("other", "independent")
			require.NoError(t, v.Rename("other", "elsewhere"))
			release()
			awaitRename(t, done)
			data, err := v.ReadFile("target")
			require.NoError(t, err)
			require.Equal(t, "new", string(data))
		})
	}
}

func TestRenameRevalidatesSourceAfterWait(t *testing.T) {
	v, create := newRenameContentionVFS(t)
	target := create("target", "old target") // lowest inode: wait here first
	create("source", "old source")
	create("replacement", "latest source")
	target.muRW.Lock()
	var once sync.Once
	release := func() { once.Do(target.muRW.Unlock) }
	defer release()
	done := make(chan error, 1)
	go func() { done <- v.Rename("source", "target") }()
	select {
	case err := <-done:
		t.Fatalf("returned before source could be replaced: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	require.NoError(t, v.Rename("replacement", "source"))
	release()
	awaitRename(t, done)
	data, err := v.ReadFile("target")
	require.NoError(t, err)
	require.Equal(t, "latest source", string(data), "retry must resolve the pathname, not move the retired File")
}

func TestRenameOpposingReplacements(t *testing.T) {
	v, create := newRenameContentionVFS(t)
	for i := range 20 {
		a, b := fmt.Sprintf("a-%d", i), fmt.Sprintf("b-%d", i)
		create(a, "a")
		create(b, "b")
		start := make(chan struct{})
		done := make(chan error, 2)
		go func() { <-start; done <- v.Rename(a, b) }()
		go func() { <-start; done <- v.Rename(b, a) }()
		close(start)
		awaitRename(t, done)
		awaitRename(t, done)
		// Either serial order is valid. Exactly one pathname owns the survivor.
		_, aErr := v.Stat(a)
		_, bErr := v.Stat(b)
		require.True(t, (aErr == nil && bErr == ENOENT) || (aErr == ENOENT && bErr == nil))
	}
}

func TestRenameWaitsForCacheReservation(t *testing.T) {
	for _, targetExists := range []bool{false, true} {
		t.Run(fmt.Sprint(targetExists), func(t *testing.T) {
			v, create := newRenameContentionVFS(t)
			create("source", "new")
			if targetExists {
				create("target", "old")
			}
			mutation, err := v.cache.BeginRename("another-source", "target")
			require.NoError(t, err)
			defer mutation.Close()
			done := make(chan error, 1)
			go func() { done <- v.Rename("source", "target") }()
			select {
			case err := <-done:
				t.Fatalf("returned during cache reservation: %v", err)
			case <-time.After(100 * time.Millisecond):
			}
			// A waiter may not pin the source while waiting on a cache owner.
			source, err := v.Stat("source")
			require.NoError(t, err)
			require.True(t, source.(*File).muRW.TryLock())
			source.(*File).muRW.Unlock()
			mutation.Close()
			awaitRename(t, done)
			data, err := v.ReadFile("target")
			require.NoError(t, err)
			require.Equal(t, "new", string(data))
		})
	}
}

func TestRenameNormalizedPathnames(t *testing.T) {
	for _, mode := range []string{"source-alias", "target-alias", "same-file-alias", "case-source-alias", "case-target-alias", "case-same-file-alias"} {
		t.Run(mode, func(t *testing.T) {
			insensitive := strings.HasPrefix(mode, "case-")
			v, create := newRenameContentionVFS(t, insensitive)
			nfc := "caf\u00e9"
			nfd := norm.NFD.String(nfc)
			if insensitive {
				nfc, nfd = "source-name", "SOURCE-NAME"
			}
			var from, to string
			switch strings.TrimPrefix(mode, "case-") {
			case "source-alias":
				create(nfc, "new")
				from, to = nfd, "target"
			case "target-alias":
				create(nfc, "old")
				create("source", "new")
				from, to = "source", nfd
			case "same-file-alias":
				create(nfc, "new")
				from, to = nfc, nfd
			}
			done := make(chan error, 1)
			go func() { done <- v.Rename(from, to) }()
			awaitRename(t, done)
			data, err := v.ReadFile(to)
			require.NoError(t, err)
			require.Equal(t, "new", string(data))
			root, err := v.Root()
			require.NoError(t, err)
			nodes, err := root.ReadDirAll()
			require.NoError(t, err)
			require.Len(t, nodes, 1, "no stale source or displaced alias remains")
		})
	}
}

func TestRenameTwoSourcesToAbsentTarget(t *testing.T) {
	v, create := newRenameContentionVFS(t)
	for i := range 20 {
		a, b, target := fmt.Sprintf("left-%d", i), fmt.Sprintf("right-%d", i), fmt.Sprintf("target-%d", i)
		left, right := create(a, "left"), create(b, "right")
		start, done := make(chan struct{}), make(chan error, 2)
		go func() { <-start; done <- v.Rename(a, target) }()
		go func() { <-start; done <- v.Rename(b, target) }()
		close(start)
		awaitRename(t, done)
		awaitRename(t, done)
		current, err := v.Stat(target)
		require.NoError(t, err)
		loser := left
		if current == left {
			loser = right
		} else {
			require.Same(t, right, current)
		}
		loser.mu.RLock()
		retired := loser.unlinked
		loser.mu.RUnlock()
		require.True(t, retired, "the overwritten generation must be fenced even when the target began absent")
		loser.setObject(nil)
		after, err := v.Stat(target)
		require.NoError(t, err)
		require.Same(t, current, after)
		data, err := v.ReadFile(target)
		require.NoError(t, err)
		if current == left {
			require.Equal(t, "left", string(data))
		} else {
			require.Equal(t, "right", string(data))
		}
	}
}
