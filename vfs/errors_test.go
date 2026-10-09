// gclone-local VFS integration: copied from rclone v1.75.1; see vfs/UPSTREAM.txt.

package vfs

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestErrorError(t *testing.T) {
	assert.Equal(t, "Success", OK.Error())
	assert.Equal(t, "Function not implemented", ENOSYS.Error())
	assert.Equal(t, "Low level error 99", Error(99).Error())
}
