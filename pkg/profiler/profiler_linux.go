//go:build linux

package profiler

import (
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
)

// DetectGPUs tries NVIDIA first, then AMD. Returns zero-values if no GPU found.
func DetectGPUs() (count int, models []string, memoryMB []int, version string, vendor string) {
	count, models, memoryMB, version = detectNVIDIAGPUs()
	if count > 0 {
		return count, models, memoryMB, version, "nvidia"
	}

	count, models, memoryMB, version = detectAMDGPUs()
	if count > 0 {
		return count, models, memoryMB, version, "amd"
	}

	// Fallback for ARM64 Jetson devices on Linux
	count, models, memoryMB, version = detectJetsonGPUS()
	if count > 0 {
		return count, models, memoryMB, version, "nvidia"
	}

	return 0, nil, nil, "", ""
}

// detectNVIDIAGPUs queries nvidia-smi for GPU information.
func detectNVIDIAGPUs() (count int, models []string, memoryMB []int, cudaVersion string) {
	smiPath, err := exec.LookPath("nvidia-smi")
	if err != nil {
		return 0, nil, nil, ""
	}

	cmd := exec.Command(smiPath, "--query-gpu=name,memory.total", "--format=csv,noheader,nounits")
	out, err := cmd.Output()
	if err != nil {
		return 0, nil, nil, ""
	}

	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, ", ", 2)
		if len(parts) == 2 {
			models = append(models, strings.TrimSpace(parts[0]))
			mem, _ := strconv.Atoi(strings.TrimSpace(parts[1]))
			memoryMB = append(memoryMB, mem)
		}
	}
	count = len(models)

	cmd2 := exec.Command(smiPath)
	out2, err := cmd2.Output()
	if err == nil {
		re := regexp.MustCompile(`CUDA Version:\s+([\d.]+)`)
		if matches := re.FindSubmatch(out2); len(matches) > 1 {
			cudaVersion = string(matches[1])
		}
	}

	return count, models, memoryMB, cudaVersion
}

// detectAMDGPUs queries rocm-smi for AMD GPU information.
func detectAMDGPUs() (count int, models []string, memoryMB []int, rocmVersion string) {
	smiPath, err := exec.LookPath("rocm-smi")
	if err != nil {
		return 0, nil, nil, ""
	}

	cmd := exec.Command(smiPath, "--showproductname", "--csv")
	out, err := cmd.Output()
	if err != nil {
		return detectAMDGPUsFallback(smiPath)
	}

	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	for i, line := range lines {
		if i == 0 {
			continue // skip header
		}
		fields := strings.Split(line, ",")
		if len(fields) >= 2 {
			name := strings.TrimSpace(fields[1])
			if name == "" && len(fields) >= 3 {
				name = strings.TrimSpace(fields[2])
			}
			if name != "" {
				models = append(models, name)
			}
		}
	}
	count = len(models)

	cmd2 := exec.Command(smiPath, "--showmeminfo", "vram", "--csv")
	out2, err := cmd2.Output()
	if err == nil {
		lines2 := strings.Split(strings.TrimSpace(string(out2)), "\n")
		for i, line := range lines2 {
			if i == 0 {
				continue // skip header
			}
			fields := strings.Split(line, ",")
			if len(fields) >= 2 {
				totalBytes, _ := strconv.ParseInt(strings.TrimSpace(fields[1]), 10, 64)
				memoryMB = append(memoryMB, int(totalBytes/1024/1024))
			}
		}
	}

	cmd3 := exec.Command(smiPath, "--showdriverversion")
	out3, err := cmd3.Output()
	if err == nil {
		re := regexp.MustCompile(`(?i)driver version:\s+([\d.]+)`)
		if matches := re.FindSubmatch(out3); len(matches) > 1 {
			rocmVersion = string(matches[1])
		}
	}

	for len(memoryMB) < count {
		memoryMB = append(memoryMB, 0)
	}

	return count, models, memoryMB, rocmVersion
}

// detectAMDGPUsFallback uses rocm-smi without --csv flags.
func detectAMDGPUsFallback(smiPath string) (count int, models []string, memoryMB []int, rocmVersion string) {
	cmd := exec.Command(smiPath)
	out, err := cmd.Output()
	if err != nil {
		return 0, nil, nil, ""
	}

	lines := strings.Split(string(out), "\n")
	for _, line := range lines {
		if strings.Contains(line, "GPU[") || strings.Contains(strings.ToLower(line), "gpu") && strings.Contains(line, ":") {
			count++
		}
	}

	for i := 0; i < count; i++ {
		models = append(models, "AMD GPU")
		memoryMB = append(memoryMB, 0)
	}

	return count, models, memoryMB, ""
}

// detectJetsonGPUS queries tegrastats or sysfs for Nvidia Jetson embedded hardware.
func detectJetsonGPUS() (count int, models []string, memoryMB []int, version string) {
	// Look for Tegra/Jetson GPU signature in sysfs
	if _, err := os.Stat("/sys/devices/gpu.0/dma_mask"); err == nil {
		// Read system memory as unified memory size fallback
		memBytes := 0
		if data, err := os.ReadFile("/proc/meminfo"); err == nil {
			re := regexp.MustCompile(`MemTotal:\s+(\d+)`)
			if matches := re.FindSubmatch(data); len(matches) > 1 {
				kb, _ := strconv.Atoi(string(matches[1]))
				memBytes = kb * 1024
			}
		}
		// Typically Jetson uses unified memory. Allocate a portion (e.g. 4096MB) as VRAM representation.
		vramMB := 4096
		if memBytes > 0 {
			vramMB = (memBytes / 1024 / 1024) / 2 // Assume 50% system memory max for GPU
		}

		modelName := "NVIDIA Tegra Embedded GPU"
		if data, err := os.ReadFile("/sys/firmware/devicetree/base/model"); err == nil {
			modelName = strings.TrimSpace(strings.ReplaceAll(string(data), "\x00", ""))
		}

		return 1, []string{modelName}, []int{vramMB}, "Jetson Unified"
	}
	return 0, nil, nil, ""
}
