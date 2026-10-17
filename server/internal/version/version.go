// Package version carries build metadata injected with -ldflags:
//
//	-X github.com/yangguangdijia/xingshu-matrix/server/internal/version.Version=v1.0.0
//	-X github.com/yangguangdijia/xingshu-matrix/server/internal/version.Commit=$(git rev-parse --short HEAD)
//	-X github.com/yangguangdijia/xingshu-matrix/server/internal/version.BuildTime=$(date -u +%FT%TZ)
package version

import "runtime"

var (
	Version   = "dev"
	Commit    = "unknown"
	BuildTime = "unknown"
)

// Info is the payload of GET /api/v1/version.
type Info struct {
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	BuildTime string `json:"build_time"`
	GoVersion string `json:"go_version"`
	Platform  string `json:"platform"`
}

// Get returns the build info.
func Get() Info {
	return Info{Version: Version, Commit: Commit, BuildTime: BuildTime, GoVersion: runtime.Version(), Platform: runtime.GOOS + "/" + runtime.GOARCH}
}
