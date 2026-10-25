// Package version holds the version every vigil binary reports. It is set
// at build time, the same way for all of them:
//
//	go build -ldflags "-X github.com/ijcodelabs/vigil/internal/version.Version=v1.2.3"
package version

// Version is "dev" in a build that did not set it.
var Version = "dev"
