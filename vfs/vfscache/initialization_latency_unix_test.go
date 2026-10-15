// Verify cache initialization isolation with a genuinely blocked metadata read.
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

	"github.com/stretchr/testify/require"
)

func TestSlowMetadataInitializationDoesNotBlockOtherItems(t *testing.T) {
	_, c := newTestCache(t)
	other := c.Item("other")
	data, meta := c.toOSPath("slow"), c.toOSPathMeta("slow")
	require.NoError(t, os.MkdirAll(filepath.Dir(data), 0700))
	require.NoError(t, os.MkdirAll(filepath.Dir(meta), 0700))
	require.NoError(t, os.WriteFile(data, []byte("old"), 0600))
	require.NoError(t, syscall.Mkfifo(meta, 0600))
	loading := make(chan *Item, 1)
	go func() { loading <- c.Item("slow") }()
	var writer *os.File
	require.Eventually(t, func() bool {
		var err error
		writer, err = os.OpenFile(meta, os.O_WRONLY|syscall.O_NONBLOCK, 0600)
		if errors.Is(err, syscall.ENXIO) {
			return false
		}
		require.NoError(t, err)
		return err == nil
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
	lookup := make(chan *Item, 1)
	go func() { lookup <- c.Item("other") }()
	ready := false
	select {
	case item := <-lookup:
		require.Same(t, other, item)
		ready = true
	case <-time.After(time.Second):
	}
	release()
	require.NotNil(t, <-loading)
	if !ready {
		require.Same(t, other, <-lookup)
	}
	require.True(t, ready, "unrelated item waited for metadata file I/O")
}
