// Regression tests for observation ownership, maintenance, and failed creation.
package vfscache

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/rclone/rclone/fs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Keep cancellation pending to expose the real writeback-drain unlock window.
func blockObservationDrain(t *testing.T, item *Item) (<-chan struct{}, func()) {
	t.Helper()
	started, canceled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var once sync.Once
	finish := func() { once.Do(func() { close(release) }) }
	t.Cleanup(finish)
	item.c.writeback.SetID(&item.writeBackID)
	item.c.writeback.Add(item.writeBackID, item.name, 0, false, func(ctx context.Context) error {
		close(started)
		<-ctx.Done()
		close(canceled)
		<-release
		// Model a backend that committed despite cancellation. A successful
		// result avoids rescheduling a synthetic zero-delay upload in this test.
		return nil
	})
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("writeback did not start")
	}
	return canceled, finish
}

func TestObservationCannotInvalidateAfterNewerAbsence(t *testing.T) {
	_, c, item, _ := uploadedCacheFixture(t)
	data, err := os.ReadFile(c.toOSPath("wal"))
	require.NoError(t, err)
	meta, err := os.ReadFile(c.toOSPathMeta("wal"))
	require.NoError(t, err)
	require.NoError(t, item.o.Remove(context.Background()))
	item.mu.Lock()
	item._observeAbsence()
	obs := item._objectObservation()
	item.mu.Unlock()
	canceled, finish := blockObservationDrain(t, item)
	done := make(chan error, 1)
	go func() {
		item.mu.Lock()
		err := item._invalidateObject("review regression", obs, 0)
		item.mu.Unlock()
		done <- err
	}()
	select {
	case <-canceled:
	case <-time.After(5 * time.Second):
		t.Fatal("invalidation did not release its lock for cancellation")
	}
	_, err = item.OpenWithObject(item.o, false)
	require.ErrorIs(t, err, os.ErrNotExist, "a second true->true absence must invalidate the old token")
	finish()
	require.ErrorIs(t, <-done, errObjectViewChanged)
	after, err := os.ReadFile(c.toOSPath("wal"))
	require.NoError(t, err)
	afterMeta, err := os.ReadFile(c.toOSPathMeta("wal"))
	require.NoError(t, err)
	assert.Equal(t, data, after)
	assert.Equal(t, meta, afterMeta)
	assert.True(t, item.mustRevalidate)
}

func TestCreationCannotClearNewerAbsenceDuringDrain(t *testing.T) {
	_, c := newItemTestCache(t)
	item := c.Item("wal")
	require.NoError(t, item.Open(nil))
	item.mu.Lock()
	item._observeAbsence()
	obs := item._objectObservation()
	item.mu.Unlock()
	canceled, finish := blockObservationDrain(t, item)
	done := make(chan error, 1)
	go func() {
		item.mu.Lock()
		err := item._commitCreate(obs)
		item.mu.Unlock()
		done <- err
	}()
	select {
	case <-canceled:
	case <-time.After(5 * time.Second):
		t.Fatal("creation did not release its lock for cancellation")
	}
	_, err := item.OpenWithObject(nil, false)
	require.ErrorIs(t, err, os.ErrNotExist)
	finish()
	require.ErrorIs(t, <-done, errObjectViewChanged)
	assert.True(t, item.mustRevalidate)
	assert.False(t, item.IsDirty())
	require.NoError(t, item.Close(nil))
}

func TestFailedCreationKeepsAbsenceGuard(t *testing.T) {
	r, c := newItemTestCache(t)
	item := c.Item("wal")
	require.NoError(t, item.Open(nil))
	meta := c.toOSPathMeta("wal")
	require.NoError(t, os.Remove(meta))
	require.NoError(t, os.Mkdir(meta, 0700)) // make the metadata save fail
	item.mu.Lock()
	item._observeAbsence()
	err := item._commitCreate(item._objectObservation())
	guard, dirty := item.mustRevalidate, item.info.Dirty
	item._abortObjectOpen()
	opens, fd := item.opens, item.fd
	item.mu.Unlock()
	require.Error(t, err)
	assert.True(t, guard)
	assert.False(t, dirty)
	assert.Zero(t, opens)
	assert.Nil(t, fd)
	require.NoError(t, os.Remove(meta))
	_, err = item.OpenWithObject(nil, false)
	require.ErrorIs(t, err, os.ErrNotExist)
	r.WriteObject(context.Background(), "wal", "fresh", time.Unix(20, 0))
	_, err = item.OpenWithObject(nil, false)
	require.NoError(t, err)
	buf := make([]byte, 5)
	_, err = item.ReadAt(buf, 0)
	require.NoError(t, err)
	require.Equal(t, "fresh", string(buf), "failed creation's empty cache must not mask a fresh remote")
	require.NoError(t, item.Close(nil))
}

func TestResetCannotClearConfirmedAbsence(t *testing.T) {
	_, c, item, _ := uploadedCacheFixture(t)
	require.NoError(t, item.Open(item.o))
	require.NoError(t, item.o.Remove(context.Background()))
	item.mu.Lock()
	item._observeAbsence()
	item.mu.Unlock()
	fingerprint := item.info.Fingerprint
	lookups := c.fremote.(*cacheLookupProbe).calls
	result, freed, err := item.Reset()
	require.NoError(t, err)
	require.Equal(t, SkippedRevalidation, result)
	require.Zero(t, freed)
	require.Equal(t, lookups, c.fremote.(*cacheLookupProbe).calls, "cleaner must not query the remote under Cache.mu")
	assert.True(t, item.mustRevalidate)
	assert.Equal(t, fingerprint, item.info.Fingerprint, "maintenance retains the guarded cache unchanged")
	_, err = item.OpenWithObject(item.o, false)
	require.ErrorIs(t, err, os.ErrNotExist)
	require.NoError(t, item.Close(nil))
}

func TestGuardedOpenPreservesAbsenceAcrossLookupFailures(t *testing.T) {
	r, c, item, old := uploadedCacheFixture(t)
	probe := c.fremote.(*cacheLookupProbe)
	probe.override, probe.err = true, fs.ErrorObjectNotFound
	_, err := item.OpenWithObject(old, false)
	require.ErrorIs(t, err, os.ErrNotExist)
	for _, lookupErr := range []error{os.ErrPermission, context.DeadlineExceeded, nil, fs.ErrorObjectNotFound} {
		probe.err = lookupErr
		calls := probe.calls
		_, err = item.OpenWithObject(item.o, false) // matching fingerprint must still query
		require.Error(t, err)
		assert.Equal(t, calls+1, probe.calls)
		assert.True(t, item.mustRevalidate)
		assert.Zero(t, item.opens)
		data, readErr := os.ReadFile(c.toOSPath("wal"))
		require.NoError(t, readErr)
		assert.Equal(t, "uploaded bytes", string(data))
	}
	r.WriteObject(context.Background(), "wal", "new valid bytes", time.Unix(300, 0))
	probe.override = false
	_, err = item.OpenWithObject(item.o, false)
	require.NoError(t, err)
	assert.False(t, item.mustRevalidate)
	buf := make([]byte, len("new valid bytes"))
	_, err = item.ReadAt(buf, 0)
	require.NoError(t, err)
	assert.Equal(t, "new valid bytes", string(buf))
	require.NoError(t, item.Close(nil))
}

func TestGuardedDirtyDataRecreatesItsWritebackTarget(t *testing.T) {
	r, _, item, _ := uploadedCacheFixture(t)
	require.NoError(t, item.Open(item.o))
	_, err := item.WriteAt([]byte("pending bytes!"), 0)
	require.NoError(t, err)
	require.NoError(t, item.o.Remove(context.Background()))
	item.mu.Lock()
	item._observeAbsence()
	item.mu.Unlock()
	_, err = item.OpenWithObject(item.o, false)
	require.NoError(t, err, "a real dirty local owner keeps its data")
	assert.Nil(t, item.o)
	assert.True(t, item.mustRevalidate)
	require.NoError(t, item.Close(nil))
	require.NoError(t, item.Close(nil))
	checkObject(t, r, "wal", "pending bytes!")
	_, err = item.OpenWithObject(item.o, false)
	require.NoError(t, err)
	assert.False(t, item.mustRevalidate)
	require.NoError(t, item.Close(nil))
}
