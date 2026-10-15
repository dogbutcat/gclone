// Verify physical creation ownership across logical directory aliases.
package dircache

import (
	"context"
	"testing"
	"time"

	upstream "github.com/rclone/rclone/lib/dircache"
	"github.com/stretchr/testify/require"
)

type cxbDirectoryResolver interface {
	FindRoot(context.Context, bool) error
	FindDir(context.Context, string, bool) (string, error)
	Put(string, string)
}

func TestPhysicalAliasCreationCoalesces(t *testing.T) {
	for _, implementation := range []string{"candidate", "upstream"} {
		t.Run(implementation, func(t *testing.T) {
			ctx := context.Background()
			remote := newResolverRemote(t)
			remote.gateStep, remote.gateKey = "find", remoteKey("P", "child")
			var cache cxbDirectoryResolver
			if implementation == "candidate" {
				cache = New("", "root", remote)
			} else {
				cache = upstream.New("", "root", remote)
			}
			require.NoError(t, cache.FindRoot(ctx, false))
			cache.Put("left", "P")
			cache.Put("right", "P")
			resolve := func(name string) <-chan resolution {
				done := make(chan resolution, 1)
				go func() { id, err := cache.FindDir(ctx, name, true); done <- resolution{id, err} }()
				return done
			}
			first := resolve("left/child")
			awaitGate(t, remote)
			second := resolve("right/child")
			var secondResult resolution
			early := false
			select {
			case secondResult = <-second:
				early = true
			case <-time.After(300 * time.Millisecond):
			}
			remote.unblock()
			firstResult := awaitResult(t, first)
			if !early {
				secondResult = awaitResult(t, second)
			}
			require.NoError(t, firstResult.err)
			require.NoError(t, secondResult.err)
			t.Logf("second completed before old absence released=%v; ids=%q/%q; physical creates=%d", early, firstResult.id, secondResult.id, remote.count("P", "child"))
			require.Equal(t, 1, remote.count("P", "child"), "one physical parent/leaf was created twice through aliases")
			require.Equal(t, firstResult.id, secondResult.id)
		})
	}
}
