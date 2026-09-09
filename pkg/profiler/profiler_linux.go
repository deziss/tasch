//go:build linux

package profiler

import (
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
)

// DetectGPUDetail reports the accelerators on this node, and which vendor supplied them.
func DetectGPUDetail() (GPUInventory, string) {
	if inv, ok := detectNVIDIAGPUs(); ok {
		return inv, "nvidia"
	}
	if inv, ok := detectAMDGPUs(); ok {
		return inv, "amd"
	}
	// ARM64 Jetson boards carry an integrated GPU that nvidia-smi does not report.
	if inv, ok := detectJetsonGPUS(); ok {
		return inv, "nvidia"
	}
	return GPUInventory{}, ""
}

// detectNVIDIAGPUs reads the driver's own inventory through nvidia-smi.
//
// The XML form is asked for first because it is an interface: it carries the CUDA version,
// per-device free memory and utilisation, and MIG instances, and it either parses or reports an
// error. The CSV query is kept as a fallback for drivers too old to support `-q -x`, and it
// still beats the previous approach of scraping the human-readable banner with a regular
// expression — that banner is layout, it has changed between releases, and a miss produced an
// empty CUDA version silently.
func detectNVIDIAGPUs() (GPUInventory, bool) {
	smiPath, err := exec.LookPath("nvidia-smi")
	if err != nil {
		return GPUInventory{}, false
	}

	if out, err := probe(smiPath, "-q", "-x"); err == nil {
		if inv, err := parseNvidiaSMIXML(out); err == nil {
			return inv, true
		}
	}
	return detectNVIDIAGPUsCSV(smiPath)
}

// detectNVIDIAGPUsCSV is the fallback for drivers without XML output.
func detectNVIDIAGPUsCSV(smiPath string) (GPUInventory, bool) {
	out, err := probe(smiPath,
		"--query-gpu=name,memory.total,memory.free,utilization.gpu",
		"--format=csv,noheader,nounits")
	if err != nil {
		return GPUInventory{}, false
	}

	var inv GPUInventory
	freeMB := -1
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.Split(line, ",")
		if len(fields) < 2 {
			continue
		}
		inv.Models = append(inv.Models, strings.TrimSpace(fields[0]))
		mem, _ := strconv.Atoi(strings.TrimSpace(fields[1]))
		inv.MemoryMB = append(inv.MemoryMB, mem)

		if len(fields) >= 3 {
			if free, err := strconv.Atoi(strings.TrimSpace(fields[2])); err == nil {
				if freeMB < 0 || free < freeMB {
					freeMB = free
				}
			}
		}
		if len(fields) >= 4 {
			if util, err := strconv.Atoi(strings.TrimSpace(fields[3])); err == nil && util > inv.UtilPct {
				inv.UtilPct = util
			}
		}
	}
	if len(inv.Models) == 0 {
		return GPUInventory{}, false
	}
	if freeMB >= 0 {
		inv.FreeMB = freeMB
	}

	// The CUDA version has no --query-gpu field, so this path simply does not report it rather
	// than going back to scraping the banner for it.
	return inv, true
}

// detectAMDGPUs queries rocm-smi for AMD GPU information.
func detectAMDGPUs() (GPUInventory, bool) {
	var inv GPUInventory
	smiPath, err := exec.LookPath("rocm-smi")
	if err != nil {
		return inv, false
	}

	out, err := probe(smiPath, "--showproductname", "--csv")
	if err != nil {
		return detectAMDGPUsFallback(smiPath)
	}

	var models []string
	var memoryMB []int
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
	count := len(models)

	out2, err := probe(smiPath, "--showmeminfo", "vram", "--csv")
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

	out3, err := probe(smiPath, "--showdriverversion")
	if err == nil {
		re := regexp.MustCompile(`(?i)driver version:\s+([\d.]+)`)
		if matches := re.FindSubmatch(out3); len(matches) > 1 {
			inv.Version = string(matches[1])
		}
	}

	for len(memoryMB) < count {
		memoryMB = append(memoryMB, 0)
	}

	inv.Models = models
	inv.MemoryMB = memoryMB
	return inv, count > 0
}

// detectAMDGPUsFallback uses rocm-smi without --csv flags.
func detectAMDGPUsFallback(smiPath string) (GPUInventory, bool) {
	var inv GPUInventory
	out, err := probe(smiPath)
	if err != nil {
		return inv, false
	}

	count := 0
	lines := strings.Split(string(out), "\n")
	for _, line := range lines {
		if strings.Contains(line, "GPU[") || strings.Contains(strings.ToLower(line), "gpu") && strings.Contains(line, ":") {
			count++
		}
	}

	for i := 0; i < count; i++ {
		inv.Models = append(inv.Models, "AMD GPU")
		inv.MemoryMB = append(inv.MemoryMB, 0)
	}

	return inv, count > 0
}

// detectJetsonGPUS queries tegrastats or sysfs for Nvidia Jetson embedded hardware.
func detectJetsonGPUS() (GPUInventory, bool) {
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

		return GPUInventory{
			Models:   []string{modelName},
			MemoryMB: []int{vramMB},
			Version:  "Jetson Unified",
		}, true
	}
	return GPUInventory{}, false
}
