//go:build !linux && !windows && !darwin

package profiler

// DetectGPUs is a stub for unsupported operating systems to ensure compilation succeeds.
// DetectGPUDetail reports the accelerators on this node, and which vendor supplied them.
//
// Live utilisation and free memory are Linux-only for now: they come from the NVIDIA driver's
// structured output, which is where the scheduler's GPU pressure information originates.
func DetectGPUDetail() (GPUInventory, string) {
	_, models, memoryMB, version, vendor := detectGPUsPlatform()
	return GPUInventory{Models: models, MemoryMB: memoryMB, Version: version}, vendor
}

func detectGPUsPlatform() (count int, models []string, memoryMB []int, version string, vendor string) {
	return 0, nil, nil, "", ""
}
