package profiler

import (
	"encoding/json"
	"fmt"
	"os"
	"runtime"
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
	GPUVendor   string   `json:"gpu_vendor"` // "nvidia", "amd", "intel", "apple", "qualcomm", or ""
	GPUModels   []string `json:"gpu_models"`
	GPUMemoryMB []int    `json:"gpu_memory_mb"`
	CUDAVersion string   `json:"cuda_version"`
	ROCmVersion string   `json:"rocm_version"`

	// Live GPU state, as aggregates rather than per-device values: the ad is capped at 512
	// bytes, and a scheduling requirement is almost always "is there a device with room" rather
	// than a statement about a particular card. GPUFreeMB is the least free memory across
	// devices and GPUUtilPct the busiest device's utilisation, so a requirement written against
	// them holds for at least one device and no more than it should.
	GPUFreeMB   int  `json:"gpu_free_mb,omitempty"`
	GPUUtilPct  int  `json:"gpu_util_pct,omitempty"`
	GPUMIGSplit bool `json:"gpu_mig,omitempty"`
}

// DetectGPUs reports the accelerators on this node in the flattened form most callers want.
//
// DetectGPUDetail is the real detector; this is the adapter kept for callers that only need the
// inventory and not the live state.
func DetectGPUs() (count int, models []string, memoryMB []int, version string, vendor string) {
	inv, vendor := DetectGPUDetail()
	return inv.Count(), inv.Models, inv.MemoryMB, inv.Version, vendor
}

// MaxMetaBytes is the hard ceiling on a serialized ClassAd.
//
// memberlist attaches the ClassAd to every node as gossip metadata and panics outright if it
// exceeds MetaMaxSize (512 bytes). A realistic 8-GPU node serializes to ~553 bytes, so the
// ad must be actively kept under the limit rather than assumed to fit.
const MaxMetaBytes = 512

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

	gpu, gpuVendor := DetectGPUDetail()
	gpuCount, gpuModels, gpuMemory, gpuVersion := gpu.Count(), gpu.Models, gpu.MemoryMB, gpu.Version

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
		GPUFreeMB:      gpu.FreeMB,
		GPUUtilPct:     gpu.UtilPct,
		GPUMIGSplit:    gpu.MIGEnabled,
	}

	switch gpuVendor {
	case "nvidia":
		ad.CUDAVersion = gpuVersion
	case "amd":
		ad.ROCmVersion = gpuVersion
	}

	b, err := fitClassAd(ad, MaxMetaBytes)
	if err != nil {
		return "", err
	}

	return string(b), nil
}

// fitClassAd serializes ad, shrinking it if necessary so the result stays within limit.
//
// The reduction ladder drops the least load-bearing content first. Every field named in the
// documented CEL surface (gpu_count, gpu_vendor, gpu_memory_mb, cuda_version, rocm_version,
// cpu_cores, total_memory_mb, os, architecture, host_type) survives every step, so shrinking
// never changes whether a job matches a node. Only the descriptive extras — cpu_model and
// gpu_models, neither of which is addressable from CEL — are sacrificed, and repeated GPU
// entries collapse to their distinct values, which leaves index 0 intact for the documented
// `ad.gpu_memory_mb[0] >= N` idiom on the homogeneous nodes that idiom is written for.
func fitClassAd(ad ClassAd, limit int) ([]byte, error) {
	steps := []func(*ClassAd){
		func(a *ClassAd) {},                                             // as detected
		func(a *ClassAd) { a.CPUModel = "" },                            // drop CPU model
		func(a *ClassAd) { a.GPUModels = distinctStrings(a.GPUModels) }, // collapse GPU models
		func(a *ClassAd) { a.GPUModels = nil },                          // drop GPU models
		func(a *ClassAd) { a.GPUMemoryMB = distinctInts(a.GPUMemoryMB) },
		func(a *ClassAd) { a.GPUMemoryMB = nil },
	}

	var b []byte
	var err error
	shrunk := ad
	for _, step := range steps {
		step(&shrunk)
		b, err = json.Marshal(shrunk)
		if err != nil {
			return nil, err
		}
		if len(b) <= limit {
			return b, nil
		}
	}

	// Nothing optional remains. Report the oversize ad rather than emitting a document that
	// would panic the gossip layer; the caller decides how to fail.
	return nil, fmt.Errorf("class ad is %d bytes, over the %d byte limit, with no reducible fields left", len(b), limit)
}

// distinctStrings returns s with consecutive-or-repeated duplicates removed, order preserved.
func distinctStrings(s []string) []string {
	if len(s) < 2 {
		return s
	}
	seen := make(map[string]bool, len(s))
	out := make([]string, 0, len(s))
	for _, v := range s {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}

// distinctInts returns s with duplicates removed, order preserved.
func distinctInts(s []int) []int {
	if len(s) < 2 {
		return s
	}
	seen := make(map[int]bool, len(s))
	out := make([]int, 0, len(s))
	for _, v := range s {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}

// detectHostType determines if running in a container or bare metal.
func detectHostType() string {
	if _, err := os.Stat("/.dockerenv"); err == nil {
		return "container"
	}
	return "vm_or_baremetal"
}
