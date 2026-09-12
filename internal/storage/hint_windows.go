//go:build windows

package storage

// missingRootHint is the advice attached to a local target whose root is
// absent.
//
// The native Windows build has no container and nothing is mapped anywhere, so
// the container advice would send an operator looking for a bind mount that was
// never involved. A missing path here is a missing path: a drive that is not
// attached, a share that is not connected, or a typo.
const missingRootHint = "check that the path exists and that this service can reach it — " +
	"a disconnected drive or an unmapped network path looks the same as a typo"
