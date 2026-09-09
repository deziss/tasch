package profiler

import (
	"encoding/xml"
	"fmt"
	"strconv"
	"strings"
)

// NVIDIA GPU detection through nvidia-smi's structured output.
//
// On why this is not NVML directly. NVML is a C library, so binding it means cgo, and cgo means
// giving up the property the whole project is built around: one static binary that
// cross-compiles to six targets from any machine. A cgo build needs a C toolchain per target
// and links against a driver library that will not be present on most of them. That trade is
// not worth making for hardware inventory.
//
// What matters about NVML is the data, not the entry point — and `nvidia-smi -q -x` is NVML's
// own output, serialized. Reading that gives per-device UUIDs, live memory and utilisation, MIG
// instances and the driver and CUDA versions, all in a schema that either parses or does not,
// with none of the fragility that made this worth changing.
//
// The fragility was real and specific: the CUDA version was scraped out of nvidia-smi's banner
// with a regular expression over its human-readable table. That banner is layout, not an
// interface — it has changed between driver releases, it is localised, and a miss produced an
// empty version with no error, so a node would quietly advertise no CUDA at all.

// nvidiaSMILog is the subset of `nvidia-smi -q -x` this needs. Unknown elements are ignored, so
// a newer driver adding fields cannot break parsing.
type nvidiaSMILog struct {
	XMLName       xml.Name    `xml:"nvidia_smi_log"`
	DriverVersion string      `xml:"driver_version"`
	CUDAVersion   string      `xml:"cuda_version"`
	GPUs          []nvidiaGPU `xml:"gpu"`
}

type nvidiaGPU struct {
	ProductName string `xml:"product_name"`
	UUID        string `xml:"uuid"`
	Memory      struct {
		Total string `xml:"total"`
		Free  string `xml:"free"`
	} `xml:"fb_memory_usage"`
	Utilization struct {
		GPUUtil string `xml:"gpu_util"`
	} `xml:"utilization"`
	MIGMode struct {
		Current string `xml:"current_mig"`
	} `xml:"mig_mode"`
	MIGDevices []nvidiaMIGDevice `xml:"mig_devices>mig_device"`
}

type nvidiaMIGDevice struct {
	Index  string `xml:"index"`
	Memory struct {
		Total string `xml:"total"`
		Free  string `xml:"free"`
	} `xml:"fb_memory_usage"`
}

// GPUInventory is what a vendor probe reports about the accelerators on a node.
type GPUInventory struct {
	Models   []string
	MemoryMB []int
	Version  string

	// FreeMB is the least free memory across devices and UtilPct the busiest device's
	// utilisation. Aggregates rather than per-device values because the class ad they feed is
	// capped at 512 bytes, and because a scheduling requirement is nearly always "is there a
	// device with room" rather than a statement about a particular card.
	FreeMB  int
	UtilPct int

	// MIGEnabled reports whether any GPU is partitioned. When it is, Models describes the MIG
	// instances rather than the physical cards, because an instance is what a job can be given.
	MIGEnabled bool
}

// Count is how many schedulable devices the node has.
func (g GPUInventory) Count() int { return len(g.Models) }

// parseNvidiaSMIXML reads `nvidia-smi -q -x` output into an inventory.
func parseNvidiaSMIXML(data []byte) (GPUInventory, error) {
	var log nvidiaSMILog
	if err := xml.Unmarshal(data, &log); err != nil {
		return GPUInventory{}, fmt.Errorf("parse nvidia-smi XML: %w", err)
	}

	inv := GPUInventory{Version: strings.TrimSpace(log.CUDAVersion)}
	freeMB := -1

	for _, gpu := range log.GPUs {
		if strings.EqualFold(strings.TrimSpace(gpu.MIGMode.Current), "enabled") && len(gpu.MIGDevices) > 0 {
			// A partitioned card is not one device. Reporting it as one would let the scheduler
			// place a job on "the GPU" that is really seven separate instances, and reporting
			// the whole card's memory would promise capacity no single instance has.
			inv.MIGEnabled = true
			for _, mig := range gpu.MIGDevices {
				inv.Models = append(inv.Models, migName(gpu.ProductName, mig.Index))
				inv.MemoryMB = append(inv.MemoryMB, parseMiB(mig.Memory.Total))
				if free := parseMiB(mig.Memory.Free); free >= 0 && (freeMB < 0 || free < freeMB) {
					freeMB = free
				}
			}
			continue
		}

		name := strings.TrimSpace(gpu.ProductName)
		if name == "" {
			name = "NVIDIA GPU"
		}
		inv.Models = append(inv.Models, name)
		inv.MemoryMB = append(inv.MemoryMB, parseMiB(gpu.Memory.Total))

		if free := parseMiB(gpu.Memory.Free); free >= 0 && (freeMB < 0 || free < freeMB) {
			freeMB = free
		}
		if util := parsePercent(gpu.Utilization.GPUUtil); util > inv.UtilPct {
			inv.UtilPct = util
		}
	}

	if freeMB >= 0 {
		inv.FreeMB = freeMB
	}
	if len(inv.Models) == 0 {
		return GPUInventory{}, fmt.Errorf("nvidia-smi reported no GPUs")
	}
	return inv, nil
}

// migName labels a MIG instance so the model list distinguishes instances of the same card.
func migName(product, index string) string {
	product = strings.TrimSpace(product)
	if product == "" {
		product = "NVIDIA GPU"
	}
	if index == "" {
		return product + " MIG"
	}
	return fmt.Sprintf("%s MIG %s", product, strings.TrimSpace(index))
}

// parseMiB reads a value like "40960 MiB". Returns -1 when the driver reports "N/A", which it
// does for fields a particular card does not support — distinct from a real zero.
func parseMiB(value string) int {
	fields := strings.Fields(strings.TrimSpace(value))
	if len(fields) == 0 {
		return -1
	}
	n, err := strconv.Atoi(fields[0])
	if err != nil {
		return -1
	}
	if len(fields) > 1 && strings.EqualFold(fields[1], "GiB") {
		n *= 1024
	}
	return n
}

// parsePercent reads a value like "37 %".
func parsePercent(value string) int {
	fields := strings.Fields(strings.TrimSpace(value))
	if len(fields) == 0 {
		return 0
	}
	n, err := strconv.Atoi(fields[0])
	if err != nil {
		return 0
	}
	return n
}
