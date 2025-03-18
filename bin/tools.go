// This file is no meaningful except for mod tidy keeping those indirect mod(s) avoid to be deleted.
// those mod(s) is relevant with build action part, no infurence with local build or functionality
// DO NOT remove those if needing github-action work.
package main

import (
	_ "github.com/dop251/scsu"
	_ "golang.org/x/mobile/event/key"
)
