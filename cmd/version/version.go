// Package version provides the version command.
package version

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/coreos/go-semver/semver"
	"github.com/rclone/rclone/cmd"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/config/flags"
	"github.com/rclone/rclone/fs/fshttp"
	"github.com/spf13/cobra"
)

var (
	check = false
)

func init() {
	cmd.Root.AddCommand(commandDefinition)
	cmdFlags := commandDefinition.Flags()
	flags.BoolVarP(cmdFlags, &check, "check", "", false, "Check for new version", "")
}

var commandDefinition = &cobra.Command{
	Use:   "gversion",
	Short: `Show the version number.`,
	Long: `
Show the gclone version number, the go version, the build target
OS and architecture, the runtime OS and kernel version and bitness,
build tags and the type of executable (static or dynamic).

For example:

    $ gclone version
    gclone v1.55.0
    - os/version: ubuntu 18.04 (64 bit)
    - os/kernel: 4.15.0-136-generic (x86_64)
    - os/type: linux
    - os/arch: amd64
    - go/version: go1.16
    - go/linking: static
    - go/tags: none

Note: before gclone version 1.55 the os/type and os/arch lines were merged,
      and the "go/version" line was tagged as "go version".

If you supply the --check flag, then it will do an online check to
compare your version with the latest release and the latest beta.

    $ gclone version --check
    yours:  1.42.0.6
    latest: 1.42          (released 2018-06-16)
    beta:   1.42.0.5      (released 2018-06-17)

Or

    $ gclone version --check
    yours:  1.41
    latest: 1.42          (released 2018-06-16)
      upgrade: https://downloads.rclone.org/v1.42
    beta:   1.42.0.5      (released 2018-06-17)
      upgrade: https://beta.rclone.org/v1.42-005-g56e1e820

`,
	Annotations: map[string]string{
		"versionIntroduced": "v1.64",
	},
	Run: func(command *cobra.Command, args []string) {
		ctx := context.Background()
		cmd.CheckArgs(0, 0, command, args)
		if check {
			CheckVersion(ctx)
		} else {
			cmd.ShowVersion()
		}
	},
}

// strip a leading v off the string
func stripV(s string) (string, string) {
	if len(s) > 0 && s[0] == 'v' {
		delimeter := "-"
		if strings.Contains(s, "-mod") {
			delimeter = "-mod"
		}
		p := strings.Split(s[1:], delimeter)
		if p[1] == "DEV" {
			p[1] = "1.0.0"
		}
		return p[0], p[1]
	}
	return s, "1.0.0"
}

// GetVersion gets the version available for download
func GetVersion(ctx context.Context, url string) (v *semver.Version, vs string, date time.Time, err error) {
	resp, err := fshttp.NewClient(ctx).Get(url)
	if err != nil {
		return v, vs, date, err
	}
	defer fs.CheckClose(resp.Body, &err)
	if resp.StatusCode != http.StatusOK {
		return v, vs, date, errors.New(resp.Status)
	}
	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return v, vs, date, err
	}
	vs = strings.TrimSpace(string(bodyBytes))
	vs = strings.TrimPrefix(vs, "gclone ")
	vs = strings.TrimRight(vs, "β")
	date, err = http.ParseTime(resp.Header.Get("Last-Modified"))
	if err != nil {
		return v, vs, date, err
	}
	// v, err = semver.NewVersion(stripV(vs))
	return v, vs, date, err
}

// CheckVersion checks the installed version against available downloads
func CheckVersion(ctx context.Context) {
	// fs.Version = "v1.62.1-mod1.5.2" // TODO: remember to takedown
	v, m := stripV(fs.Version)
	vCurrent, err := semver.NewVersion(v)
	vModCurrent, errMod := semver.NewVersion(m)
	// vCurrent, err := semver.NewVersion(stripV(fs.Version))
	if err != nil {
		fs.Errorf(nil, "Failed to parse version: %v", err)
	}
	if errMod != nil {
		fs.Errorf(nil, "Failed to parse mod version: %v", errMod)
	}
	const timeFormat = "2006-01-02"

	printVersion := func(what, url string) {
		_, vs, t, err := GetVersion(ctx, url+"/download/version.txt")
		vNew, vMod := stripV(vs)
		v, _ := semver.NewVersion(vNew)
		m, _ := semver.NewVersion(vMod)
		if err != nil {
			fs.Errorf(nil, "Failed to get gclone %s version: %v", what, err)
			return
		}
		fmt.Printf("%-8s%-40v %20s\n",
			what+":",
			vs,
			"(released "+t.Format(timeFormat)+")",
		)
		if v.Compare(*vCurrent) > 0 {
			fmt.Printf("  upgrade: %s\n", url)
			return
		}
		if v.Compare(*vCurrent) == 0 && m.Compare(*vModCurrent) > 0 {
			fmt.Printf("  upgrade: %s\n", url)
			return
		}
	}
	fmt.Printf("yours:  %-13s\n", fs.Version)
	printVersion(
		"latest",
		"https://github.com/dogbutcat/gclone/releases/latest",
	)
	// printVersion(
	// 	"beta",
	// 	"https://beta.rclone.org/",
	// )
	if strings.HasSuffix(fs.Version, "-DEV") {
		fmt.Println("Your version is compiled from git so comparisons may be wrong.")
	}
}

func ConvertV(s string) (v *semver.Version, m *semver.Version) {
	v1, v2 := stripV(s)
	v, _ = semver.NewVersion(v1)
	m, _ = semver.NewVersion(v2)
	return v, m
}
