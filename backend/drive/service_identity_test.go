// Verify same-account control operations on a copied Fs use committed SA identity.
package drive

import (
	"context"
	"sync"
	"testing"

	"github.com/rclone/rclone/fs/config/configmap"
	"github.com/stretchr/testify/require"
)

func TestRootCopyNoopSwitchPreservesActivePoolAccount(t *testing.T) {
	pool := &SaInfo{}
	pool.updateSas([]string{"account-A", "account-B"}, "account-A")
	f := &Fs{
		opt:                 Options{ServiceAccountFile: "account-A"},
		ServiceAccountFiles: pool,
		waitChangeSvc:       new(sync.Mutex),
		serviceState:        new(serviceState),
		m:                   configmap.Simple{},
	}
	f.publishServiceLocked()
	copy := *f
	f.opt.ServiceAccountFile = "account-B"
	f.publishServiceLocked()
	pool.activeSa("account-B")
	_, err := copy.Command(context.Background(), "set", nil, map[string]string{"service_account_file": "account-B"})
	require.NoError(t, err)
	t.Logf("committed=%q; copied options=%q; pool active=%q", copy.currentService().file, copy.opt.ServiceAccountFile, pool.sas[pool.activeIdx].saPath)
	require.Equal(t, "account-B", copy.currentService().file)
	require.Equal(t, "account-B", pool.sas[pool.activeIdx].saPath, "no-op switch regressed pool identity to the copied stale options")
}
