package profiler

import (
	"strconv"
	"strings"
)

// macGPU is one display adapter reported by system_profiler.
type macGPU struct {
	Model string
	// VRAMMegabytes is 0 when the adapter has no dedicated VRAM, which is the normal case on
	// Apple Silicon: the GPU shares system memory rather than owning any.
	VRAMMegabytes int
	Unified       bool
}

// parseMacGPUs reads `system_profiler SPDisplaysDataType` output.
//
// The previous implementation kept only the last "Chipset Model" line it saw, and reported a
// GPU count of 1 whenever the command merely succeeded — so a headless Mac or a VM with no
// graphics hardware still advertised a GPU and attracted GPU jobs it could not run.
func parseMacGPUs(raw string) []macGPU {
	var gpus []macGPU
	var current *macGPU

	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)

		switch {
		case strings.HasPrefix(line, "Chipset Model:"):
			// A new adapter begins. Close out the previous one.
			if current != nil {
				gpus = append(gpus, *current)
			}
			model := strings.TrimSpace(strings.TrimPrefix(line, "Chipset Model:"))
			if model == "" {
				current = nil
				continue
			}
			current = &macGPU{Model: model}

		case current == nil:
			// Text outside any adapter block.
			continue

		case strings.HasPrefix(line, "VRAM (Total):"),
			strings.HasPrefix(line, "VRAM (Dynamic, Max):"):
			value := line[strings.Index(line, ":")+1:]
			current.VRAMMegabytes = parseAppleMemorySize(value)

		case strings.HasPrefix(line, "Vendor:"):
			if strings.Contains(strings.ToLower(line), "apple") {
				current.Unified = true
			}
		}
	}
	if current != nil {
		gpus = append(gpus, *current)
	}
	return gpus
}

// parseAppleMemorySize reads sizes like "4 GB" or "1536 MB" into megabytes.
func parseAppleMemorySize(value string) int {
	fields := strings.Fields(strings.TrimSpace(value))
	if len(fields) == 0 {
		return 0
	}
	amount, err := strconv.ParseFloat(fields[0], 64)
	if err != nil || amount <= 0 {
		return 0
	}
	unit := ""
	if len(fields) > 1 {
		unit = strings.ToUpper(fields[1])
	}
	switch unit {
	case "GB":
		return int(amount * 1024)
	case "TB":
		return int(amount * 1024 * 1024)
	default: // MB, or unlabelled
		return int(amount)
	}
}

// unifiedMemoryShare is the fraction of system memory reported as available to an Apple Silicon
// GPU. The hardware imposes no fixed split — CPU and GPU draw from the same pool — so this is a
// convention for matchmaking, not a measurement, and deliberately leaves headroom for the host.
const unifiedMemoryShare = 0.75

// unifiedMemoryMB converts a system's total memory into the figure advertised for a
// unified-memory GPU.
//
// Zero in, zero out: when the memory size cannot be read, the node advertises no GPU memory
// rather than a fabricated one. The previous code fell back to a hardcoded 8 GB, which would let
// a job match a machine that could not actually hold it.
func unifiedMemoryMB(totalBytes int64) int {
	if totalBytes <= 0 {
		return 0
	}
	return int(float64(totalBytes/1024/1024) * unifiedMemoryShare)
}

// vendorFromMacModel classifies a macOS adapter.
func vendorFromMacModel(model string) string {
	lower := strings.ToLower(model)
	switch {
	case strings.Contains(lower, "nvidia"), strings.Contains(lower, "geforce"):
		return "nvidia"
	case strings.Contains(lower, "amd"), strings.Contains(lower, "radeon"):
		return "amd"
	case strings.Contains(lower, "intel"):
		return "intel"
	default:
		return "apple"
	}
}
