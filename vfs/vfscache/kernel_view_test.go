// Verify data generations and descriptor reuse for kernel-backed cache views.
package vfscache

import (
	"bytes"
	"context"
	"io"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestKernelViewReplacementSurvivesCloseAndReopen(t *testing.T) {
	for _, body := range []string{"after-1111", "replacement with more bytes"} {
		t.Run(body, func(t *testing.T) {
			r, c := newItemTestCache(t)
			ctx, when := context.Background(), time.Unix(1700000000, 0)
			r.WriteObject(ctx, "data", "before0000", when)
			item, owner := c.Item("data"), NewKernelCacheOwner()
			var leases []*KernelCacheLease
			closeOne := func() error {
				lease := leases[len(leases)-1]
				leases = leases[:len(leases)-1]
				err := item.Close(nil)
				lease.Close()
				return err
			}
			t.Cleanup(func() {
				for len(leases) > 0 {
					_ = closeOne()
				}
			})
			open := func() {
				t.Helper()
				o, err := r.Fremote.NewObject(ctx, "data")
				require.NoError(t, err)
				_, lease, err := item.OpenForKernel(o, false, owner, false)
				require.NoError(t, err)
				leases = append(leases, lease)
			}
			read := func(want string) {
				t.Helper()
				got := make([]byte, len(want))
				n, err := item.ReadAt(got, 0)
				if err != nil && err != io.EOF {
					require.NoError(t, err)
				}
				require.Equal(t, len(want), n)
				require.Equal(t, want, string(got))
			}
			open()
			read("before0000")
			before, err := leases[0].Version()
			require.NoError(t, err)
			r.WriteObject(ctx, "data", body, when)
			open()
			read(body)
			after, err := leases[1].Version()
			require.NoError(t, err)
			require.NotEqual(t, before, after, "external bytes must retire the kernel view")
			require.NoError(t, closeOne())
			require.NoError(t, closeOne())
			open()
			read(body)
			require.NoError(t, closeOne())
		})
	}
}

func TestKernelReaderReclaimsCleanRangesWithoutChangingSource(t *testing.T) {
	r, c := newItemTestCache(t)
	ctx := context.Background()
	data := bytes.Repeat([]byte("01234567"), 128*1024)
	r.WriteObject(ctx, "large", string(data), time.Unix(1700000000, 0))
	item, owner := c.Item("large"), NewKernelCacheOwner()
	o, err := r.Fremote.NewObject(ctx, "large")
	require.NoError(t, err)
	_, lease, err := item.OpenForKernel(o, false, owner, false)
	require.NoError(t, err)
	defer func() { require.NoError(t, item.Close(nil)); lease.Close() }()
	version, err := lease.Version()
	require.NoError(t, err)
	for offset := 0; offset < len(data); offset += 64 * 1024 {
		got := make([]byte, 64*1024)
		n, err := item.ReadAt(got, int64(offset))
		require.NoError(t, err)
		require.Equal(t, len(got), n)
		require.Equal(t, data[offset:offset+len(got)], got)
		reset, freed, err := item.Reset()
		require.NoError(t, err)
		require.Equal(t, ResetComplete, reset)
		require.Greater(t, freed, int64(0))
		after, err := lease.Version()
		require.NoError(t, err)
		require.Equal(t, version, after)
	}
}

func TestKernelViewDefersExternalBytesUntilWriterCloses(t *testing.T) {
	r, c := newItemTestCache(t)
	ctx, when := context.Background(), time.Unix(1700000000, 0)
	r.WriteObject(ctx, "data", "before0000", when)
	item, owner := c.Item("data"), NewKernelCacheOwner()
	o, err := r.Fremote.NewObject(ctx, "data")
	require.NoError(t, err)
	_, writer, err := item.OpenForKernel(o, false, owner, true)
	require.NoError(t, err)
	_, err = item.ReadAt(make([]byte, 10), 0)
	require.NoError(t, err)
	before, err := writer.Version()
	require.NoError(t, err)
	r.WriteObject(ctx, "data", "after-1111", when)
	o, err = r.Fremote.NewObject(ctx, "data")
	require.NoError(t, err)
	_, reader, err := item.OpenForKernel(o, false, owner, false)
	require.NoError(t, err)
	got := make([]byte, 10)
	_, err = item.ReadAt(got, 0)
	require.NoError(t, err)
	require.Equal(t, "before0000", string(got))
	after, err := reader.Version()
	require.NoError(t, err)
	require.Equal(t, before, after)
	reset, freed, err := item.Reset()
	require.NoError(t, err)
	require.Equal(t, int64(0), freed)
	require.Equal(t, SkippedPendingAccess, reset)
	// No write has reached VFS yet, but the accepted writer may have dirty mmap
	// pages. Close is the boundary after which a clean remote view can replace it.
	require.NoError(t, item.Close(nil))
	writer.Close()
	_, latest, err := item.OpenForKernel(o, false, owner, false)
	require.NoError(t, err)
	_, err = item.ReadAt(got, 0)
	require.NoError(t, err)
	require.Equal(t, "after-1111", string(got))
	after, err = latest.Version()
	require.NoError(t, err)
	require.NotEqual(t, before, after)
	require.NoError(t, item.Close(nil))
	latest.Close()
	require.NoError(t, item.Close(nil))
	reader.Close()
	_, err = writer.Version()
	require.ErrorIs(t, err, os.ErrClosed)
}

func TestKernelViewForeignWriterCannotDiscardPendingPages(t *testing.T) {
	r, c := newItemTestCache(t)
	ctx, when := context.Background(), time.Unix(1700000000, 0)
	r.WriteObject(ctx, "data", "before0000", when)
	item, owner := c.Item("data"), NewKernelCacheOwner()
	o, err := r.Fremote.NewObject(ctx, "data")
	require.NoError(t, err)
	_, writer, err := item.OpenForKernel(o, false, owner, true)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, item.Close(nil)); writer.Close() })
	_, err = item.WriteAt([]byte("unsafe"), 0)
	require.ErrorIs(t, err, errObjectViewChanged)
	require.ErrorIs(t, item.Truncate(1), errObjectViewChanged)
	_, other, err := item.OpenForKernel(o, false, NewKernelCacheOwner(), true)
	require.ErrorIs(t, err, errObjectViewChanged)
	require.Nil(t, other)
	require.Equal(t, 1, item.opens, "rejected opens must not leak a descriptor pin")
	_, err = item.WriteAtForKernel([]byte("safe"), 0, owner)
	require.NoError(t, err)
}

func TestKernelViewReplacementRetainsWritebackPath(t *testing.T) {
	r, c := newItemTestCache(t)
	ctx, when := context.Background(), time.Unix(1700000000, 0)
	r.WriteObject(ctx, "data", "before0000", when)
	item, owner := c.Item("data"), NewKernelCacheOwner()
	var leases []*KernelCacheLease
	closeOne := func() error {
		lease := leases[len(leases)-1]
		leases = leases[:len(leases)-1]
		err := item.Close(nil)
		lease.Close()
		return err
	}
	t.Cleanup(func() {
		for len(leases) > 0 {
			_ = closeOne()
		}
	})
	open := func(writable bool) {
		t.Helper()
		o, err := r.Fremote.NewObject(ctx, "data")
		require.NoError(t, err)
		_, lease, err := item.OpenForKernel(o, false, owner, writable)
		require.NoError(t, err)
		leases = append(leases, lease)
	}
	open(false)
	_, err := item.ReadAt(make([]byte, 10), 0)
	require.NoError(t, err)
	r.WriteObject(ctx, "data", "after-1111", when)
	open(false)
	current := make([]byte, 10)
	_, err = item.ReadAt(current, 0)
	require.NoError(t, err)
	require.Equal(t, "after-1111", string(current))
	open(true)
	_, err = item.WriteAtForKernel([]byte("EDIT"), 0, owner)
	require.NoError(t, err)
	require.NoError(t, closeOne())
	require.NoError(t, closeOne())
	require.NoError(t, closeOne())
	checkObject(t, r, "data", "EDITr-1111")
}
