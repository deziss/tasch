//go:build darwin

package profiler

import (
	"os/exec"
	"strconv"
	"strings"
)

// DetectGPUs detects Apple Silicon / AMD / Intel GPUs on macOS.
func DetectGPUs() (count int, models []string, memoryMB []int, version string, vendor string) {
	cmd := exec.Command("system_profiler", "SPDisplaysDataType")
	out, err := cmd.Output()
	if err != nil {
		return 0, nil, nil, "", ""
	}

	lines := strings.Split(string(out), "\n")
	model := "Apple GPU"
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "Chipset Model:") {
			model = strings.TrimSpace(strings.TrimPrefix(line, "Chipset Model:"))
		}
	}

	var vramMB int
	cmd2 := exec.Command("sysctl", "-n", "hw.memsize")
	out2, err := cmd2.Output()
	if err == nil {
		bytes, _ := strconv.ParseInt(strings.TrimSpace(string(out2)), 10, 64)
		if bytes > 0 {
			// For Unified Memory, allocate 75% of total memory as available VRAM
			vramMB = int((bytes / 1024 / 1024) * 3 / 4)
		}
	}
	if vramMB == 0 {
		vramMB = 8192 // Fallback to 8GB
	}

	models = []string{model}
	memoryMB = []int{vramMB}
	count = 1
	version = "Metal API"

	lowerModel := strings.ToLower(model)
	if strings.Contains(lowerModel, "nvidia") {
		vendor = "nvidia"
	} else if strings.Contains(lowerModel, "amd") || strings.Contains(lowerModel, "radeon") {
		vendor = "amd"
	} else if strings.Contains(lowerModel, "intel") {
		vendor = "intel"
	} else {
		vendor = "apple"
	}

	return count, models, memoryMB, version, vendor
}
