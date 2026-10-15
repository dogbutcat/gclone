// Publish complete SA client tuples without making readers wait for rotation I/O.
package drive

import (
	"sync"
	"sync/atomic"
)

// The pointer is shared by Fs copies used during root resolution. Rotation
// remains serialized by waitChangeSvc; readers use the last committed tuple.
type serviceState struct {
	current atomic.Pointer[saService]
	queryMu sync.Mutex
}

func (f *Fs) publishServiceLocked() {
	if f.serviceState != nil {
		current := &saService{svc: f.svc, v2Svc: f.v2Svc, client: f.client, file: f.opt.ServiceAccountFile}
		f.serviceState.current.Store(current)
	}
}

func (f *Fs) recordQuery(query string) {
	if f.serviceState != nil {
		f.serviceState.queryMu.Lock()
		f.lastQuery = query
		f.serviceState.queryMu.Unlock()
	} else {
		// Hand-built fixtures without a publication holder retain the old guard.
		f.waitChangeSvc.Lock()
		f.lastQuery = query
		f.waitChangeSvc.Unlock()
	}
}
