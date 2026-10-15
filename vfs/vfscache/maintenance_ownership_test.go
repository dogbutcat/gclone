// Exercise maintenance admission against captured users and namespace generations.
package vfscache

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func findBinding(t *testing.T, c *Cache, name string) cacheBinding {
	t.Helper()
	for _, binding := range c.bindings() {
		if binding.name == name {
			return binding
		}
	}
	t.Fatalf("missing binding %q", name)
	return cacheBinding{}
}

func TestMaintenancePinsCapturedItemAndName(t *testing.T) {
	_, c := newTestCache(t)
	item, other := c.Item("slow"), c.Item("other")
	owner := c.claimMaintenance(findBinding(t, c, "slow"))
	require.NotNil(t, owner)
	finished := false
	defer func() {
		if !finished {
			owner.finish(false, nil)
		}
	}()
	opened := make(chan error, 1)
	go func() { opened <- item.Open(nil) }()
	require.Eventually(t, func() bool { item.mu.Lock(); defer item.mu.Unlock(); return item.maintenanceWaiters == 1 }, time.Second, time.Millisecond)
	lookup := make(chan *Item, 1)
	go func() { lookup <- c.Item("slow") }()
	require.Same(t, other, c.Item("other"))
	select {
	case <-opened:
		t.Fatal("captured open crossed maintenance")
	default:
	}
	select {
	case <-lookup:
		t.Fatal("same-name lookup crossed maintenance")
	default:
	}
	removed, _ := item.RemoveNotInUse(0, false)
	require.True(t, removed)
	owner.finish(removed, nil)
	finished = true
	require.NoError(t, <-opened)
	require.Same(t, item, <-lookup, "waiting captured user retains the cleaned lifetime")
	require.Same(t, item, c.Item("slow"))
	_, err := item.WriteAt([]byte("accepted"), 0)
	require.NoError(t, err)
	require.NoError(t, item.Close(nil))
}

func TestMaintenanceSkipsActiveAccess(t *testing.T) {
	_, c := newTestCache(t)
	item := c.Item("slow")
	item.preAccess()
	require.Nil(t, c.claimMaintenance(findBinding(t, c, "slow")))
	item.postAccess()
	owner := c.claimMaintenance(findBinding(t, c, "slow"))
	require.NotNil(t, owner)
	owner.finish(false, nil)
}

func TestMaintenanceRejectsRenameABA(t *testing.T) {
	_, c := newTestCache(t)
	item := c.Item("source")
	require.NoError(t, item.Open(nil))
	require.NoError(t, item.Close(nil))
	old := findBinding(t, c, "source")
	require.NoError(t, c.Rename("source", "moved", nil))
	require.NoError(t, c.Rename("moved", "source", nil))
	require.Same(t, item, c.Item("source"))
	require.Nil(t, c.claimMaintenance(old), "same pointer is not the original binding generation")
}

func TestEvictionDoesNotRetainPathEpochs(t *testing.T) {
	_, c := newTestCache(t)
	for _, name := range []string{"a", "b", "c"} {
		c.Item(name)
	}
	c.purgeOld(0)
	c.mu.Lock()
	defer c.mu.Unlock()
	require.Empty(t, c.item)
	require.Empty(t, c.epochs)
	require.NotZero(t, c.bindingEpoch)
}
