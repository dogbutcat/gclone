// gclone-local VFS integration: copied from rclone v1.75.1; see vfs/UPSTREAM.txt.

// Package proxyflags implements command line flags to set up a proxy
package proxyflags

import (
	"github.com/dogbutcat/gclone/cmd/serve/proxy"
	"github.com/rclone/rclone/fs/config/flags"
	"github.com/spf13/pflag"
)

// AddFlags adds the non filing system specific flags to the command
func AddFlags(flagSet *pflag.FlagSet) {
	flags.AddFlagsFromOptions(flagSet, "", proxy.OptionsInfo)
}
