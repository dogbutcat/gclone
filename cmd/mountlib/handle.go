// Bind cached descriptors before a mount acknowledges a successful OS open.
package mountlib

import (
	"errors"

	"github.com/dogbutcat/gclone/vfs"
)

// PrepareHandle pins the cache lifetime without downloading file contents.
// Kernel page-cache reads may never call ReadAt; late mmap writes after unlink
// must still use this accepted descriptor's generation. Failed admission closes
// the handle so no writer or cache reference is leaked.
func PrepareHandle(handle vfs.Handle) error {
	if cached, ok := handle.(*vfs.RWFileHandle); ok {
		if err := cached.Prepare(); err != nil {
			return errors.Join(err, handle.Close())
		}
	}
	return nil
}
