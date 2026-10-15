// Regression coverage for a newer remote object observed during upload publication.
package vfs

import (
	"context"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dogbutcat/gclone/vfs/vfscommon"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/rc"
	"github.com/rclone/rclone/fstest"
	"github.com/stretchr/testify/require"
)

type publicationFS struct {
	fs.Fs
	id       atomic.Int64
	features fs.Features
	armed    atomic.Bool
	entered  chan struct{}
	release  chan struct{}
	once     sync.Once
}
type publicationObject struct {
	fs.Object
	owner *publicationFS
	id    int64
}

func (f *publicationFS) Features() *fs.Features { return &f.features }
func (f *publicationFS) NewObject(ctx context.Context, name string) (fs.Object, error) {
	o, e := f.Fs.NewObject(ctx, name)
	if e != nil {
		return nil, e
	}
	return &publicationObject{Object: o, owner: f, id: f.id.Load()}, nil
}
func (f *publicationFS) List(ctx context.Context, dir string) (fs.DirEntries, error) {
	es, e := f.Fs.List(ctx, dir)
	for i, x := range es {
		if o, ok := x.(fs.Object); ok {
			es[i] = &publicationObject{Object: o, owner: f, id: f.id.Load()}
		}
	}
	return es, e
}
func (o *publicationObject) Fs() fs.Info { return o.owner }
func (o *publicationObject) Remote() string {
	if o.owner.armed.Load() {
		var pcs [20]uintptr
		frames := runtime.CallersFrames(pcs[:runtime.Callers(1, pcs[:])])
		for {
			frame, more := frames.Next()
			if strings.HasSuffix(frame.Function, ".(*File).setObject") && o.owner.armed.CompareAndSwap(true, false) {
				close(o.owner.entered)
				<-o.owner.release
				break
			}
			if !more {
				break
			}
		}
	}
	return o.Object.Remote()
}
func (o *publicationObject) Update(ctx context.Context, in io.Reader, src fs.ObjectInfo, options ...fs.OpenOption) error {
	if id := o.owner.id.Load(); id != o.id {
		return fmt.Errorf("deleted ID %d, current ID %d: %w", o.id, id, fs.ErrorObjectNotFound)
	}
	return o.Object.Update(ctx, in, src, options...)
}

func TestPublicationReplacementDuringDeferredOpen(t *testing.T) {
	ctx := context.Background()
	r := fstest.NewRun(t)
	r.WriteObject(ctx, "wal", "old", time.Unix(1, 0))
	remote := &publicationFS{Fs: r.Fremote, features: *r.Fremote.Features(), entered: make(chan struct{}), release: make(chan struct{})}
	remote.features.Copy = nil
	remote.features.Move = nil
	remote.features.OpenWriterAt = nil
	remote.features.OpenChunkWriter = nil
	remote.id.Store(1)
	opt := vfscommon.Opt
	opt.CacheMode = vfscommon.CacheModeFull
	opt.WriteBack = fs.Duration(time.Millisecond)
	opt.HandleCaching = 0
	opt.CachePollInterval = 0
	opt.Links = true
	v := New(ctx, remote, &opt)
	t.Cleanup(func() { remote.once.Do(func() { close(remote.release) }); v.Shutdown(); _ = v.CleanUp() })
	w, err := v.OpenFile("wal", os.O_RDWR, 0600)
	require.NoError(t, err)
	_, err = w.Write([]byte("uploaded bytes"))
	require.NoError(t, err)
	reader, err := v.OpenFile("wal", os.O_RDWR, 0600)
	require.NoError(t, err)
	f := reader.Node().(*File)
	root, err := v.Root()
	require.NoError(t, err)
	remote.armed.Store(true)
	require.NoError(t, w.Close())
	select {
	case <-remote.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("callback did not reach publication hook")
	}
	// The hook is only a scheduling stop between publication timestamp and
	// File.mu unlock. The actual uploader and File.setObject remain in use.
	r.WriteObject(ctx, "wal", "external bytes", time.Unix(2, 0))
	remote.id.Store(2)
	fresh, err := remote.NewObject(ctx, "wal")
	require.NoError(t, err)
	// Hold the publication boundary explicitly. Listing preparation and lock
	// waits now release Dir.mu, so a marker.Remote callback would pause before
	// the observation is committed rather than in this publication window.
	root.mu.Lock()
	locked := true
	defer func() {
		if locked {
			root.mu.Unlock()
		}
	}()
	remote.once.Do(func() { close(remote.release) })
	f.setObjectNoUpdate(fresh, time.Now())
	require.Equal(t, int64(2), f.getObject().(*publicationObject).id)
	readDone := make(chan error, 1)
	go func() { _, e := reader.ReadAt(make([]byte, 1), 0); readDone <- e }()
	// Wait until openPending has opened the cache and is blocked behind the
	// same directory lock. This proves reconciliation happened in the window.
	require.Eventually(t, func() bool {
		buf := make([]byte, 1<<20)
		n := runtime.Stack(buf, true)
		for _, stack := range strings.Split(string(buf[:n]), "\n\n") {
			if strings.Contains(stack, "(*RWFileHandle).openPending") && strings.Contains(stack, "(*Dir).addObject") {
				return true
			}
		}
		return false
	}, 5*time.Second, time.Millisecond)
	accepted := f.getObject().(*publicationObject).id
	t.Logf("File ID after deferred cache open = %d", accepted)
	root.mu.Unlock()
	locked = false
	require.NoError(t, <-readDone)
	_, err = reader.WriteAt([]byte("pending bytes!"), 0)
	require.NoError(t, err)
	require.NoError(t, reader.Close())
	// Check both the File view and the actual subsequent asynchronous writeback.
	require.Equal(t, int64(2), accepted, "opening the observed replacement must not restore deleted ID 1")
	require.Eventually(t, func() bool {
		stats := v.Stats()["diskCache"].(rc.Params)
		return stats["uploadsQueued"] == 0 && stats["uploadsInProgress"] == 0
	}, 5*time.Second, time.Millisecond)
	current, err := remote.NewObject(ctx, "wal")
	require.NoError(t, err)
	stream, err := current.Open(ctx)
	require.NoError(t, err)
	content, err := io.ReadAll(stream)
	require.NoError(t, err)
	require.NoError(t, stream.Close())
	require.Equal(t, "pending bytes!", string(content))
}

func TestPublicationDeletionRespectsCreationIntent(t *testing.T) {
	for _, create := range []bool{false, true} {
		for _, fast := range []bool{false, true} {
			t.Run(fmt.Sprintf("create=%v/fast=%v", create, fast), func(t *testing.T) {
				ctx := context.Background()
				r := fstest.NewRun(t)
				r.WriteObject(ctx, "wal", "old", time.Unix(1, 0))
				remote := &publicationFS{Fs: r.Fremote, features: *r.Fremote.Features(), entered: make(chan struct{}), release: make(chan struct{})}
				remote.features.Copy, remote.features.Move = nil, nil
				remote.features.OpenWriterAt, remote.features.OpenChunkWriter = nil, nil
				remote.id.Store(1)
				opt := vfscommon.Opt
				opt.FastFingerprint = fast
				opt.CacheMode, opt.WriteBack = vfscommon.CacheModeFull, fs.Duration(time.Millisecond)
				opt.HandleCaching, opt.CachePollInterval, opt.Links = 0, 0, true
				v := New(ctx, remote, &opt)
				t.Cleanup(func() { remote.once.Do(func() { close(remote.release) }); v.Shutdown(); _ = v.CleanUp() })
				w, err := v.OpenFile("wal", os.O_RDWR, 0600)
				require.NoError(t, err)
				_, err = w.Write([]byte("uploaded bytes"))
				require.NoError(t, err)
				flags := os.O_RDONLY
				if create {
					flags |= os.O_CREATE
				}
				reader, err := v.OpenFile("wal", flags, 0600)
				require.NoError(t, err)
				var secondReader Handle
				if !create {
					secondReader, err = v.OpenFile("wal", os.O_RDONLY, 0600)
					require.NoError(t, err)
					require.Same(t, reader.(*RWFileHandle).item, secondReader.(*RWFileHandle).item)
					t.Cleanup(func() { _ = secondReader.Close() })
				}
				root, err := v.Root()
				require.NoError(t, err)
				remote.armed.Store(true)
				require.NoError(t, w.Close())
				select {
				case <-remote.entered:
				case <-time.After(5 * time.Second):
					t.Fatal("upload callback did not reach publication barrier")
				}
				obj, err := r.Fremote.NewObject(ctx, "wal")
				require.NoError(t, err)
				require.NoError(t, obj.Remove(ctx))
				root.mu.Lock()
				var unlock sync.Once
				t.Cleanup(func() { unlock.Do(root.mu.Unlock) })
				remote.once.Do(func() { close(remote.release) })
				readDone := make(chan error, 1)
				go func() { _, err := reader.ReadAt(make([]byte, 1), 0); readDone <- err }()
				if create {
					// Recreating a deleted target must drain the old callback before
					// dirtying the new file, without losing the dirty bit at _store's end.
					require.Eventually(t, func() bool {
						buf := make([]byte, 1<<20)
						n := runtime.Stack(buf, true)
						for _, stack := range strings.Split(string(buf[:n]), "\n\n") {
							if strings.Contains(stack, "(*RWFileHandle).openPending") && strings.Contains(stack, "(*WriteBack).Remove") {
								return true
							}
						}
						return false
					}, 5*time.Second, time.Millisecond)
				} else {
					select {
					case err = <-readDone:
						require.ErrorIs(t, err, ENOENT)
					case <-time.After(5 * time.Second):
						t.Fatal("existing-file read failed to report deletion")
					}
				}
				unlock.Do(root.mu.Unlock)
				if create {
					select {
					case err = <-readDone:
						require.NoError(t, err)
					case <-time.After(5 * time.Second):
						t.Fatal("creation did not finish after publication drained")
					}
				}
				if !create {
					require.Eventually(t, func() bool {
						stats := v.Stats()["diskCache"].(rc.Params)
						return stats["uploadsQueued"] == 0 && stats["uploadsInProgress"] == 0
					}, 5*time.Second, time.Millisecond)
					for _, handle := range []Handle{reader, reader, secondReader} {
						n, err := handle.ReadAt(make([]byte, 14), 0)
						require.Zero(t, n)
						require.ErrorIs(t, err, ENOENT, "confirmed absence survives publication and is shared by all Item handles")
					}
					require.NoError(t, secondReader.Close())
				}
				require.NoError(t, reader.Close())
				require.Eventually(t, func() bool {
					stats := v.Stats()["diskCache"].(rc.Params)
					return stats["uploadsQueued"] == 0 && stats["uploadsInProgress"] == 0
				}, 5*time.Second, time.Millisecond)
				obj, err = r.Fremote.NewObject(ctx, "wal")
				if create {
					require.NoError(t, err)
					// O_CREATE does not truncate dirty cached bytes. A remote
					// deletion during publication must not discard local content.
					stream, openErr := obj.Open(ctx)
					require.NoError(t, openErr)
					data, readErr := io.ReadAll(stream)
					require.NoError(t, readErr)
					require.NoError(t, stream.Close())
					require.Equal(t, "uploaded bytes", string(data))
				} else {
					require.ErrorIs(t, err, fs.ErrorObjectNotFound)
					require.NoError(t, root.readDir())
					_, err = v.Stat("wal")
					require.ErrorIs(t, err, ENOENT)
				}
			})
		}
	}
}
