// Verify waiter freshness and descendant binding ownership at directory lock handoffs.
package vfs

import (
	"path"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/rclone/rclone/fs"
	"github.com/stretchr/testify/require"
)

func TestRefreshWaiterRechecksInvalidation(t *testing.T) {
	_, root, remote := directoryRefreshFixture(t)
	root.invalidateDir("")
	remote.blockList.Store(true)
	refreshed := make(chan error, 1)
	go func() {
		root.mu.Lock()
		err := root.refreshDirectory(true, true)
		if err == nil {
			// Schedule an invalidation immediately after refresh completion,
			// before the waiting Stat can reacquire Dir.mu. These are the
			// namespace mutations performed by ForgetAll without virtuals.
			root.generation++
			root.read = time.Time{}
			root.items = make(map[string]Node)
			root.namespaceUpdated = nil
			root.cleanupTimer.Stop()
		}
		root.mu.Unlock()
		refreshed <- err
	}()
	<-remote.listEntered
	waited := make(chan error, 1)
	go func() { _, err := root.Stat("other"); waited <- err }()
	require.Eventually(t, func() bool {
		buffer := make([]byte, 1<<20)
		n := runtime.Stack(buffer, true)
		for _, stack := range strings.Split(string(buffer[:n]), "\n\n") {
			if strings.Contains(stack, "(*Dir).stat(") && strings.Contains(stack, "(*Dir).refreshDirectory(") && strings.Contains(stack, "[select]") {
				return true
			}
		}
		return false
	}, time.Second, time.Millisecond)
	close(remote.releaseList)
	require.NoError(t, <-refreshed)
	waitErr := <-waited
	_, retryErr := root.Stat("other")
	t.Logf("strict waiter=%v; subsequent fresh Stat=%v", waitErr, retryErr)
	require.NoError(t, retryErr)
	require.NoError(t, waitErr, "completed old flight cannot satisfy a newer invalidation")
}

func TestAncestorWalkPreservesCompletedChildRename(t *testing.T) {
	_, root, _ := directoryRefreshFixture(t)
	parent, err := root.Mkdir("parent")
	require.NoError(t, err)
	child, err := parent.Mkdir("child")
	require.NoError(t, err)
	child.mu.Lock()
	childLocked := true
	defer func() {
		if childLocked {
			child.mu.Unlock()
		}
	}()
	walked := make(chan struct{})
	go func() { parent.renameTree("parent-renamed"); close(walked) }()
	require.Eventually(t, func() bool {
		root.mu.RLock()
		defer root.mu.RUnlock()
		return root.items["parent-renamed"] == parent
	}, time.Second, time.Millisecond)
	// The ancestor releases parent.mu while waiting for child.mu. Publish
	// the complete same-parent child rename before resuming that older walk.
	parent.mu.Lock()
	delete(parent.items, "child")
	parent.items["moved"] = child
	parent.noteNamespaceChange("child")
	parent.noteNamespaceChange("moved")
	child.path = path.Join(parent.path, "moved")
	child.entry = fs.NewDir(child.path, time.Now())
	child.generation++
	parent.mu.Unlock()
	child.mu.Unlock()
	childLocked = false
	select {
	case <-walked:
	case <-time.After(time.Second):
		t.Fatal("ancestor rename walk did not finish")
	}
	parent.mu.RLock()
	moved, reverted := parent.items["moved"], parent.items["child"]
	parent.mu.RUnlock()
	t.Logf("child path=%q; moved binding=%v; old binding=%v", child.Path(), moved != nil, reverted != nil)
	require.True(t, moved == child, "older ancestor walk reverted a completed child rename")
	require.Nil(t, reverted)
	require.Equal(t, "parent-renamed/moved", child.Path())
}
