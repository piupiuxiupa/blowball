package xizhi

// ApplyLandlock restricts the current process to read+write access under each
// of the given directories. On Linux this applies the go-landlock V2
// restriction; on other platforms (e.g. macOS dev) it is a no-op that logs a
// warning. The function is best-effort by design so dev workflows on non-Linux
// machines are not broken.
//
// D6 decision: the caller passes the specific runtime subdirectories we touch
// — {data-dir}/data, {data-dir}/logs, {data-dir}/skills — rather than the whole
// runtime root, keeping the sandbox as tight as today while still covering the
// logs directory so lumberjack's post-rotation reopen (which happens after this
// call) stays inside the sandbox. go-landlock's RWDirs is variadic, so all three
// subdirs are restricted in a single RestrictPaths call.
func ApplyLandlock(dirs ...string) error {
	return applyLandlock(dirs)
}
