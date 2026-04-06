package profiler

import (
	"encoding/json"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/shirou/gopsutil/v3/cpu"
	"github.com/shirou/gopsutil/v3/mem"
)

// ClassAd represents a resource advertisement for a worker node.
type ClassAd struct {
	Timestamp      int64  `json:"timestamp"`
	OS             string `json:"os"`
	Architecture   string `json:"architecture"`
	CPUCores       int    `json:"cpu_cores"`
	CPUModel       string `json:"cpu_model"`
	TotalMemoryMB  uint64 `json:"total_memory_mb"`
	AvailableMemMB uint64 `json:"available_mem_mb"`
	HostType       string `json:"host_type"`

	// GPU fields
	GPUCount    int      `json:"gpu_count"`
	GPUVendor   string   `json:"gpu_vendor"`    // "nvidia", "amd", or ""
	GPUModels   []string `json:"gpu_models"`
	GPUMemoryMB []int    `json:"gpu_memory_mb"`
	CUDAVersion string   `json:"cuda_version"`
	ROCmVersion string   `json:"rocm_version"`
}

// GenerateClassAd detects current machine statistics and returns a JSON advertisement payload.
func GenerateClassAd() (string, error) {
	v, err := mem.VirtualMemory()
	if err != nil {
		return "", err
	}

	cpuInfo, err := cpu.Info()
	if err != nil {
		return "", err
	}

	cores, _ := cpu.Counts(true)

	modelName := "Unknown"
	if len(cpuInfo) > 0 {
		modelName = cpuInfo[0].ModelName
	}

	gpuCount, gpuModels, gpuMemory, gpuVersion, gpuVendor := DetectGPUs()

	ad := ClassAd{
		Timestamp:      time.Now().Unix(),
		OS:             runtime.GOOS,
		Architecture:   runtime.GOARCH,
		CPUCores:       cores,
		CPUModel:       modelName,
		TotalMemoryMB:  v.Total / 1024 / 1024,
		AvailableMemMB: v.Available / 1024 / 1024,
		HostType:       detectHostType(),
		GPUCount:       gpuCount,
		GPUVendor:      gpuVendor,
		GPUModels:      gpuModels,
		GPUMemoryMB:    gpuMemory,
	}

	if gpuVendor == "nvidia" {
		ad.CUDAVersion = gpuVersion
	} else if gpuVendor == "amd" {
		ad.ROCmVersion = gpuVersion
	}

	b, err := json.Marshal(ad)
	if err != nil {
		return "", err
	}

	return string(b), nil
}

// detectHostType determines if running in a container or bare metal.
func detectHostType() string {
	if _, err := os.Stat("/.dockerenv"); err == nil {
		return "container"
	}
	return "vm_or_baremetal"
}

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

	// Get GPU names: rocm-smi --showproductname --csv
	cmd := exec.Command(smiPath, "--showproductname", "--csv")
	out, err := cmd.Output()
	if err != nil {
		// Fallback: try rocm-smi --showallinfo and parse
		return detectAMDGPUsFallback(smiPath)
	}

	// Parse CSV: skip header line, each row has device,card_series,card_model,card_vendor,...
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	for i, line := range lines {
		if i == 0 {
			continue // skip header
		}
		fields := strings.Split(line, ",")
		if len(fields) >= 2 {
			name := strings.TrimSpace(fields[1]) // card_series
			if name == "" && len(fields) >= 3 {
				name = strings.TrimSpace(fields[2]) // card_model
			}
			if name != "" {
				models = append(models, name)
			}
		}
	}
	count = len(models)

	// Get VRAM: rocm-smi --showmeminfo vram --csv
	cmd2 := exec.Command(smiPath, "--showmeminfo", "vram", "--csv")
	out2, err := cmd2.Output()
	if err == nil {
		lines2 := strings.Split(strings.TrimSpace(string(out2)), "\n")
		for i, line := range lines2 {
			if i == 0 {
				continue // skip header
			}
			// Format: device, vram_total, vram_used
			fields := strings.Split(line, ",")
			if len(fields) >= 2 {
				totalBytes, _ := strconv.ParseInt(strings.TrimSpace(fields[1]), 10, 64)
				memoryMB = append(memoryMB, int(totalBytes/1024/1024))
			}
		}
	}

	// Get ROCm version
	cmd3 := exec.Command(smiPath, "--showdriverversion")
	out3, err := cmd3.Output()
	if err == nil {
		re := regexp.MustCompile(`(?i)driver version:\s+([\d.]+)`)
		if matches := re.FindSubmatch(out3); len(matches) > 1 {
			rocmVersion = string(matches[1])
		}
	}

	// If we got models but no VRAM data, pad with zeros
	for len(memoryMB) < count {
		memoryMB = append(memoryMB, 0)
	}

	return count, models, memoryMB, rocmVersion
}

// detectAMDGPUsFallback uses rocm-smi without --csv flags (older versions).
func detectAMDGPUsFallback(smiPath string) (count int, models []string, memoryMB []int, rocmVersion string) {
	cmd := exec.Command(smiPath)
	out, err := cmd.Output()
	if err != nil {
		return 0, nil, nil, ""
	}

	// Count GPU lines: rocm-smi default output lists GPUs as "GPU[0]", "GPU[1]", etc.
	lines := strings.Split(string(out), "\n")
	for _, line := range lines {
		if strings.Contains(line, "GPU[") || strings.Contains(strings.ToLower(line), "gpu") && strings.Contains(line, ":") {
			count++
		}
	}

	// Minimal fallback: count GPUs, no model/memory info
	for i := 0; i < count; i++ {
		models = append(models, "AMD GPU")
		memoryMB = append(memoryMB, 0)
	}

	return count, models, memoryMB, ""
}
