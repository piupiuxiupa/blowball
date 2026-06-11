package xizhi

// ApplyLandlock restricts the current process to read+write access under
// dataDir. On Linux this applies the go-landlock V2 restriction; on other
// platforms (e.g. macOS dev) it is a no-op that logs a warning. The function
// is best-effort by design so dev workflows on non-Linux machines are not
// broken.
func ApplyLandlock(dataDir string) error {
	return applyLandlock(dataDir)
}
