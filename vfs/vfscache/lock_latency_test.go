// Verify that a blocked cache item cannot monopolize the cache namespace.
package vfscache

import (
	"fmt"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestBlockedItemDoesNotBlockUnrelatedCacheLookup(t *testing.T) {
	for _, operation := range []string{"dirty-item", "age-cleaner", "usage-scan", "in-use-scan", "reservation", "register"} {
		t.Run(operation, func(t *testing.T) {
			_, cache := newTestCache(t)
			slow, other := cache.Item("slow"), cache.Item("other")
			if operation == "dirty-item" {
				slow.info.Dirty = true
			}
			slow.mu.Lock()
			var once sync.Once
			release := func() { once.Do(slow.mu.Unlock) }
			defer release()
			started, finished := make(chan struct{}), make(chan error, 1)
			go func() {
				close(started)
				var err error
				switch operation {
				case "dirty-item":
					if got := cache.DirtyItem("slow"); got != slow {
						err = fmt.Errorf("busy dirty item was reported as absent")
					}
				case "age-cleaner":
					cache.purgeOld(time.Hour)
				case "usage-scan":
					cache.updateUsed()
				case "in-use-scan":
					cache.TotalInUse()
				case "reservation":
					var reservation *pathMutation
					reservation, err = cache.reservePaths("slow")
					if reservation != nil {
						reservation.Close()
					}
				case "register":
					err = cache.put("slow", slow)
				}
				finished <- err
			}()
			<-started
			// Confirm the operation actually reached its blocked mutex. This
			// avoids accepting a probe which merely won the scheduler race.
			method := map[string]string{"dirty-item": "DirtyItem", "age-cleaner": "purgeOld", "usage-scan": "updateUsed", "in-use-scan": "TotalInUse", "reservation": "reservePaths", "register": "put"}[operation]
			require.Eventually(t, func() bool {
				buffer := make([]byte, 1<<20)
				n := runtime.Stack(buffer, true)
				for _, stack := range strings.Split(string(buffer[:n]), "\n\n") {
					if strings.Contains(stack, "(*Cache)."+method+"(") && strings.Contains(stack, "sync.Mutex.Lock") {
						return true
					}
				}
				return false
			}, time.Second, time.Millisecond)
			lookup := make(chan *Item, 1)
			go func() { lookup <- cache.Item("other") }()
			ready := false
			select {
			case got := <-lookup:
				require.Same(t, other, got)
				ready = true
			case <-time.After(time.Second):
			}
			release()
			select {
			case err := <-finished:
				require.NoError(t, err)
			case <-time.After(5 * time.Second):
				t.Fatal("gated operation did not finish after release")
			}
			if !ready {
				require.Same(t, other, <-lookup)
			}
			require.True(t, ready, "unrelated cache lookup waited for another item")
		})
	}
}
