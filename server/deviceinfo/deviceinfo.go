// Package deviceinfo collects facts about the host (OS, hardware model,
// hostname, app version) for the agent to publish during pair-init. The
// vault stores these alongside the device row so a multi-device user can
// distinguish "MacBook Pro at home" from "Mac mini in the office" in the
// /devices admin page.
//
// All lookups are best-effort: failures fall back to "" rather than
// refusing to pair. The vault accepts empty meta fields, so a partial
// fingerprint is always preferable to no fingerprint at all.
package deviceinfo

import (
	"os"
	"runtime"
)

// Version is the application version reported during pair. Override at
// build time with:
//
//	go build -ldflags="-X github.com/hoveychen/foxy-switcher/server/deviceinfo.Version=v1.2.3"
//
// release.yml wires this for tagged builds; un-overridden local builds
// surface as "dev".
var Version = "dev"

// Info captures the device facts an agent reports during pair-init.
// Empty fields are acceptable — the vault treats every meta field as
// optional, and platform-specific lookups silently fall back to "" on
// failure rather than refusing to pair.
type Info struct {
	Hostname   string
	OS         string // runtime.GOOS — "darwin" / "linux" / "windows"
	OSVersion  string // platform-specific (e.g. "14.5", "Ubuntu 22.04", "10.0.19045")
	Arch       string // runtime.GOARCH — "amd64" / "arm64"
	Model      string // hardware identifier (e.g. "MacBookPro18,3", "ThinkPad X1")
	AppVersion string
}

// Collect builds an Info from the current process and host. Cheap enough
// to call once per pair attempt; do not cache long-term because hostname
// and OS version can change after a reboot/upgrade.
func Collect() Info {
	hn, _ := os.Hostname()
	return Info{
		Hostname:   hn,
		OS:         runtime.GOOS,
		OSVersion:  osVersion(),
		Arch:       runtime.GOARCH,
		Model:      hardwareModel(),
		AppVersion: Version,
	}
}
