//go:build darwin

package profiler

import (
	"strconv"
	"strings"
)

// DetectGPUs detects Apple Silicon / AMD / Intel GPUs on macOS.
// DetectGPUDetail reports the accelerators on this node, and which vendor supplied them.
//
// Live utilisation and free memory are Linux-only for now: they come from the NVIDIA driver's
// structured output, which is where the scheduler's GPU pressure information originates.
func DetectGPUDetail() (GPUInventory, string) {
	_, models, memoryMB, version, vendor := detectGPUsPlatform()
	return GPUInventory{Models: models, MemoryMB: memoryMB, Version: version}, vendor
}

func detectGPUsPlatform() (count int, models []string, memoryMB []int, version string, vendor string) {
	out, err := probe("system_profiler", "SPDisplaysDataType")
	if err != nil {
		return 0, nil, nil, "", ""
	}

	gpus := parseMacGPUs(string(out))
	if len(gpus) == 0 {
		// A headless Mac or a VM with no graphics hardware. Reporting a GPU here would attract
		// GPU jobs the node cannot run, which is what the previous unconditional count of 1 did.
		return 0, nil, nil, "", ""
	}

	// Apple Silicon shares system memory with the CPU, so system_profiler reports no dedicated
	// VRAM. Derive a figure from installed memory for those; a discrete card's own number is
	// used as reported.
	var unifiedMB int
	if sysOut, err := probe("sysctl", "-n", "hw.memsize"); err == nil {
		if bytes, parseErr := strconv.ParseInt(strings.TrimSpace(string(sysOut)), 10, 64); parseErr == nil {
			unifiedMB = unifiedMemoryMB(bytes)
		}
	}

	for _, gpu := range gpus {
		models = append(models, gpu.Model)

		mb := gpu.VRAMMegabytes
		if mb == 0 {
			// No dedicated VRAM. Fall back to the unified-memory share, and to zero when the
			// system's memory size is unknown — a fabricated figure would let a job match a node
			// that cannot hold it.
			mb = unifiedMB
		}
		memoryMB = append(memoryMB, mb)
	}

	count = len(models)
	vendor = vendorFromMacModel(models[0])
	version = "Metal API"

	return count, models, memoryMB, version, vendor
}
