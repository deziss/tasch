package profiler

import "testing"

// appleSiliconOutput is what system_profiler reports on Apple Silicon: a GPU sharing system
// memory, with no VRAM line at all.
const appleSiliconOutput = `Graphics/Displays:

    Apple M2 Ultra:

      Chipset Model: Apple M2 Ultra
      Type: GPU
      Bus: Built-In
      Total Number of Cores: 60
      Vendor: Apple (0x106b)
      Metal Support: Metal 3
`

// discreteOutput is an Intel Mac with a dedicated card, which does report VRAM.
const discreteOutput = `Graphics/Displays:

    AMD Radeon Pro 5500M:

      Chipset Model: AMD Radeon Pro 5500M
      Type: GPU
      Bus: PCIe
      VRAM (Total): 4 GB
      Vendor: AMD (0x1002)
`

// headlessOutput is a VM or headless Mac: the command succeeds and reports no GPU.
const headlessOutput = `Graphics/Displays:

    No display hardware was found.
`

// TestParseMacGPUsHeadlessReportsNone is the regression test for a node advertising hardware it
// does not have: the count was set to 1 whenever system_profiler merely succeeded, so a headless
// Mac or a VM attracted GPU jobs it could not run.
func TestParseMacGPUsHeadlessReportsNone(t *testing.T) {
	if gpus := parseMacGPUs(headlessOutput); len(gpus) != 0 {
		t.Errorf("got %d GPUs on a headless machine, want 0: %+v", len(gpus), gpus)
	}
	if gpus := parseMacGPUs(""); len(gpus) != 0 {
		t.Errorf("got %d GPUs from empty output, want 0", len(gpus))
	}
}

func TestParseMacGPUsAppleSilicon(t *testing.T) {
	gpus := parseMacGPUs(appleSiliconOutput)
	if len(gpus) != 1 {
		t.Fatalf("got %d GPUs, want 1", len(gpus))
	}
	if gpus[0].Model != "Apple M2 Ultra" {
		t.Errorf("model = %q, want %q", gpus[0].Model, "Apple M2 Ultra")
	}
	// Unified memory means no dedicated VRAM is reported; the caller substitutes a share of
	// system memory rather than the parser inventing one.
	if gpus[0].VRAMMegabytes != 0 {
		t.Errorf("VRAM = %d, want 0 for unified memory", gpus[0].VRAMMegabytes)
	}
	if !gpus[0].Unified {
		t.Error("Apple vendor was not recognised as unified memory")
	}
}

func TestParseMacGPUsDiscreteCard(t *testing.T) {
	gpus := parseMacGPUs(discreteOutput)
	if len(gpus) != 1 {
		t.Fatalf("got %d GPUs, want 1", len(gpus))
	}
	if gpus[0].VRAMMegabytes != 4096 {
		t.Errorf("VRAM = %d MB, want 4096", gpus[0].VRAMMegabytes)
	}
}

// TestParseMacGPUsKeepsEveryAdapter is the regression test for only the last one surviving: the
// old code overwrote its single model variable on each Chipset Model line, so a Mac with an eGPU
// or a discrete plus integrated pair reported one GPU instead of two.
func TestParseMacGPUsKeepsEveryAdapter(t *testing.T) {
	both := appleSiliconOutput + discreteOutput
	gpus := parseMacGPUs(both)
	if len(gpus) != 2 {
		t.Fatalf("got %d GPUs, want 2: %+v", len(gpus), gpus)
	}
	if gpus[0].Model != "Apple M2 Ultra" || gpus[1].Model != "AMD Radeon Pro 5500M" {
		t.Errorf("models = %q, %q", gpus[0].Model, gpus[1].Model)
	}
	if gpus[1].VRAMMegabytes != 4096 {
		t.Errorf("second GPU VRAM = %d, want 4096 — VRAM was attributed to the wrong adapter",
			gpus[1].VRAMMegabytes)
	}
}

func TestParseAppleMemorySize(t *testing.T) {
	cases := map[string]int{
		"4 GB":    4096,
		"1536 MB": 1536,
		"8 GB":    8192,
		"1 TB":    1048576,
		"1536":    1536,
		"":        0,
		"unknown": 0,
		"-2 GB":   0,
	}
	for input, want := range cases {
		if got := parseAppleMemorySize(input); got != want {
			t.Errorf("parseAppleMemorySize(%q) = %d, want %d", input, got, want)
		}
	}
}

func TestVendorFromMacModel(t *testing.T) {
	cases := map[string]string{
		"Apple M2 Ultra":         "apple",
		"AMD Radeon Pro 5500M":   "amd",
		"Intel Iris Plus":        "intel",
		"NVIDIA GeForce GT 750M": "nvidia",
	}
	for model, want := range cases {
		if got := vendorFromMacModel(model); got != want {
			t.Errorf("vendorFromMacModel(%q) = %q, want %q", model, got, want)
		}
	}
}

// TestUnifiedMemoryMB confirms an unreadable memory size yields no advertised GPU memory rather
// than a fabricated figure. The old code fell back to a hardcoded 8 GB, which would let a job
// match a machine that could not hold it.
func TestUnifiedMemoryMB(t *testing.T) {
	const gb = int64(1024 * 1024 * 1024)

	if got := unifiedMemoryMB(64 * gb); got != 49152 {
		t.Errorf("64 GB system gave %d MB, want 49152 (75%%)", got)
	}
	if got := unifiedMemoryMB(0); got != 0 {
		t.Errorf("unknown memory size gave %d MB, want 0", got)
	}
	if got := unifiedMemoryMB(-1); got != 0 {
		t.Errorf("negative memory size gave %d MB, want 0", got)
	}
}
