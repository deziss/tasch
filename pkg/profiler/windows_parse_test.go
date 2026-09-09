package profiler

import "testing"

// TestWindowsVRAMHandlesLargeCards is the regression test for the defect that made
// `ad.gpu_memory_mb` unusable on Windows. Win32_VideoController.AdapterRAM is a 32-bit field, so
// any card with 4 GB or more wraps — PowerShell renders it as a negative number — and the old
// code clamped anything negative to zero. Every modern Windows GPU therefore advertised 0 MB.
func TestWindowsVRAMHandlesLargeCards(t *testing.T) {
	cases := []struct {
		name string
		json string
		want int
	}{
		{
			// An 8 GB card: AdapterRAM wraps to -2147483648, the registry has the true size.
			name: "8GB with registry size",
			json: `{"Name":"NVIDIA GeForce RTX 4060 Ti","AdapterRAM":-2147483648,"QwMemorySize":8589934592}`,
			want: 8192,
		},
		{
			// The same card with no registry entry: the wrapped value must be reinterpreted as
			// unsigned rather than discarded.
			name: "wrapped AdapterRAM, no registry",
			json: `{"Name":"NVIDIA GeForce RTX 4060 Ti","AdapterRAM":-2147483648,"QwMemorySize":null}`,
			want: 2048,
		},
		{
			// A 2 GB card, which fits in 32 bits and was always reported correctly.
			name: "small card",
			json: `{"Name":"Intel UHD Graphics","AdapterRAM":2147483648,"QwMemorySize":null}`,
			want: 2048,
		},
		{
			// A 24 GB card. Only the registry can express this at all.
			name: "24GB card",
			json: `{"Name":"NVIDIA RTX A5000","AdapterRAM":-1,"QwMemorySize":25769803776}`,
			want: 24576,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, memory := parseWindowsGPUs([]byte(tc.json))
			if len(memory) != 1 {
				t.Fatalf("got %d GPUs, want 1", len(memory))
			}
			if memory[0] != tc.want {
				t.Errorf("VRAM = %d MB, want %d MB", memory[0], tc.want)
			}
		})
	}
}

// TestParseWindowsGPUsAcceptsBothShapes covers PowerShell emitting a bare object for a single
// result and an array for several.
func TestParseWindowsGPUsAcceptsBothShapes(t *testing.T) {
	single := `{"Name":"NVIDIA RTX A5000","AdapterRAM":0,"QwMemorySize":25769803776}`
	models, _ := parseWindowsGPUs([]byte(single))
	if len(models) != 1 || models[0] != "NVIDIA RTX A5000" {
		t.Errorf("single object gave %v", models)
	}

	array := `[{"Name":"NVIDIA RTX A5000","AdapterRAM":0,"QwMemorySize":25769803776},
	           {"Name":"Intel UHD Graphics","AdapterRAM":1073741824,"QwMemorySize":null}]`
	models, memory := parseWindowsGPUs([]byte(array))
	if len(models) != 2 {
		t.Fatalf("array gave %d GPUs, want 2", len(models))
	}
	if memory[0] != 24576 || memory[1] != 1024 {
		t.Errorf("memory = %v, want [24576 1024]", memory)
	}
}

// TestParseWindowsGPUsSkipsVirtualAdapters confirms RDP and hypervisor display devices are not
// advertised as schedulable GPUs, which would attract GPU jobs to a node with none.
func TestParseWindowsGPUsSkipsVirtualAdapters(t *testing.T) {
	raw := `[{"Name":"Microsoft Basic Display Adapter","AdapterRAM":0,"QwMemorySize":null},
	         {"Name":"Citrix Indirect Display Adapter","AdapterRAM":0,"QwMemorySize":null},
	         {"Name":"NVIDIA RTX A5000","AdapterRAM":0,"QwMemorySize":25769803776}]`
	models, _ := parseWindowsGPUs([]byte(raw))
	if len(models) != 1 || models[0] != "NVIDIA RTX A5000" {
		t.Errorf("models = %v, want only the real GPU", models)
	}
}

// TestParseWindowsGPUsHandlesStringNumbers covers PowerShell quoting large integers.
func TestParseWindowsGPUsHandlesStringNumbers(t *testing.T) {
	raw := `{"Name":"NVIDIA RTX A5000","AdapterRAM":"0","QwMemorySize":"25769803776"}`
	_, memory := parseWindowsGPUs([]byte(raw))
	if len(memory) != 1 || memory[0] != 24576 {
		t.Errorf("memory = %v, want [24576]", memory)
	}
}

func TestParseWindowsGPUsHandlesGarbage(t *testing.T) {
	for _, raw := range []string{"", "   ", "not json", "null"} {
		if models, _ := parseWindowsGPUs([]byte(raw)); models != nil {
			t.Errorf("input %q gave %v, want nil", raw, models)
		}
	}
}

func TestVendorFromModel(t *testing.T) {
	cases := map[string]string{
		"NVIDIA GeForce RTX 4090":  "nvidia",
		"NVIDIA A100-SXM4-80GB":    "nvidia",
		"AMD Radeon RX 7900 XTX":   "amd",
		"Intel(R) Arc(TM) A770":    "intel",
		"Qualcomm Adreno 690":      "qualcomm",
		"Some Unknown Accelerator": "generic",
	}
	for model, want := range cases {
		if got := vendorFromModel(model); got != want {
			t.Errorf("vendorFromModel(%q) = %q, want %q", model, got, want)
		}
	}
}
