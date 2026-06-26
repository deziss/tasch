//go:build !linux && !windows && !darwin

package profiler

// DetectGPUs is a stub for unsupported operating systems to ensure compilation succeeds.
func DetectGPUs() (count int, models []string, memoryMB []int, version string, vendor string) {
	return 0, nil, nil, "", ""
}
