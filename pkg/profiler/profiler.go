package profiler

import (
	"encoding/json"
	"os"
	"runtime"
	"time"

	"github.com/shirou/gopsutil/v3/cpu"
	"github.com/shirou/gopsutil/v3/mem"
)

// ClassAd represents a resource advertisement for a worker node.
type ClassAd struct {
	Timestamp      int64    `json:"timestamp"`
	OS             string   `json:"os"`
	Architecture   string   `json:"architecture"`
	CPUCores       int      `json:"cpu_cores"`
	CPUModel       string   `json:"cpu_model"`
	TotalMemoryMB  uint64   `json:"total_memory_mb"`
	AvailableMemMB uint64   `json:"available_mem_mb"`
	HostType       string   `json:"host_type"`

	// GPU fields
	GPUCount    int      `json:"gpu_count"`
	GPUVendor   string   `json:"gpu_vendor"` // "nvidia", "amd", "intel", "apple", "qualcomm", or ""
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
