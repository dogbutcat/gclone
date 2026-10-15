// A slow SA credential read must not stop requests using a committed service.
//go:build linux || darwin

package drive

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	drive "google.golang.org/api/drive/v3"
)

func TestCommittedServiceSurvivesBlockedCredentialRead(t *testing.T) {
	ctx, f, _ := testSAListFixture(t, func(saListRequest) (int, any) { return 200, &drive.FileList{} })
	_, err := f.list(ctx, []string{"root"}, "", false, false, false, false, func(*drive.File) bool { return false })
	require.NoError(t, err)
	original := f.currentService()
	credentials := f.opt.ServiceAccountCredentials
	name := filepath.Join(t.TempDir(), "blocked.json")
	require.NoError(t, syscall.Mkfifo(name, 0600))
	switched := make(chan error, 1)
	go func() { switched <- f.changeServiceAccountFile(ctx, name) }()
	var writer *os.File
	require.Eventually(t, func() bool {
		var openErr error
		writer, openErr = os.OpenFile(name, os.O_WRONLY|syscall.O_NONBLOCK, 0600)
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
			_, _ = writer.Write([]byte("invalid credentials"))
			_ = writer.Close()
		}
	}
	defer release()
	snapshot := make(chan saService, 1)
	listed := make(chan error, 1)
	go func() { snapshot <- f.currentService() }()
	go func() {
		_, e := f.list(ctx, []string{"root"}, "", false, false, false, false, func(*drive.File) bool { return false })
		listed <- e
	}()
	snapshotReady, listReady := false, false
	timer := time.NewTimer(time.Second)
wait:
	for !snapshotReady || !listReady {
		select {
		case got := <-snapshot:
			require.Equal(t, original, got)
			snapshotReady = true
		case e := <-listed:
			require.NoError(t, e)
			listReady = true
		case <-timer.C:
			break wait
		}
	}
	timer.Stop()
	release()
	require.Error(t, <-switched)
	if !snapshotReady {
		require.Equal(t, original, <-snapshot)
	}
	if !listReady {
		require.NoError(t, <-listed)
	}
	require.Equal(t, original, f.currentService())
	require.Equal(t, credentials, f.opt.ServiceAccountCredentials)
	require.True(t, snapshotReady, "service reader waited for credential I/O")
	require.True(t, listReady, "ordinary listing waited for credential I/O")
}

func TestRootCopyUsesSharedCommittedService(t *testing.T) {
	ctx, f, _ := testSAListFixture(t, func(saListRequest) (int, any) { return 200, &drive.FileList{} })
	old := f.currentService()
	copy := *f
	next := f.ServiceAccountFiles.rollup()
	require.NotEmpty(t, next)
	require.NoError(t, copy.changeServiceAccountFile(ctx, next))
	require.Equal(t, next, f.currentService().file)
	f.waitChangeSvc.Lock()
	f.changeSvcForSA(context.Background(), old.file)
	f.waitChangeSvc.Unlock()
	require.Equal(t, next, f.currentService().file, "late old-account failure changed the published replacement")
}
