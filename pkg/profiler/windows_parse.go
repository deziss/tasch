package profiler

import (
	"encoding/json"
	"strconv"
	"strings"
)

// winGPU is one entry from the Windows video-controller query.
type winGPU struct {
	Name string `json:"Name"`
	// AdapterRAM comes from WMI's Win32_VideoController, whose field is 32-bit. Anything with
	// 4 GB or more of VRAM cannot be represented and comes back wrapped — often as a negative
	// number once PowerShell renders it as JSON.
	AdapterRAM interface{} `json:"AdapterRAM"`
	// QwMemorySize is the driver's own 64-bit figure, read from the registry. It is the only
	// reliable source for a modern card, so it wins whenever it is present.
	QwMemorySize interface{} `json:"QwMemorySize"`
}

// virtualAdapterMarkers identify display adapters that are not real GPUs.
var virtualAdapterMarkers = []string{"basic display", "virtual", "remote", "citrix", "meta driver"}

// parseWindowsGPUs turns the PowerShell query's JSON into models and VRAM sizes.
//
// PowerShell's ConvertTo-Json emits a bare object rather than an array when there is exactly one
// result, so both shapes have to be accepted.
func parseWindowsGPUs(raw []byte) (models []string, memoryMB []int) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" {
		return nil, nil
	}

	var gpus []winGPU
	switch {
	case strings.HasPrefix(trimmed, "{"):
		var single winGPU
		if err := json.Unmarshal([]byte(trimmed), &single); err != nil {
			return nil, nil
		}
		gpus = append(gpus, single)
	case strings.HasPrefix(trimmed, "["):
		if err := json.Unmarshal([]byte(trimmed), &gpus); err != nil {
			return nil, nil
		}
	default:
		return nil, nil
	}

	for _, gpu := range gpus {
		name := strings.TrimSpace(gpu.Name)
		if name == "" || isVirtualAdapter(name) {
			continue
		}
		models = append(models, name)
		memoryMB = append(memoryMB, windowsVRAMMegabytes(gpu))
	}
	return models, memoryMB
}

func isVirtualAdapter(name string) bool {
	lower := strings.ToLower(name)
	for _, marker := range virtualAdapterMarkers {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

// windowsVRAMMegabytes resolves a card's VRAM, preferring the driver's 64-bit figure.
//
// Falling back to AdapterRAM, a negative value is a 32-bit wrap rather than a real quantity: the
// previous code clamped it to zero, so every GPU with 4 GB or more reported no memory at all and
// no `ad.gpu_memory_mb` requirement could ever match on Windows.
func windowsVRAMMegabytes(gpu winGPU) int {
	if qw := toInt64(gpu.QwMemorySize); qw > 0 {
		return int(qw / 1024 / 1024)
	}

	ram := toInt64(gpu.AdapterRAM)
	if ram < 0 {
		// Reinterpret the low 32 bits as unsigned.
		ram = int64(uint32(ram))
	}
	if ram <= 0 {
		return 0
	}
	return int(ram / 1024 / 1024)
}

// toInt64 accepts the several shapes PowerShell's JSON uses for a number.
func toInt64(v interface{}) int64 {
	switch t := v.(type) {
	case float64:
		return int64(t)
	case int64:
		return t
	case string:
		parsed, err := strconv.ParseInt(strings.TrimSpace(t), 10, 64)
		if err != nil {
			return 0
		}
		return parsed
	default:
		return 0
	}
}

// vendorFromModel classifies a GPU by its advertised name.
func vendorFromModel(model string) string {
	lower := strings.ToLower(model)
	switch {
	case strings.Contains(lower, "nvidia"), strings.Contains(lower, "geforce"), strings.Contains(lower, "quadro"), strings.Contains(lower, "tesla"):
		return "nvidia"
	case strings.Contains(lower, "amd"), strings.Contains(lower, "radeon"):
		return "amd"
	case strings.Contains(lower, "intel"), strings.Contains(lower, "arc"):
		return "intel"
	case strings.Contains(lower, "adreno"), strings.Contains(lower, "qualcomm"):
		return "qualcomm"
	default:
		return "generic"
	}
}
