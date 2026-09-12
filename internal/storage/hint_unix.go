//go:build !windows

package storage

// missingRootHint is the advice attached to a local target whose root is
// absent.
//
// On Linux the server runs in a container (SPEC.md §3), so the overwhelmingly
// likely cause is that the host directory was never mapped in — the path exists
// on the machine and not in the container.
const missingRootHint = "check that the host directory is bind-mounted into the container"
