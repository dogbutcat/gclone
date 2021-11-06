module github.com/dogbutcat/gclone

replace github.com/jlaffaye/ftp => github.com/rclone/ftp v1.0.0-210902f

require (
	github.com/pkg/errors v0.9.1
	github.com/rclone/rclone v1.57.0
	github.com/spf13/cobra v1.2.1
	github.com/stretchr/testify v1.7.0
	golang.org/x/oauth2 v0.0.0-20210819190943-2bc19b11175f
	google.golang.org/api v0.54.0
)

go 1.14
