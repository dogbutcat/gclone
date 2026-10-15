// Independent cold directory paths must not queue behind unrelated remote work.
package drive

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/dogbutcat/gclone/internal/dircache"
	"github.com/stretchr/testify/require"
)

type coldDirectoryGate struct {
	phase            string
	entered, release chan struct{}
	once             sync.Once
}

func (g *coldDirectoryGate) wait(ctx context.Context, phase, leaf string) error {
	if phase == g.phase && leaf == "slow" {
		g.once.Do(func() { close(g.entered) })
		select {
		case <-g.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

func (g *coldDirectoryGate) FindLeaf(ctx context.Context, parent, leaf string) (string, bool, error) {
	return "", false, g.wait(ctx, "find", leaf)
}

func (g *coldDirectoryGate) CreateDir(ctx context.Context, parent, leaf string) (string, error) {
	if err := g.wait(ctx, "create", leaf); err != nil {
		return "", err
	}
	return parent + "/" + leaf, nil
}

func TestIndependentColdDirectoryDoesNotWait(t *testing.T) {
	for _, phase := range []string{"find", "create"} {
		t.Run(phase, func(t *testing.T) {
			ctx := context.Background()
			gate := &coldDirectoryGate{phase: phase, entered: make(chan struct{}), release: make(chan struct{})}
			var once sync.Once
			release := func() { once.Do(func() { close(gate.release) }) }
			defer release()
			f := &Fs{dirCache: dircache.New("", "root", gate)}
			require.NoError(t, f.dirCache.FindRoot(ctx, false))
			f.dirCache.Put("a", "parent-a")
			f.dirCache.Put("b", "parent-b")
			slow := make(chan error, 1)
			go func() { _, err := f.findDirectory(ctx, "a/slow", true); slow <- err }()
			select {
			case <-gate.entered:
			case <-time.After(5 * time.Second):
				t.Fatal("slow remote request not reached")
			}
			other := make(chan error, 1)
			go func() { _, err := f.findDirectory(ctx, "b/other", true); other <- err }()
			ready := false
			select {
			case err := <-other:
				require.NoError(t, err)
				ready = true
			case <-time.After(time.Second):
			}
			release()
			require.NoError(t, <-slow)
			if !ready {
				require.NoError(t, <-other)
			}
			require.True(t, ready, "independent cold directory waited for unrelated remote request")
		})
	}
}
