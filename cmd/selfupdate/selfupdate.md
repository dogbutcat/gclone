This command downloads the latest release of gclone and replaces the
currently running binary, parallel working with rclone's `selfupdate`

You can check in advance what version would be installed by adding the
`--check` flag, then repeat the command without it when you are satisfied.

Sometimes the rclone team may recommend you a concrete beta or stable
rclone release to troubleshoot your issue or add a bleeding edge feature.
The `--version VER` flag, if given, will update to the concrete version
instead of the latest one. If you omit micro version from `VER` (for
example `1.64.0-mod1.6.0`), the latest matching micro version will be used.

Upon successful update gclone will print a message that contains a previous
version number. You will need it if you later decide to revert your update
for some reason. Then you'll have to note the previous version and run the
following command: `gclone gselfupdate --version OLDVER`.
(if you are a developer and use a locally built rclone, the version number
will end with `-DEV`, you will have to rebuild it as it obviously can't
be distributed).

If you previously installed rclone via a package manager, the package may
include local documentation or configure services. You may wish to update
with the flag `--package deb` or `--package rpm` (whichever is correct for
your OS) to update these too. This command with the default `--package zip`
will update only the rclone executable so the local manual may become
inaccurate after it.

The [gclone mount](/commands/rclone_mount/) command may
or may not support extended FUSE options depending on the build and OS.
`gselfupdate` will refuse to update if the capability would be discarded.

Note: Windows forbids deletion of a currently running executable so this
command will rename the old executable to 'gclone.old.exe' upon success.

Please note that this command was not available before gclone version `1.64.0-mod1.6.0`.
If it fails for you with the message `unknown command "gselfupdate"` then
you will need to update manually following the install instructions located
at https://github.com/dogbutcat/gclone

Example:
```sh
gclone gselfupdate [--check]
\ [--output [of]]
\ [--version [v]]
\ [--package [zip|deb|rpm]]
```