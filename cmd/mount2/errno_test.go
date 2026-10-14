// Preserve namespace contention errors at the FUSE adapter boundary.
//go:build linux || (darwin && amd64)

package mount2

import (
	"context"
	"fmt"
	"syscall"
	"testing"

	"github.com/dogbutcat/gclone/vfs/vfscache"
	"github.com/stretchr/testify/require"
)

func TestNamespaceBusyErrno(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want syscall.Errno
	}{
		{"raw-busy", syscall.EBUSY, syscall.EBUSY},
		{"path-busy", &vfscache.PathBusyError{}, syscall.EBUSY},
		{"wrapped-busy", fmt.Errorf("rename recovery: %w", syscall.EBUSY), syscall.EBUSY},
		{"retry-control", syscall.EAGAIN, syscall.EAGAIN},
		{"cancel-control", context.Canceled, syscall.EINTR},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := translateError(tc.err)
			t.Logf("source=%v adapter=%v expected=%v", tc.err, got, tc.want)
			require.Equal(t, tc.want, got)
		})
	}
}
