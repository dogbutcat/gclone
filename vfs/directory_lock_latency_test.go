// Keep cached sibling lookups independent from slow listing and metadata I/O.
package vfs

import (
	"context"
	"errors"
	"os"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dogbutcat/gclone/vfs/vfscommon"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fstest"
	"github.com/stretchr/testify/require"
)

type directoryLatencyRemote struct {
	fs.Fs
	features                 fs.Features
	blockList                atomic.Bool
	omitSlow                 atomic.Bool
	failList                 atomic.Bool
	listEntered, releaseList chan struct{}
	setEntered, releaseSet   chan struct{}
	observedOmission         chan struct{}
	omissionOnce             sync.Once
}

func (r *directoryLatencyRemote) Features() *fs.Features { return &r.features }

func (r *directoryLatencyRemote) List(ctx context.Context, dir string) (fs.DirEntries, error) {
	entries, err := r.Fs.List(ctx, dir)
	if err != nil {
		return nil, err
	}
	if r.blockList.CompareAndSwap(true, false) {
		close(r.listEntered)
		select {
		case <-r.releaseList:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if r.failList.Load() {
		return nil, errors.New("injected listing failure")
	}
	out := make(fs.DirEntries, 0, len(entries))
	for _, entry := range entries {
		if entry.Remote() == "slow" {
			if r.omitSlow.Load() {
				continue
			}
			if object, ok := entry.(fs.Object); ok {
				entry = &directoryLatencyObject{Object: object, remote: r}
			}
		}
		out = append(out, entry)
	}
	if r.omitSlow.Load() {
		r.omissionOnce.Do(func() { close(r.observedOmission) })
	}
	return out, nil
}

type directoryLatencyObject struct {
	fs.Object
	remote *directoryLatencyRemote
}

func (o *directoryLatencyObject) SetModTime(ctx context.Context, when time.Time) error {
	close(o.remote.setEntered)
	select {
	case <-o.remote.releaseSet:
	case <-ctx.Done():
		return ctx.Err()
	}
	return o.Object.SetModTime(ctx, when)
}

func TestCachedSiblingDoesNotWaitForDirectoryIO(t *testing.T) {
	for _, blocked := range []string{"listing", "absent-entry-metadata"} {
		t.Run(blocked, func(t *testing.T) {
			ctx := context.Background()
			r := fstest.NewRun(t)
			r.WriteObject(ctx, "slow", "slow-data", time.Unix(1, 0))
			r.WriteObject(ctx, "other", "other-data", time.Unix(1, 0))
			remote := &directoryLatencyRemote{Fs: r.Fremote, features: *r.Fremote.Features(),
				listEntered: make(chan struct{}), releaseList: make(chan struct{}),
				setEntered: make(chan struct{}), releaseSet: make(chan struct{}), observedOmission: make(chan struct{})}
			remote.features.ListP, remote.features.ListR = nil, nil
			opt := vfscommon.Opt
			opt.CacheMode = vfscommon.CacheModeFull
			opt.CachePollInterval, opt.PollInterval, opt.HandleCaching = 0, 0, 0
			v := New(ctx, remote, &opt)
			t.Cleanup(func() { cleanupVFS(t, v) })
			root, err := v.Root()
			require.NoError(t, err)
			slow, err := root.Stat("slow")
			require.NoError(t, err)
			other, err := root.Stat("other")
			require.NoError(t, err)
			var releaseOnce sync.Once
			release := func() { releaseOnce.Do(func() { close(remote.releaseList); close(remote.releaseSet) }) }
			defer release()
			metadataDone := make(chan error, 1)
			if blocked == "listing" {
				remote.blockList.Store(true)
			} else {
				go func() { metadataDone <- slow.SetModTime(time.Unix(100, 0)) }()
				select {
				case <-remote.setEntered:
				case <-time.After(5 * time.Second):
					t.Fatal("metadata gate not reached")
				}
				remote.omitSlow.Store(true)
			}
			refreshDone := make(chan error, 1)
			go func() { refreshDone <- root.readDir() }()
			entered := remote.listEntered
			if blocked != "listing" {
				entered = remote.observedOmission
			}
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("listing gate not reached")
			}
			if blocked != "listing" {
				require.Eventually(t, func() bool {
					buffer := make([]byte, 1<<20)
					n := runtime.Stack(buffer, true)
					for _, stack := range strings.Split(string(buffer[:n]), "\n\n") {
						if strings.Contains(stack, "(*Dir).setListingNode(") && strings.Contains(stack, "sync.") {
							return true
						}
					}
					return false
				}, time.Second, time.Millisecond)
			}
			lookup := make(chan error, 1)
			go func() {
				node, err := root.Stat("other")
				if err == nil && node != other {
					err = fs.ErrorObjectNotFound
				}
				lookup <- err
			}()
			ready := false
			select {
			case err := <-lookup:
				require.NoError(t, err)
				ready = true
			case <-time.After(time.Second):
			}
			release()
			require.NoError(t, <-refreshDone)
			if blocked != "listing" {
				require.NoError(t, <-metadataDone)
			}
			if !ready {
				require.NoError(t, <-lookup)
			}
			require.True(t, ready, "cached sibling lookup waited for unrelated directory I/O")
		})
	}
}

func directoryRefreshFixture(t *testing.T) (*VFS, *Dir, *directoryLatencyRemote) {
	t.Helper()
	ctx := context.Background()
	r := fstest.NewRun(t)
	r.WriteObject(ctx, "other", "original", time.Unix(1, 0))
	remote := &directoryLatencyRemote{Fs: r.Fremote, features: *r.Fremote.Features(),
		listEntered: make(chan struct{}), releaseList: make(chan struct{}), setEntered: make(chan struct{}), releaseSet: make(chan struct{}), observedOmission: make(chan struct{})}
	remote.features.ListP, remote.features.ListR = nil, nil
	opt := vfscommon.Opt
	opt.CacheMode = vfscommon.CacheModeFull
	opt.CachePollInterval, opt.PollInterval, opt.HandleCaching, opt.WriteBack = 0, 0, 0, 0
	view := New(ctx, remote, &opt)
	t.Cleanup(func() { cleanupVFS(t, view) })
	root, err := view.Root()
	require.NoError(t, err)
	_, err = root.Stat("other")
	require.NoError(t, err)
	return view, root, remote
}

func TestRefreshPreservesStrictFreshness(t *testing.T) {
	for _, kind := range []string{"invalidated-positive", "unknown-name", "strict-readdir"} {
		t.Run(kind, func(t *testing.T) {
			_, root, remote := directoryRefreshFixture(t)
			if kind == "invalidated-positive" {
				root.invalidateDir("")
			}
			remote.blockList.Store(true)
			var once sync.Once
			release := func() { once.Do(func() { close(remote.releaseList) }) }
			defer release()
			refreshed := make(chan error, 1)
			go func() { refreshed <- root.readDir() }()
			select {
			case <-remote.listEntered:
			case <-time.After(time.Second):
				t.Fatal("refresh not reached")
			}
			lookup := make(chan error, 1)
			go func() {
				var err error
				if kind == "strict-readdir" {
					_, err = root.ReadDirAll()
				} else {
					name := "other"
					if kind == "unknown-name" {
						name = "absent"
					}
					_, err = root.Stat(name)
				}
				lookup <- err
			}()
			early := false
			var lookupErr error
			select {
			case lookupErr = <-lookup:
				early = true
			case <-time.After(25 * time.Millisecond):
			}
			release()
			require.NoError(t, <-refreshed)
			if !early {
				lookupErr = <-lookup
			}
			if kind == "unknown-name" {
				require.ErrorIs(t, lookupErr, ENOENT)
			} else {
				require.NoError(t, lookupErr)
			}
			require.False(t, early, "strict caller used a superseded or incomplete observation")
		})
	}
}

func TestFailedRefreshDoesNotRenewSnapshot(t *testing.T) {
	_, root, remote := directoryRefreshFixture(t)
	root.mu.RLock()
	before := root.read
	root.mu.RUnlock()
	remote.failList.Store(true)
	require.ErrorContains(t, root.readDir(), "injected listing failure")
	root.mu.RLock()
	after := root.read
	root.mu.RUnlock()
	require.Equal(t, before, after)
	_, err := root.Stat("other")
	require.NoError(t, err, "still-valid snapshot remains usable")
	root.invalidateDir("")
	_, err = root.Stat("other")
	require.ErrorContains(t, err, "injected listing failure")
}

func TestCapturedListingCannotUndoLocalNamespaceCompletion(t *testing.T) {
	for _, operation := range []string{"create", "unlink"} {
		t.Run(operation, func(t *testing.T) {
			view, root, remote := directoryRefreshFixture(t)
			var file *File
			var err error
			if operation == "create" {
				file, err = root.Create("new", os.O_CREATE|os.O_RDWR)
			} else {
				var node Node
				node, err = root.Stat("other")
				file = node.(*File)
			}
			require.NoError(t, err)
			remote.blockList.Store(true)
			var once sync.Once
			release := func() { once.Do(func() { close(remote.releaseList) }) }
			defer release()
			refreshed := make(chan error, 1)
			go func() { refreshed <- root.readDir() }()
			select {
			case <-remote.listEntered:
			case <-time.After(time.Second):
				t.Fatal("captured listing not reached")
			}
			changed := make(chan error, 1)
			go func() {
				if operation == "unlink" {
					changed <- file.Remove()
					return
				}
				handle, e := file.Open(os.O_CREATE | os.O_RDWR)
				if e == nil {
					_, e = handle.Write([]byte("new bytes"))
					e = errors.Join(e, handle.Close())
				}
				changed <- e
			}()
			ready := false
			select {
			case err = <-changed:
				require.NoError(t, err)
				ready = true
			case <-time.After(time.Second):
			}
			release()
			require.NoError(t, <-refreshed)
			if !ready {
				require.NoError(t, <-changed)
			}
			require.True(t, ready, "local completion waited for old remote listing")
			if operation == "unlink" {
				_, err = root.Stat("other")
				require.ErrorIs(t, err, ENOENT)
			} else {
				data, e := view.ReadFile("new")
				require.NoError(t, e)
				require.Equal(t, "new bytes", string(data))
			}
		})
	}
}
