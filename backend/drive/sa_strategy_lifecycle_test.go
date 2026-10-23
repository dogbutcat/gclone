// Probe rotation cancellation and committed identity at lifecycle boundaries.
package drive

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/dogbutcat/gclone/internal/dircache"
	"github.com/rclone/rclone/fs/config/configmap"
	"github.com/stretchr/testify/require"
	"google.golang.org/api/drive/v3"
)

// Pause construction at its first context-value lookup, after candidate selection.
type saConstructionContext struct {
	context.Context
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (c *saConstructionContext) Value(key any) any {
	c.once.Do(func() {
		close(c.entered)
		<-c.release
	})
	return c.Context.Value(key)
}

func TestSAStrategyCancelDuringConstruction(t *testing.T) {
	for _, tc := range []struct {
		mode  string
		quota bool
	}{
		{"default", false}, {"default", true}, {"cross-folder", false}, {"cross-folder", true},
	} {
		quota := tc.quota
		name := tc.mode + "/rolling"
		if quota {
			name = tc.mode + "/quota"
		}
		t.Run(name, func(t *testing.T) {
			ctx, f, _, _ := crossFolderFixture(t, func(saListRequest) (int, any) {
				return 200, &drive.FileList{}
			}, configmap.Simple{"service_account_file_strategy": tc.mode})
			before := f.currentService()
			activeBefore := f.ServiceAccountFiles.activeIdx
			pendingBefore := f.ServiceAccountFiles.rollup()
			inner, cancel := context.WithCancel(ctx)
			defer cancel()
			paused := &saConstructionContext{
				Context: inner, entered: make(chan struct{}), release: make(chan struct{}),
			}
			var releaseOnce sync.Once
			release := func() { releaseOnce.Do(func() { close(paused.release) }) }
			defer release()
			done := make(chan struct{})
			go func() {
				f.waitChangeSvc.Lock()
				if quota {
					f.changeSvcForSA(paused, before.file)
				} else {
					f.rollingSvc(paused)
				}
				f.waitChangeSvc.Unlock()
				close(done)
			}()
			select {
			case <-paused.entered:
			case <-time.After(3 * time.Second):
				t.Fatal("constructor did not reach context lookup")
			}
			cancel()
			require.ErrorIs(t, inner.Err(), context.Canceled)
			release()
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				t.Fatal("rotation did not settle after release")
			}
			after := f.currentService()
			pendingAfter := f.ServiceAccountFiles.rollup()
			_, requestErr := f.listWithSA(ctx, []string{"root"}, "", false, false, false, false, false, func(*drive.File) bool { return false })
			t.Logf("cancelled construction: service %s -> %s; active %d -> %d; next %s -> %s; healthy-parent request error=%v",
				filepath.Base(filepath.Dir(before.file))+"/"+filepath.Base(before.file),
				filepath.Base(filepath.Dir(after.file))+"/"+filepath.Base(after.file),
				activeBefore, f.ServiceAccountFiles.activeIdx, pendingBefore, pendingAfter, requestErr)
			require.NoError(t, requestErr)
			if after != before {
				t.Errorf("cancelled construction published a replacement service")
			}
			if f.ServiceAccountFiles.activeIdx != activeBefore || pendingAfter != pendingBefore {
				t.Errorf("cancelled construction consumed rotation position")
			}
		})
	}
}

func TestSAStrategyMissingCommittedAccountKeepsGroup(t *testing.T) {
	for _, rescan := range []bool{false, true} {
		name := "lazy-initial-scan"
		if rescan {
			name = "successful-rescan"
		}
		t.Run(name, func(t *testing.T) {
			ctx, f, paths, requests := crossFolderFixture(t, func(saListRequest) (int, any) {
				return 200, &drive.FileList{}
			}, nil)
			if !rescan {
				var err error
				f, err = newFs(ctx, "lazy-initial-scan", "", f.m)
				require.NoError(t, err)
				f.rootFolderID = "root"
				f.dirCache = dircache.New("", "root", f)
				require.True(t, f.ServiceAccountFiles.isPoolEmpty())
			}
			require.Equal(t, paths[0], f.currentService().file)
			require.NoError(t, os.Remove(paths[0]))
			if rescan {
				f.waitChangeSvc.Lock()
				err := f.ServiceAccountFiles.loadInfoFromDir(f.opt.ServiceAccountFilePath, f.currentService().file)
				f.waitChangeSvc.Unlock()
				require.NoError(t, err)
			}
			strategyList(t, ctx, f, true)
			got := requests()
			t.Logf("committed=p1/a.json (file removed); selected=%s; request account=%s",
				f.currentService().file, got[len(got)-1].account)
			require.Equal(t, paths[2], f.currentService().file, "eligible p2 must be preferred over another p1 account")
		})
	}
}
