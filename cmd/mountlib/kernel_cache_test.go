// Exercise completion, retry and ownership boundaries of kernel cache changes.
package mountlib

import (
	"context"
	"errors"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dogbutcat/gclone/vfs"
	"github.com/dogbutcat/gclone/vfs/vfscache"
	"github.com/dogbutcat/gclone/vfs/vfscommon"
	"github.com/rclone/rclone/fs"
	"github.com/stretchr/testify/require"
)

func kernelCacheFixture(t *testing.T) (*KernelCache, *vfs.File, vfs.Handle, func() error) {
	t.Helper()
	remote, err := fs.NewFs(context.Background(), t.TempDir())
	require.NoError(t, err)
	opt := vfscommon.Opt
	opt.CacheMode = vfscommon.CacheModeFull
	opt.CachePollInterval, opt.HandleCaching, opt.WriteBack = 0, 0, 0
	view := vfs.New(context.Background(), remote, &opt)
	t.Cleanup(func() { view.WaitForWriters(time.Second); view.Shutdown(); require.NoError(t, view.CleanUp()) })
	initial, err := view.OpenFile("data", os.O_CREATE|os.O_RDWR, 0600)
	require.NoError(t, err)
	_, err = initial.Write([]byte("original"))
	require.NoError(t, err)
	require.NoError(t, initial.Close())
	file := initial.Node().(*vfs.File)
	control := NewKernelCache()
	reader, err := file.OpenForKernel(os.O_RDONLY, control.Owner())
	require.NoError(t, err)
	require.NoError(t, PrepareHandle(reader))
	t.Cleanup(func() {
		defer control.Close()
		if err := reader.Close(); !errors.Is(err, vfs.ECLOSED) {
			require.NoError(t, err)
		}
	})
	keep, err := control.Prepare(context.Background(), reader, func() error { return nil })
	require.NoError(t, err)
	require.True(t, keep)
	mutate := func() error {
		h, err := file.Open(os.O_RDWR)
		if err != nil {
			return err
		}
		_, err = h.WriteAt([]byte("changed!"), 0)
		return errors.Join(err, h.Close())
	}
	return control, file, reader, mutate
}

func TestKernelCacheNotificationCompletesBeforePublication(t *testing.T) {
	control, _, reader, mutate := kernelCacheFixture(t)
	entered, release, retried := make(chan struct{}), make(chan struct{}), make(chan struct{})
	want := errors.New("notification failed")
	var calls atomic.Int32
	control.SetNotifier(func() error {
		if calls.Add(1) == 1 {
			close(entered)
			<-release
			return want
		}
		if calls.Load() == 2 {
			close(retried)
		}
		return nil
	})
	write := make(chan error, 1)
	go func() { write <- mutate() }()
	<-entered
	second := make(chan error, 1)
	go func() { _, err := control.Prepare(context.Background(), reader, nil); second <- err }()
	select {
	case err := <-second:
		t.Fatalf("open acknowledged unfinished notification: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	require.ErrorIs(t, <-write, want)
	require.ErrorIs(t, <-second, want)
	select {
	case <-retried:
	case <-time.After(2 * time.Second):
		t.Fatal("failed notification did not retry without another OPEN")
	}
	require.Eventually(t, func() bool { return control.Wait(context.Background()) == nil }, time.Second, time.Millisecond)
}

func TestKernelCacheChangedDuringNotification(t *testing.T) {
	control, _, reader, mutate := kernelCacheFixture(t)
	entered, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	control.SetNotifier(func() error {
		if calls.Add(1) == 1 {
			close(entered)
			<-release
		}
		return nil
	})
	a, b := make(chan error, 1), make(chan error, 1)
	go func() { a <- mutate() }()
	<-entered
	first, _, err := reader.(*vfs.RWFileHandle).KernelCacheVersion()
	require.NoError(t, err)
	go func() { b <- mutate() }()
	require.Eventually(t, func() bool {
		now, _, err := reader.(*vfs.RWFileHandle).KernelCacheVersion()
		return err == nil && now != first
	}, time.Second, time.Millisecond)
	close(release)
	require.NoError(t, <-a)
	require.NoError(t, <-b)
	require.GreaterOrEqual(t, calls.Load(), int32(2))
}

func TestKernelCacheCancellationDoesNotAbandonExistingReaders(t *testing.T) {
	control, _, reader, mutate := kernelCacheFixture(t)
	entered, release := make(chan struct{}), make(chan struct{})
	control.SetNotifier(func() error { close(entered); <-release; return nil })
	write := make(chan error, 1)
	go func() { write <- mutate() }()
	<-entered
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := control.Prepare(ctx, reader, nil)
	require.ErrorIs(t, err, context.Canceled)
	close(release)
	require.NoError(t, <-write)
	got := make([]byte, 8)
	_, err = reader.ReadAt(got, 0)
	require.NoError(t, err)
	require.Equal(t, "changed!", string(got))
	require.NoError(t, control.Wait(context.Background()))
}

func TestKernelCacheOwnWritesKeepView(t *testing.T) {
	control, file, reader, _ := kernelCacheFixture(t)
	var notifications atomic.Int32
	control.SetNotifier(func() error { notifications.Add(1); return nil })
	writer, err := file.OpenForKernel(os.O_RDWR, control.Owner())
	require.NoError(t, err)
	require.NoError(t, PrepareHandle(writer))
	_, err = writer.WriteAt([]byte("written!"), 0)
	require.NoError(t, err)
	require.NoError(t, writer.Truncate(4))
	_, err = control.Prepare(context.Background(), reader, nil)
	require.NoError(t, err)
	require.NoError(t, writer.Close())
	require.Zero(t, notifications.Load())
}

func TestKernelCacheForeignWriteNotifiesWithoutReopen(t *testing.T) {
	for _, closed := range []bool{false, true} {
		t.Run(map[bool]string{false: "held-reader", true: "closed-reader-pages"}[closed], func(t *testing.T) {
			control, _, reader, mutate := kernelCacheFixture(t)
			var notifications atomic.Int32
			control.SetNotifier(func() error { notifications.Add(1); return nil })
			if closed {
				require.NoError(t, reader.Close())
			}
			require.NoError(t, mutate())
			require.Greater(t, notifications.Load(), int32(0))
		})
	}
}

func TestRejectedKernelTruncatePreservesVisibleSize(t *testing.T) {
	for _, ordinary := range []bool{false, true} {
		t.Run(map[bool]string{false: "other-owner", true: "ordinary-vfs"}[ordinary], func(t *testing.T) {
			control, file, _, _ := kernelCacheFixture(t)
			writer, err := file.OpenForKernel(os.O_RDWR, control.Owner())
			require.NoError(t, err)
			require.NoError(t, PrepareHandle(writer))
			defer writer.Close()
			owner := vfscache.NewKernelCacheOwner()
			if ordinary {
				owner = nil
			}
			require.Error(t, file.TruncateForKernel(4, owner))
			require.Equal(t, int64(8), file.Size())
			require.Equal(t, int64(8), writer.(*vfs.RWFileHandle).Size())
		})
	}
}

func TestUnmountedKernelViewDoesNotBlockOtherWriters(t *testing.T) {
	control, _, _, mutate := kernelCacheFixture(t)
	control.Close()
	require.NoError(t, mutate())
}
