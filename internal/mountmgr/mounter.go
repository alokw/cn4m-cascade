// Package mountmgr implements SPEC.md §5: on-demand kernel CIFS mounts with
// refcounting, stale-mount detection, bounded I/O and legible errors.
//
// Everything that touches the kernel goes through the Mounter interface so
// that the policy above it — option resolution, the version ladder,
// refcounting, error mapping — is unit-testable on any platform.
package mountmgr

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
)

// MountSpec is one attempt to mount one share.
type MountSpec struct {
	Source    string // //192.168.1.50/media
	Dir       string // /mnt/smb/<target-id>
	Options   string // resolved option string; never contains a password
	CredsFile string // path to a 0600 credentials file, or "" for guest
}

// MountInfo is one line of the kernel mount table.
type MountInfo struct {
	Dir     string
	FSType  string
	Source  string
	MountID string
}

// FSStat is the subset of statfs(2) we need to prove a mount is alive.
type FSStat struct {
	BlockSize   int64
	Blocks      uint64
	BlocksFree  uint64
	BlocksAvail uint64
}

// Mounter is the seam over the kernel and the mount.cifs binary.
type Mounter interface {
	// Mount runs mount.cifs. It must respect ctx cancellation.
	Mount(ctx context.Context, spec MountSpec) error
	// Unmount unmounts dir; lazy selects umount -l, which detaches a mount
	// whose server has gone away and cannot be unmounted normally.
	Unmount(ctx context.Context, dir string, lazy bool) error
	// Mounts reads the kernel mount table.
	Mounts() ([]MountInfo, error)
	// StatFS must return within ctx: on a dead SMB server the underlying
	// syscall can block indefinitely.
	StatFS(ctx context.Context, dir string) (FSStat, error)
}

// ExecMounter is the real Mounter: it shells out to mount.cifs and umount.
type ExecMounter struct {
	// MountBin and UmountBin are overridable for testing; empty means the
	// standard names, resolved via PATH.
	MountBin  string
	UmountBin string
}

// NewExecMounter returns a Mounter backed by the system mount utilities.
func NewExecMounter() *ExecMounter { return &ExecMounter{} }

func (m *ExecMounter) mountBin() string {
	if m.MountBin != "" {
		return m.MountBin
	}
	return "mount.cifs"
}

func (m *ExecMounter) umountBin() string {
	if m.UmountBin != "" {
		return m.UmountBin
	}
	return "umount"
}

// Mount execs mount.cifs. Credentials are passed as a path to a 0600 file
// (credentials=), never as an option value, so they never appear in the
// process table (SPEC.md §5).
func (m *ExecMounter) Mount(ctx context.Context, spec MountSpec) error {
	opts := spec.Options
	if spec.CredsFile != "" {
		opts = "credentials=" + spec.CredsFile + "," + opts
	}

	cmd := exec.CommandContext(ctx, m.mountBin(), spec.Source, spec.Dir, "-o", opts)
	var stderr, stdout bytes.Buffer
	cmd.Stderr = &stderr
	cmd.Stdout = &stdout

	err := cmd.Run()
	if err == nil {
		return nil
	}

	exitCode := -1
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		exitCode = exitErr.ExitCode()
	}
	// mount.cifs writes its diagnostics to stdout in some versions.
	detail := strings.TrimSpace(stderr.String())
	if detail == "" {
		detail = strings.TrimSpace(stdout.String())
	}
	return classifyMountFailure(spec.Source, exitCode, detail, ctx.Err())
}

// Unmount unmounts dir, optionally lazily.
func (m *ExecMounter) Unmount(ctx context.Context, dir string, lazy bool) error {
	args := []string{}
	if lazy {
		args = append(args, "-l")
	}
	args = append(args, dir)

	cmd := exec.CommandContext(ctx, m.umountBin(), args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		detail := strings.TrimSpace(stderr.String())
		// Already gone is success as far as callers are concerned.
		if strings.Contains(detail, "not mounted") || strings.Contains(detail, "not found") {
			return nil
		}
		return fmt.Errorf("unmounting %s: %s: %w", dir, detail, err)
	}
	return nil
}

// Mounts parses /proc/self/mountinfo.
func (m *ExecMounter) Mounts() ([]MountInfo, error) {
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return nil, fmt.Errorf("reading the kernel mount table: %w", err)
	}
	defer f.Close()
	return parseMountInfo(f)
}

// parseMountInfo reads the mountinfo format:
//
//	36 35 98:0 /mnt1 /mnt rw,noatime - ext3 /dev/root rw,errors=continue
//	                                  ^ separator; fields after it are
//	                                    fstype, source, super options
//
// Optional fields sit between the root/mountpoint fields and the separator,
// so the fstype and source can only be found relative to the "-".
func parseMountInfo(r io.Reader) ([]MountInfo, error) {
	var infos []MountInfo
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 7 {
			continue
		}
		sep := -1
		for i, f := range fields {
			if f == "-" {
				sep = i
				break
			}
		}
		if sep == -1 || sep+2 >= len(fields) {
			continue
		}
		infos = append(infos, MountInfo{
			MountID: fields[0],
			// Mountpoints containing spaces are escaped as \040 by the kernel.
			Dir:    unescapeMountField(fields[4]),
			FSType: fields[sep+1],
			Source: unescapeMountField(fields[sep+2]),
		})
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("reading the kernel mount table: %w", err)
	}
	return infos, nil
}

// unescapeMountField reverses the kernel's octal escaping of space, tab,
// newline and backslash in mountinfo paths.
func unescapeMountField(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			var v int
			if _, err := fmt.Sscanf(s[i+1:i+4], "%03o", &v); err == nil {
				b.WriteByte(byte(v))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}
