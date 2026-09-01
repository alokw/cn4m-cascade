package mountmgr

import (
	"fmt"
	"strings"

	"github.com/alokw/cn4m-cascade/internal/store"
)

// dialectLadder is the fallback order from SPEC.md §5. Whichever rung
// succeeds is recorded on the target.
var dialectLadder = []string{"3.1.1", "3.0", "2.1"}

// Params are the process-level inputs to option resolution.
type Params struct {
	UID int
	GID int
}

// buildOptions produces the mount.cifs option string for a target at a given
// SMB dialect. The defaults are SPEC.md §5 verbatim; `soft` and
// `echo_interval` are the load-bearing ones — they make I/O return errors
// instead of hanging when the server disappears.
//
// The credentials file is not part of this string: ExecMounter prepends it,
// so an option string is always safe to log.
func buildOptions(t *store.Target, p Params, dialect string, multichannel bool) string {
	opts := newOptionList()

	opts.set("vers", dialect)
	opts.set("rsize", "4194304")
	opts.set("wsize", "4194304")
	opts.set("cache", "loose")
	opts.set("actimeo", "30")
	opts.flag("soft")
	// SPEC.md §5 gives 10 as the default. It is lowered here because
	// echo_interval alone governs how long the kernel takes to declare a
	// dead server dead, and 10 puts that at ~72s — far outside the ~30s
	// failure budget SPEC.md §11 asks for. Measured on the test harness:
	// 10 → ~72s, 3 → ~31-50s, 2 → ~43s, 1 → ~22s (noisy; ±20s). 2 keeps
	// some headroom against declaring a merely slow server dead; a target
	// can override it either way. See PROGRESS.md D-25.
	opts.set("echo_interval", "2")
	opts.set("uid", fmt.Sprintf("%d", p.UID))
	opts.set("gid", fmt.Sprintf("%d", p.GID))
	opts.set("iocharset", "utf8")

	// Force a separate TCP session per mount. Without it the kernel can
	// share a superblock between two mounts of the same host+share, which
	// would break the SPEC.md §5 requirement that two targets with different
	// credentials be genuinely distinct mounts (PROGRESS.md D-9).
	opts.flag("nosharesock")

	if t.IsGuest() {
		opts.flag("guest")
	}
	// Passed in rather than read from the target: the caller drops it and
	// retries when a server rejects it (SPEC.md §5).
	if multichannel {
		opts.flag("multichannel")
	}
	if t.Port > 0 {
		opts.set("port", fmt.Sprintf("%d", t.Port))
	}

	// User overrides win over every default above.
	opts.merge(t.MountOptsOverride)

	return opts.String()
}

// dialectsFor returns the ladder to try for a target. A user who pinned
// vers= in the advanced options gets exactly that and no fallback: they
// asked for a specific dialect, so silently mounting a different one would
// be wrong.
func dialectsFor(t *store.Target) []string {
	if pinned, ok := pinnedDialect(t.MountOptsOverride); ok {
		return []string{pinned}
	}
	return dialectLadder
}

// pinnedDialect reports the vers= value in an override string, if any.
func pinnedDialect(override string) (string, bool) {
	for _, part := range splitOptions(override) {
		k, v, found := strings.Cut(part, "=")
		if found && strings.TrimSpace(k) == "vers" {
			return strings.TrimSpace(v), true
		}
	}
	return "", false
}

// optionList is an insertion-ordered set of mount options, so the resulting
// string is stable and readable in logs.
type optionList struct {
	order []string
	byKey map[string]string // key -> rendered "k=v" or "k"
}

func newOptionList() *optionList {
	return &optionList{byKey: map[string]string{}}
}

func (o *optionList) set(key, value string) { o.put(key, key+"="+value) }
func (o *optionList) flag(key string)       { o.put(key, key) }

func (o *optionList) put(key, rendered string) {
	if _, exists := o.byKey[key]; !exists {
		o.order = append(o.order, key)
	}
	o.byKey[key] = rendered
}

// merge applies a user-supplied option string on top, replacing any option
// of the same name.
func (o *optionList) merge(override string) {
	for _, part := range splitOptions(override) {
		key, _, found := strings.Cut(part, "=")
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		if found {
			o.put(key, part)
		} else {
			o.put(key, key)
		}
	}
}

func (o *optionList) String() string {
	parts := make([]string, 0, len(o.order))
	for _, key := range o.order {
		parts = append(parts, o.byKey[key])
	}
	return strings.Join(parts, ",")
}

func splitOptions(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}
