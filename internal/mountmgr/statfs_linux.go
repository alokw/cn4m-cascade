//go:build linux

package mountmgr

import (
	"context"
	"fmt"
	"syscall"
)

// StatFS runs statfs(2) in a goroutine and selects on ctx. The syscall
// itself cannot be cancelled, but this bounds how long the *caller* waits:
// on a dead SMB server statfs can block until the CIFS timeout expires,
// which is exactly the hang SPEC.md §5 forbids.
//
// The goroutine is left to finish on its own and writes to a buffered
// channel, so it never leaks on the cancellation path.
func (m *ExecMounter) StatFS(ctx context.Context, dir string) (FSStat, error) {
	type result struct {
		stat FSStat
		err  error
	}
	ch := make(chan result, 1)

	go func() {
		var st syscall.Statfs_t
		if err := syscall.Statfs(dir, &st); err != nil {
			ch <- result{err: fmt.Errorf("statfs %s: %w", dir, err)}
			return
		}
		ch <- result{stat: FSStat{
			BlockSize:   int64(st.Bsize),
			Blocks:      st.Blocks,
			BlocksFree:  st.Bfree,
			BlocksAvail: st.Bavail,
		}}
	}()

	select {
	case res := <-ch:
		return res.stat, res.err
	case <-ctx.Done():
		return FSStat{}, fmt.Errorf("checking %s did not respond in time: %w", dir, ctx.Err())
	}
}
