package profiler

import (
	"encoding/json"
	"fmt"
	"testing"
)

// nvidiaAd builds a realistic ClassAd for an n-GPU NVIDIA node.
func nvidiaAd(n int) ClassAd {
	models := make([]string, n)
	memory := make([]int, n)
	for i := range models {
		models[i] = "NVIDIA A100-SXM4-80GB"
		memory[i] = 81920
	}
	return ClassAd{
		Timestamp:      1757318400,
		OS:             "linux",
		Architecture:   "amd64",
		CPUCores:       128,
		CPUModel:       "AMD EPYC 7763 64-Core Processor",
		TotalMemoryMB:  1031000,
		AvailableMemMB: 900000,
		HostType:       "vm_or_baremetal",
		GPUCount:       n,
		GPUVendor:      "nvidia",
		GPUModels:      models,
		GPUMemoryMB:    memory,
		CUDAVersion:    "12.4",
	}
}

// TestFitClassAdRespectsGossipLimit is the regression test for the defect where an 8-GPU node
// panicked memberlist at startup: the serialized ad was 553 bytes against a hard 512 limit.
func TestFitClassAdRespectsGossipLimit(t *testing.T) {
	for _, gpus := range []int{0, 1, 2, 4, 8, 16, 64} {
		t.Run(fmt.Sprintf("%dGPU", gpus), func(t *testing.T) {
			b, err := fitClassAd(nvidiaAd(gpus), MaxMetaBytes)
			if err != nil {
				t.Fatalf("fitClassAd(%d GPUs): %v", gpus, err)
			}
			if len(b) > MaxMetaBytes {
				t.Fatalf("ad is %d bytes, over the %d byte limit", len(b), MaxMetaBytes)
			}
			if !json.Valid(b) {
				t.Fatalf("ad is not valid JSON: %s", b)
			}
		})
	}
}

// TestFitClassAdPreservesMatchableFields asserts that shrinking never changes the answer to
// "can this node run this job". Every field reachable from a CEL requirement must survive.
func TestFitClassAdPreservesMatchableFields(t *testing.T) {
	original := nvidiaAd(16)
	b, err := fitClassAd(original, MaxMetaBytes)
	if err != nil {
		t.Fatalf("fitClassAd: %v", err)
	}

	var got ClassAd
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if got.GPUCount != original.GPUCount {
		t.Errorf("gpu_count = %d, want %d", got.GPUCount, original.GPUCount)
	}
	if got.GPUVendor != original.GPUVendor {
		t.Errorf("gpu_vendor = %q, want %q", got.GPUVendor, original.GPUVendor)
	}
	if got.CPUCores != original.CPUCores {
		t.Errorf("cpu_cores = %d, want %d", got.CPUCores, original.CPUCores)
	}
	if got.TotalMemoryMB != original.TotalMemoryMB {
		t.Errorf("total_memory_mb = %d, want %d", got.TotalMemoryMB, original.TotalMemoryMB)
	}
	if got.OS != original.OS || got.Architecture != original.Architecture {
		t.Errorf("os/arch = %q/%q, want %q/%q", got.OS, got.Architecture, original.OS, original.Architecture)
	}
	if got.HostType != original.HostType {
		t.Errorf("host_type = %q, want %q", got.HostType, original.HostType)
	}
	if got.CUDAVersion != original.CUDAVersion {
		t.Errorf("cuda_version = %q, want %q", got.CUDAVersion, original.CUDAVersion)
	}
	// `ad.gpu_memory_mb[0] >= N` is the documented idiom; index 0 must stay meaningful even
	// after identical entries collapse.
	if len(got.GPUMemoryMB) == 0 {
		t.Fatal("gpu_memory_mb was dropped entirely; ad.gpu_memory_mb[0] requirements break")
	}
	if got.GPUMemoryMB[0] != original.GPUMemoryMB[0] {
		t.Errorf("gpu_memory_mb[0] = %d, want %d", got.GPUMemoryMB[0], original.GPUMemoryMB[0])
	}
}

// TestFitClassAdKeepsSmallAdsIntact confirms the common case pays nothing for the ladder.
func TestFitClassAdKeepsSmallAdsIntact(t *testing.T) {
	ad := nvidiaAd(2)
	b, err := fitClassAd(ad, MaxMetaBytes)
	if err != nil {
		t.Fatalf("fitClassAd: %v", err)
	}
	var got ClassAd
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.CPUModel != ad.CPUModel {
		t.Errorf("cpu_model = %q, want %q — a 2-GPU ad fits and should not be reduced", got.CPUModel, ad.CPUModel)
	}
	if len(got.GPUModels) != len(ad.GPUModels) {
		t.Errorf("gpu_models has %d entries, want %d", len(got.GPUModels), len(ad.GPUModels))
	}
}

// TestFitClassAdHeterogeneousGPUs covers a node whose GPUs differ, where collapsing to
// distinct values still leaves more than one entry.
func TestFitClassAdHeterogeneousGPUs(t *testing.T) {
	ad := nvidiaAd(8)
	ad.GPUModels[7] = "NVIDIA H100 80GB HBM3"
	ad.GPUMemoryMB[7] = 81559

	b, err := fitClassAd(ad, MaxMetaBytes)
	if err != nil {
		t.Fatalf("fitClassAd: %v", err)
	}
	if len(b) > MaxMetaBytes {
		t.Fatalf("ad is %d bytes, over the %d byte limit", len(b), MaxMetaBytes)
	}
	if !json.Valid(b) {
		t.Fatalf("ad is not valid JSON: %s", b)
	}
}
