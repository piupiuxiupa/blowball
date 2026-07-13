package xizhi

// ApplyLandlock restricts the current process: read+write access under each
// directory in rwDirs and read-only access under each directory in roDirs. On
// Linux this applies the go-landlock V2 restriction; on other platforms (e.g.
// macOS dev) it is a no-op that logs a warning. The function is best-effort by
// design so dev workflows on non-Linux machines are not broken.
//
// D5/D6 decisions: the caller passes the specific runtime subdirectories we
// touch rather than the whole runtime root. rwDirs holds the directories the
// process must write to — {data-dir}/data, {data-dir}/logs, {data-dir}/skills —
// keeping the sandbox tight while still covering the logs directory so
// lumberjack's post-rotation reopen (which happens after this call) stays
// inside the sandbox. roDirs holds operator-supplied static content —
// {data-dir}/tools — that must remain readable but never mutated, mirroring the
// in-sandbox --ro-bind as defense-in-depth. go-landlock's RODirs/RWDirs are
// variadic, so all paths are restricted in a single RestrictPaths call.
func ApplyLandlock(rwDirs, roDirs []string) error {
	return applyLandlock(rwDirs, roDirs)
}
