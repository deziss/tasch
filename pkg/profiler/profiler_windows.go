//go:build windows

package profiler

import (
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
)

// windowsGPUQuery lists video controllers with both memory figures.
//
// Win32_VideoController.AdapterRAM is 32-bit, so it cannot represent 4 GB or more and comes back
// wrapped for any modern card. The driver's own 64-bit size lives in the registry under the
// display class key, matched to the controller by its driver description, so both are collected
// and the reliable one is preferred.
const windowsGPUQuery = `
$ErrorActionPreference = 'SilentlyContinue'
$reg = Get-ItemProperty 'HKLM:\SYSTEM\CurrentControlSet\Control\Class\{4d36e968-e325-11ce-bfc1-08002be10318}\*'
Get-CimInstance Win32_VideoController | ForEach-Object {
  $gpu = $_
  $entry = $reg | Where-Object { $_.DriverDesc -eq $gpu.Name } | Select-Object -First 1
  [PSCustomObject]@{
    Name         = $gpu.Name
    AdapterRAM   = $gpu.AdapterRAM
    QwMemorySize = $entry.'HardwareInformation.qwMemorySize'
  }
} | ConvertTo-Json`

// DetectGPUs tries to identify any GPUs present on Windows.
func DetectGPUs() (count int, models []string, memoryMB []int, version string, vendor string) {
	// 1. Try WMI query via PowerShell first to capture all GPU types (NVIDIA, AMD, Intel, Qualcomm)
	out, err := probe("powershell", "-NoProfile", "-NonInteractive", "-Command", windowsGPUQuery)
	if err == nil {
		models, memoryMB = parseWindowsGPUs(out)
		if len(models) > 0 {
			count = len(models)
			vendor = vendorFromModel(models[0])
			if vendor == "nvidia" {
				version = getWindowsCUDAVersion()
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

	out, err := probe(smiPath, "--query-gpu=name,memory.total", "--format=csv,noheader,nounits")
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

	out2, err := probe(smiPath)
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
	out, err := probe(smiPath)
	if err == nil {
		re := regexp.MustCompile(`CUDA Version:\s+([\d.]+)`)
		if matches := re.FindSubmatch(out); len(matches) > 1 {
			return string(matches[1])
		}
	}
	return ""
}
