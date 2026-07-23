// Package storage holds the startup health check for the shared POSIX
// filesystem backend.
//
// When storage.workspace.backend == "shared" (see internal/config), the
// per-user data root {data-dir}/data is expected to be an operator-mounted
// shared POSIX filesystem (a MinIO-backed JuiceFS FUSE mount). The most
// dangerous and most easily-missed operational failure is "one node forgot to
// mount JuiceFS, so {data-dir}/data degrades to a local directory and that node
// silently writes local disk, diverging from the rest of the cluster". The
// health check in this package refuses to let the process start in that state.
//
// The check is deliberately narrow: it does not embed or orchestrate any
// filesystem client (mounting is the operator's job, see the
// workspace-shared-storage spec) — it only verifies the mount that should
// already be there.
package storage

import (
	"fmt"
	"os"
	"path/filepath"

	"go.uber.org/zap"
)

// probeFileName is the throwaway file the writable probe creates and deletes
// under the data root. A leading dot keeps it out of the way of per-user
// directories (userIDs are opaque but historically numeric/UUID; the dot prefix
// avoids any realistic collision and the file is removed immediately).
const probeFileName = ".blowball-shared-probe"

// CheckOptions configures the shared-backend startup health check.
type CheckOptions struct {
	// DataDir is the per-user data root ({data-dir}/data) that must be the
	// shared POSIX filesystem mount point. The check writes a throwaway probe
	// file here and inspects the filesystem type of this path.
	DataDir string
	// Log receives progress/warning lines (e.g. the non-Linux fstype skip).
	Log *zap.Logger
}

// CheckSharedBackend performs the startup health check for storage.workspace.
// backend == "shared". It verifies the data root is writable and, on Linux,
// that it sits on a FUSE-family filesystem (JuiceFS presents as FUSE). The
// check runs for every role in shared mode because every role reads/writes the
// shared data plane (the api role does workspace CRUD; the agent role streams).
//
// On non-Linux platforms the fstype probe is unavailable, so it is skipped with
// a warning — dev machines (macOS/Windows) normally use backend: local. The
// writable probe still runs everywhere.
//
// A non-nil error means the process MUST NOT start in shared mode; callers
// should fatal-exit on it to avoid silent cross-node divergence.
func CheckSharedBackend(opts CheckOptions) error {
	if opts.DataDir == "" {
		return fmt.Errorf("shared storage backend: data dir is empty")
	}

	if err := writableProbe(opts.DataDir); err != nil {
		return fmt.Errorf("shared storage backend: data dir %q is not writable: %w "+
			"(ensure the shared filesystem is mounted and writable by this process)", opts.DataDir, err)
	}

	kind, magic, err := sharedFSType(opts.DataDir)
	if err != nil {
		return fmt.Errorf("shared storage backend: cannot determine filesystem type of %q: %w", opts.DataDir, err)
	}
	if kind == fstypeUnsupported {
		// Non-Linux platform: cannot inspect fstype. Warn and continue; the
		// writable probe above is the only platform-portable guard here.
		if opts.Log != nil {
			opts.Log.Warn("shared storage backend: filesystem-type health check is Linux-only; skipping on this platform",
				zap.String("data_dir", opts.DataDir))
		}
		return nil
	}
	if kind != fstypeFUSE {
		return fmt.Errorf("shared storage backend: %q is not on a shared POSIX filesystem "+
			"(fstype magic=0x%x); the operator must mount JuiceFS onto %q BEFORE starting blowball, "+
			"otherwise this node silently writes local disk and diverges from the cluster",
			opts.DataDir, magic, opts.DataDir)
	}
	return nil
}

// writableProbe confirms the data root is writable by creating and removing a
// throwaway file. It is portable across platforms and catches the "mounted
// read-only / wrong permissions" failure mode that the fstype check alone would
// miss.
func writableProbe(dir string) error {
	probe := filepath.Join(dir, probeFileName)
	if err := os.WriteFile(probe, []byte("shared-storage-probe\n"), 0o644); err != nil {
		return err
	}
	// Best-effort cleanup; a leftover probe file is harmless (dot-prefixed, in
	// the data root) but we remove it so the check is side-effect-free.
	if rmErr := os.Remove(probe); rmErr != nil && !os.IsNotExist(rmErr) {
		return rmErr
	}
	return nil
}
