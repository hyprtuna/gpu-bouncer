package cli

import "runtime/debug"

// resolveVersion picks the version string to report. The release ldflag wins
// when it was set. Otherwise the module version Go recorded in the binary is
// used, which `go install module@vX.Y.Z` fills in; a plain `go build` in a
// working tree records "(devel)", and that reports as dev.
func resolveVersion(ldflag string, info *debug.BuildInfo) string {
	if ldflag != "" && ldflag != "dev" {
		return ldflag
	}
	if info != nil && info.Main.Version != "" && info.Main.Version != "(devel)" {
		return info.Main.Version
	}
	return "dev"
}

// version is what `gpu-bouncer version` prints.
func version() string {
	info, _ := debug.ReadBuildInfo()
	return resolveVersion(Version, info)
}
