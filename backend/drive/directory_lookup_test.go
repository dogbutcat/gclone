// Reproduce unrelated lookups queuing behind a slow Drive directory creation.
package drive

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/dogbutcat/gclone/internal/dircache"
	"github.com/stretchr/testify/require"
)

type directoryLookupGate struct {
	entered, release chan struct{}
	once             sync.Once
}

func (g *directoryLookupGate) FindLeaf(_ context.Context, parent, leaf string) (string, bool, error) {
	if leaf == "nested" {
		return "nested-id", true, nil
	}
	return "", false, nil
}
func (g *directoryLookupGate) CreateDir(context.Context, string, string) (string, error) {
	g.once.Do(func() { close(g.entered) })
	<-g.release
	return "created-id", nil
}

func TestCachedDirectoryLookupDoesNotWaitForCreate(t *testing.T) {
	for _, parentLookup := range []bool{false, true} {
		t.Run(map[bool]string{false: "directory", true: "file-parent"}[parentLookup], func(t *testing.T) {
			gate := &directoryLookupGate{entered: make(chan struct{}), release: make(chan struct{})}
			f := &Fs{dirCache: dircache.New("", "root-id", gate)}
			require.NoError(t, f.dirCache.FindRoot(context.Background(), false))
			f.dirCache.Put("known", "known-id")
			creation := make(chan error, 1)
			go func() { _, err := f.dirCache.FindDir(context.Background(), "slow", true); creation <- err }()
			<-gate.entered
			result := make(chan string, 1)
			go func() {
				var id string
				var err error
				if parentLookup {
					_, id, err = f.findParentDirectory(context.Background(), "known/data", true)
				} else {
					id, err = f.findDirectory(context.Background(), "known", false)
				}
				if err != nil {
					result <- err.Error()
				} else {
					result <- id
				}
			}()
			blocked := false
			select {
			case id := <-result:
				require.Equal(t, "known-id", id)
			case <-time.After(100 * time.Millisecond):
				blocked = true
			}
			close(gate.release)
			require.NoError(t, <-creation)
			if blocked {
				<-result
				t.Fatal("cached directory lookup waited for unrelated network creation")
			}
		})
	}
}

func TestDirectoryLookupPreservesNestedRootResolution(t *testing.T) {
	gate := &directoryLookupGate{entered: make(chan struct{}), release: make(chan struct{})}
	f := &Fs{root: "nested", dirCache: dircache.New("nested", "true-root-id", gate)}
	id, err := f.findDirectory(context.Background(), "", false)
	require.NoError(t, err)
	require.Equal(t, "nested-id", id, "initial root cache entry is relative to the true root")
	leaf, parent, err := f.findParentDirectory(context.Background(), "", false)
	require.NoError(t, err)
	require.Equal(t, "nested", leaf)
	require.Equal(t, "true-root-id", parent)
}
