// Verify resolver concurrency, publication generations and physical creation ownership.
package dircache

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rclone/rclone/fs"
	"github.com/stretchr/testify/require"
)

var definiteRejection = errors.New("definite create rejection")

type resolverRemote struct {
	mu                sync.Mutex
	entries           map[string]string
	creates           map[string]int
	counter           int
	gateStep, gateKey string
	gateOnce          atomic.Bool
	entered           chan string
	release           chan struct{}
	releaseOnce       sync.Once
	gateAll           bool
	active, peak      atomic.Int32
	createErr         error
	commitOnError     bool
}

func newResolverRemote(t *testing.T) *resolverRemote {
	t.Helper()
	r := &resolverRemote{entries: make(map[string]string), creates: make(map[string]int), entered: make(chan string, 64), release: make(chan struct{})}
	r.gateOnce.Store(true)
	t.Cleanup(r.unblock)
	return r
}
func (r *resolverRemote) unblock()         { r.releaseOnce.Do(func() { close(r.release) }) }
func remoteKey(parent, leaf string) string { return parent + "\x00" + leaf }
func (r *resolverRemote) gate(ctx context.Context, step, parent, leaf string) error {
	count := r.active.Add(1)
	defer r.active.Add(-1)
	for old := r.peak.Load(); count > old; old = r.peak.Load() {
		if r.peak.CompareAndSwap(old, count) {
			break
		}
	}
	if step == r.gateStep && (r.gateAll || remoteKey(parent, leaf) == r.gateKey) && (r.gateAll || r.gateOnce.CompareAndSwap(true, false)) {
		r.entered <- parent + "/" + leaf
		select {
		case <-r.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}
func (r *resolverRemote) FindLeaf(ctx context.Context, parent, leaf string) (string, bool, error) {
	r.mu.Lock()
	id, found := r.entries[remoteKey(parent, leaf)]
	r.mu.Unlock()
	if err := r.gate(ctx, "find", parent, leaf); err != nil {
		return "", false, err
	}
	return id, found, nil
}
func (r *resolverRemote) CreateDir(ctx context.Context, parent, leaf string) (string, error) {
	if err := r.gate(ctx, "create", parent, leaf); err != nil {
		return "", err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	key := remoteKey(parent, leaf)
	r.creates[key]++
	r.counter++
	id := fmt.Sprintf("id-%d", r.counter)
	if r.createErr == nil || r.commitOnError {
		r.entries[key] = id
	}
	return id, r.createErr
}
func (r *resolverRemote) AmbiguousDirectoryCreate(err error) bool {
	return !errors.Is(err, definiteRejection)
}
func (r *resolverRemote) count(parent, leaf string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.creates[remoteKey(parent, leaf)]
}
func (r *resolverRemote) seed(parent, leaf, id string) {
	r.mu.Lock()
	r.entries[remoteKey(parent, leaf)] = id
	r.mu.Unlock()
}

func awaitGate(t *testing.T, r *resolverRemote) {
	t.Helper()
	select {
	case <-r.entered:
	case <-time.After(time.Second):
		t.Fatal("remote gate not entered")
	}
}

type resolution struct {
	id  string
	err error
}

func resolveAsync(c *DirCache, ctx context.Context, name string, create bool) <-chan resolution {
	result := make(chan resolution, 1)
	go func() { id, err := c.FindDir(ctx, name, create); result <- resolution{id, err} }()
	return result
}
func awaitResult(t *testing.T, result <-chan resolution) resolution {
	t.Helper()
	select {
	case r := <-result:
		return r
	case <-time.After(3 * time.Second):
		t.Fatal("resolver did not finish")
		return resolution{}
	}
}

func TestDistinctParentsOverlapAtAdmissionBound(t *testing.T) {
	ctx, config := fs.AddConfig(context.Background())
	config.Checkers = 4
	remote := newResolverRemote(t)
	remote.gateStep = "find"
	remote.gateAll = true
	cache := NewWithContext(ctx, "", "root", remote)
	require.NoError(t, cache.FindRoot(ctx, false))
	var results []<-chan resolution
	for i := range 16 {
		name := fmt.Sprintf("p%d", i)
		cache.Put(name, name)
		results = append(results, resolveAsync(cache, ctx, name+"/child", true))
	}
	for range 4 {
		awaitGate(t, remote)
	}
	select {
	case <-remote.entered:
		t.Fatal("admission bound exceeded")
	case <-time.After(25 * time.Millisecond):
	}
	remote.unblock()
	for _, result := range results {
		require.NoError(t, awaitResult(t, result).err)
	}
	require.Equal(t, int32(4), remote.peak.Load())
	cache.mu.Lock()
	defer cache.mu.Unlock()
	require.Empty(t, cache.creations)
	require.Empty(t, cache.flights)
}

func TestSameNameAndSharedAncestorsCreateOnce(t *testing.T) {
	remote := newResolverRemote(t)
	remote.gateStep = "create"
	remote.gateKey = remoteKey("root", "parent")
	cache := New("", "root", remote)
	var results []<-chan resolution
	for i := range 32 {
		leaf := "a"
		if i%2 != 0 {
			leaf = "b"
		}
		results = append(results, resolveAsync(cache, context.Background(), "parent/"+leaf, true))
	}
	awaitGate(t, remote)
	remote.unblock()
	ids := make(map[string]bool)
	for _, result := range results {
		row := awaitResult(t, result)
		require.NoError(t, row.err)
		ids[row.id] = true
	}
	require.Len(t, ids, 2)
	require.Equal(t, 1, remote.count("root", "parent"))
	parent, ok := cache.Get("parent")
	require.True(t, ok)
	require.Equal(t, 1, remote.count(parent, "a"))
	require.Equal(t, 1, remote.count(parent, "b"))
}

func TestInvalidationDoesNotDuplicateInFlightCreation(t *testing.T) {
	for _, reset := range []bool{false, true} {
		t.Run(fmt.Sprint(reset), func(t *testing.T) {
			remote := newResolverRemote(t)
			remote.gateStep = "create"
			remote.gateKey = remoteKey("root", "child")
			cache := New("", "root", remote)
			first := resolveAsync(cache, context.Background(), "child", true)
			awaitGate(t, remote)
			if reset {
				cache.ResetRoot()
			} else {
				cache.FlushDir("child")
			}
			second := resolveAsync(cache, context.Background(), "child", true)
			remote.unblock()
			a, b := awaitResult(t, first), awaitResult(t, second)
			require.NoError(t, a.err)
			require.NoError(t, b.err)
			require.Equal(t, a.id, b.id)
			require.Equal(t, 1, remote.count("root", "child"))
			current, ok := cache.Get("child")
			require.True(t, ok)
			require.Equal(t, a.id, current)
		})
	}
}

func TestCanceledWaiterDoesNotCancelSharedCreator(t *testing.T) {
	remote := newResolverRemote(t)
	remote.gateStep = "create"
	remote.gateKey = remoteKey("root", "child")
	cache := New("", "root", remote)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	first := resolveAsync(cache, ctx, "child", true)
	awaitGate(t, remote)
	second := resolveAsync(cache, context.Background(), "child", true)
	cancel()
	require.ErrorIs(t, awaitResult(t, first).err, context.Canceled)
	remote.unblock()
	require.NoError(t, awaitResult(t, second).err)
	require.Equal(t, 1, remote.count("root", "child"))
}

func TestAmbiguousCreationIsReconciledWithoutRetry(t *testing.T) {
	for _, committed := range []bool{false, true} {
		t.Run(fmt.Sprint(committed), func(t *testing.T) {
			remote := newResolverRemote(t)
			remote.createErr = context.DeadlineExceeded
			remote.commitOnError = committed
			cache := New("", "root", remote)
			id, err := cache.FindDir(context.Background(), "child", true)
			if committed {
				require.NoError(t, err)
				require.NotEmpty(t, id)
			} else {
				require.ErrorContains(t, err, "outcome unresolved")
				_, err = cache.FindDir(context.Background(), "child", true)
				require.ErrorContains(t, err, "outcome unresolved")
				remote.seed("root", "child", "late-id")
				id, err = cache.FindDir(context.Background(), "child", true)
				require.NoError(t, err)
				require.Equal(t, "late-id", id)
			}
			require.Equal(t, 1, remote.count("root", "child"))
		})
	}
}

func TestDefiniteRejectionCanBeRetried(t *testing.T) {
	remote := newResolverRemote(t)
	remote.createErr = definiteRejection
	cache := New("", "root", remote)
	_, err := cache.FindDir(context.Background(), "child", true)
	require.ErrorIs(t, err, definiteRejection)
	remote.mu.Lock()
	remote.createErr = nil
	remote.mu.Unlock()
	_, err = cache.FindDir(context.Background(), "child", true)
	require.NoError(t, err)
	require.Equal(t, 2, remote.count("root", "child"))
}

func TestListingPublicationUsesOriginalParentGeneration(t *testing.T) {
	remote := newResolverRemote(t)
	cache := New("", "root", remote)
	require.NoError(t, cache.FindRoot(context.Background(), false))
	cache.Put("a", "A")
	cache.Put("b", "B")
	token := cache.Observe(context.Background(), map[string]string{"a": "A", "b": "B", "": "root"})
	cache.FlushDir("a/child")
	require.False(t, cache.PutContext(token, "a/child", "stale"))
	require.True(t, cache.PutContext(token, "b/child", "unrelated"))
	cache.Put("new", "new-ID")
	require.False(t, cache.PutContext(token, "new", "old-ID"))
	cache.ResetRoot()
	require.False(t, cache.PutContext(token, "b/old", "stale-root"))
}

func TestNestedRootRejectsPreResetReply(t *testing.T) {
	remote := newResolverRemote(t)
	remote.seed("root", "a", "old-a")
	remote.seed("old-a", "b", "old-b")
	remote.gateStep = "find"
	remote.gateKey = remoteKey("root", "a")
	cache := New("a/b", "root", remote)
	first := resolveAsync(cache, context.Background(), "", false)
	awaitGate(t, remote)
	cache.ResetRoot()
	remote.seed("root", "a", "new-a")
	remote.seed("new-a", "b", "new-b")
	second := resolveAsync(cache, context.Background(), "", false)
	require.Equal(t, "new-b", awaitResult(t, second).id)
	remote.unblock()
	a := awaitResult(t, first)
	require.NoError(t, a.err)
	require.Equal(t, "new-b", a.id)
	parent, err := cache.RootParentID(context.Background(), false)
	require.NoError(t, err)
	require.Equal(t, "new-a", parent)
	leaf, parent, err := cache.FindPath(context.Background(), "", false)
	require.NoError(t, err)
	require.Equal(t, "b", leaf)
	require.Equal(t, "new-a", parent)
	_, provisional := cache.Get("a")
	require.False(t, provisional)
}

func TestMetadataCreatorSharesOrdinaryCreation(t *testing.T) {
	remote := newResolverRemote(t)
	remote.gateStep = "create"
	remote.gateKey = remoteKey("root", "child")
	cache := New("", "root", remote)
	ordinary := resolveAsync(cache, context.Background(), "child", true)
	awaitGate(t, remote)
	var metadataCalls atomic.Int32
	metadata := make(chan resolution, 1)
	go func() {
		id, err := cache.FindDirWithCreate(context.Background(), "child", func(ctx context.Context, parent, leaf string) (string, error) {
			metadataCalls.Add(1)
			return remote.CreateDir(ctx, parent, leaf)
		})
		metadata <- resolution{id, err}
	}()
	remote.unblock()
	a, b := awaitResult(t, ordinary), awaitResult(t, metadata)
	require.NoError(t, a.err)
	require.NoError(t, b.err)
	require.Equal(t, a.id, b.id)
	require.Equal(t, 1, remote.count("root", "child"))
	require.Zero(t, metadataCalls.Load())
}

func TestCacheShutdownReleasesWaiters(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	remote := newResolverRemote(t)
	remote.gateStep = "create"
	remote.gateKey = remoteKey("root", "child")
	cache := NewWithContext(ctx, "", "root", remote)
	result := resolveAsync(cache, context.Background(), "child", true)
	awaitGate(t, remote)
	cancel()
	require.ErrorIs(t, awaitResult(t, result).err, context.Canceled)
}

func TestCanceledRootParentRequestDoesNotCreate(t *testing.T) {
	remote := newResolverRemote(t)
	cache := New("a/b", "root", remote)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := cache.RootParentID(ctx, true)
	require.ErrorIs(t, err, context.Canceled)
	require.Zero(t, remote.count("root", "a"))
}
