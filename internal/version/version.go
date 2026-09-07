// Package version carries the build identity of venapce-api.
//
// Version is stamped at link time with the very same tag the appliance release
// publishes, so what the operator sees in the log and in the panel is the image
// they pulled:
//
//	go build -ldflags "-X github.com/Venapce/venapce-api/internal/version.Version=v0.1.1"
//
// The getting-started Makefile passes VERSION into the product image build,
// which forwards it to that flag. An un-stamped build — `go run ./cmd/api`, or
// `make build` without VERSION — reports "dev".
package version

// Version is set at link time; never assign to it at runtime.
var Version string

// Current is the version to show. It tolerates an empty stamp (VERSION= passed
// as an empty string) so a build never reports a blank version.
func Current() string {
	if Version == "" {
		return "dev"
	}
	return Version
}
