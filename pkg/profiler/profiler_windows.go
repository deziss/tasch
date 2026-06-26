//go:build windows

package profiler

import (
	"encoding/json"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"syscall"
)

type winGPU struct {
	Name       string      `json:"Name"`
	AdapterRAM interface{} `json:"AdapterRAM"`
}

// DetectGPUs tries to identify any GPUs present on Windows.
func DetectGPUs() (count int, models []string, memoryMB []int, version string, vendor string) {
	// 1. Try WMI query via PowerShell first to capture all GPU types (NVIDIA, AMD, Intel, Qualcomm)
	cmd := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command",
		"Get-CimInstance Win32_VideoController | Select-Object Name, AdapterRAM | ConvertTo-Json")
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}

	out, err := cmd.Output()
	if err == nil {
		var gpus []winGPU
		trimmed := strings.TrimSpace(string(out))
		if trimmed != "" {
			if strings.HasPrefix(trimmed, "{") {
				var single winGPU
				if json.Unmarshal([]byte(trimmed), &single) == nil {
					gpus = append(gpus, single)
				}
			} else if strings.HasPrefix(trimmed, "[") {
				json.Unmarshal([]byte(trimmed), &gpus)
			}
		}

		for _, gpu := range gpus {
			name := strings.TrimSpace(gpu.Name)
			lowerName := strings.ToLower(name)
			// Skip virtual / remote display adapters
			if strings.Contains(lowerName, "basic display") || strings.Contains(lowerName, "virtual") || strings.Contains(lowerName, "remote") || strings.Contains(lowerName, "citrix") {
				continue
			}

			models = append(models, name)

			var ramBytes int64
			switch v := gpu.AdapterRAM.(type) {
			case float64:
				ramBytes = int64(v)
			case string:
				ramBytes, _ = strconv.ParseInt(v, 10, 64)
			}
			ramMB := int(ramBytes / 1024 / 1024)
			if ramMB < 0 {
				ramMB = 0
			}
			memoryMB = append(memoryMB, ramMB)
		}

		if len(models) > 0 {
			count = len(models)
			firstLower := strings.ToLower(models[0])
			if strings.Contains(firstLower, "nvidia") {
				vendor = "nvidia"
				version = getWindowsCUDAVersion()
			} else if strings.Contains(firstLower, "amd") || strings.Contains(firstLower, "radeon") {
				vendor = "amd"
			} else if strings.Contains(firstLower, "intel") {
				vendor = "intel"
			} else if strings.Contains(firstLower, "adreno") || strings.Contains(firstLower, "qualcomm") {
				vendor = "qualcomm"
			} else {
				vendor = "generic"
			}
			return count, models, memoryMB, version, vendor
		}
	}

	// 2. Fallback to nvidia-smi directly
	count, models, memoryMB, version = detectWindowsNVIDIAGPUs()
	if count > 0 {
		return count, models, memoryMB, version, "nvidia"
	}

	return 0, nil, nil, "", ""
}

func detectWindowsNVIDIAGPUs() (count int, models []string, memoryMB []int, cudaVersion string) {
	smiPath := "nvidia-smi"
	if _, err := exec.LookPath(smiPath); err != nil {
		smiPath = `C:\Program Files\NVIDIA Corporation\NVSMI\nvidia-smi.exe`
		if _, err := os.Stat(smiPath); err != nil {
			return 0, nil, nil, ""
		}
	}

	cmd := exec.Command(smiPath, "--query-gpu=name,memory.total", "--format=csv,noheader,nounits")
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
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
	cmd2.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	out2, err := cmd2.Output()
	if err == nil {
		re := regexp.MustCompile(`CUDA Version:\s+([\d.]+)`)
		if matches := re.FindSubmatch(out2); len(matches) > 1 {
			cudaVersion = string(matches[1])
		}
	}

	return count, models, memoryMB, cudaVersion
}

func getWindowsCUDAVersion() string {
	smiPath := "nvidia-smi"
	if _, err := exec.LookPath(smiPath); err != nil {
		smiPath = `C:\Program Files\NVIDIA Corporation\NVSMI\nvidia-smi.exe`
		if _, err := os.Stat(smiPath); err != nil {
			return ""
		}
	}
	cmd := exec.Command(smiPath)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	out, err := cmd.Output()
	if err == nil {
		re := regexp.MustCompile(`CUDA Version:\s+([\d.]+)`)
		if matches := re.FindSubmatch(out); len(matches) > 1 {
			return string(matches[1])
		}
	}
	return ""
}
