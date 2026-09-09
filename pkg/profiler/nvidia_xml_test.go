package profiler

import "testing"

// A two-GPU node as nvidia-smi -q -x actually reports it, trimmed to the elements that matter.
const twoGPUXML = `<?xml version="1.0" ?>
<nvidia_smi_log>
  <driver_version>535.104.05</driver_version>
  <cuda_version>12.2</cuda_version>
  <attached_gpus>2</attached_gpus>
  <gpu id="00000000:01:00.0">
    <product_name>NVIDIA A100-SXM4-40GB</product_name>
    <uuid>GPU-1111</uuid>
    <fb_memory_usage>
      <total>40960 MiB</total>
      <used>1024 MiB</used>
      <free>39936 MiB</free>
    </fb_memory_usage>
    <utilization>
      <gpu_util>17 %</gpu_util>
    </utilization>
    <mig_mode>
      <current_mig>Disabled</current_mig>
    </mig_mode>
  </gpu>
  <gpu id="00000000:02:00.0">
    <product_name>NVIDIA A100-SXM4-40GB</product_name>
    <uuid>GPU-2222</uuid>
    <fb_memory_usage>
      <total>40960 MiB</total>
      <used>30000 MiB</used>
      <free>10960 MiB</free>
    </fb_memory_usage>
    <utilization>
      <gpu_util>91 %</gpu_util>
    </utilization>
    <mig_mode>
      <current_mig>Disabled</current_mig>
    </mig_mode>
  </gpu>
</nvidia_smi_log>`

func TestParseNvidiaSMIXML(t *testing.T) {
	inv, err := parseNvidiaSMIXML([]byte(twoGPUXML))
	if err != nil {
		t.Fatalf("parseNvidiaSMIXML: %v", err)
	}

	if inv.Count() != 2 {
		t.Fatalf("count = %d, want 2", inv.Count())
	}
	// The CUDA version used to be scraped out of the banner with a regular expression, and a
	// miss reported no CUDA at all rather than an error.
	if inv.Version != "12.2" {
		t.Fatalf("CUDA version = %q, want 12.2", inv.Version)
	}
	if inv.MemoryMB[0] != 40960 {
		t.Fatalf("memory = %v, want 40960 for the first card", inv.MemoryMB)
	}

	// Free memory is the *least* across devices, so a requirement written against it holds for
	// at least one card; utilisation is the busiest, so a requirement for an idle node is not
	// satisfied by an average over a card that is pinned.
	if inv.FreeMB != 10960 {
		t.Fatalf("free = %d, want the busiest card's 10960", inv.FreeMB)
	}
	if inv.UtilPct != 91 {
		t.Fatalf("utilisation = %d, want the busiest card's 91", inv.UtilPct)
	}
	if inv.MIGEnabled {
		t.Fatal("MIG reported on a node with it disabled")
	}
}

const migXML = `<?xml version="1.0" ?>
<nvidia_smi_log>
  <driver_version>535.104.05</driver_version>
  <cuda_version>12.2</cuda_version>
  <gpu id="00000000:01:00.0">
    <product_name>NVIDIA A100-SXM4-40GB</product_name>
    <fb_memory_usage>
      <total>40960 MiB</total>
      <free>40000 MiB</free>
    </fb_memory_usage>
    <mig_mode>
      <current_mig>Enabled</current_mig>
    </mig_mode>
    <mig_devices>
      <mig_device>
        <index>0</index>
        <fb_memory_usage><total>20096 MiB</total><free>20096 MiB</free></fb_memory_usage>
      </mig_device>
      <mig_device>
        <index>1</index>
        <fb_memory_usage><total>20096 MiB</total><free>9000 MiB</free></fb_memory_usage>
      </mig_device>
    </mig_devices>
  </gpu>
</nvidia_smi_log>`

// A partitioned card is not one device. Reporting it as one would let the scheduler place a job
// on "the GPU" that is really several instances, and advertising the whole card's memory would
// promise capacity no single instance has.
func TestParseNvidiaSMIXMLReportsMIGInstances(t *testing.T) {
	inv, err := parseNvidiaSMIXML([]byte(migXML))
	if err != nil {
		t.Fatalf("parseNvidiaSMIXML: %v", err)
	}

	if !inv.MIGEnabled {
		t.Fatal("MIG mode was not reported")
	}
	if inv.Count() != 2 {
		t.Fatalf("count = %d, want the 2 MIG instances rather than 1 card", inv.Count())
	}
	if inv.MemoryMB[0] != 20096 {
		t.Fatalf("instance memory = %v, want each instance's own 20096", inv.MemoryMB)
	}
	if inv.FreeMB != 9000 {
		t.Fatalf("free = %d, want the fullest instance's 9000", inv.FreeMB)
	}
	for _, name := range inv.Models {
		if name == "NVIDIA A100-SXM4-40GB" {
			t.Fatalf("instances should be distinguishable from the card, got %v", inv.Models)
		}
	}
}

// "N/A" is what the driver reports for a field a card does not support, and it must not be read
// as a real zero — a zero would advertise a card with no memory as schedulable.
func TestParseNvidiaSMIXMLHandlesUnsupportedFields(t *testing.T) {
	const partial = `<nvidia_smi_log>
  <cuda_version>11.8</cuda_version>
  <gpu>
    <product_name>NVIDIA T400</product_name>
    <fb_memory_usage><total>N/A</total><free>N/A</free></fb_memory_usage>
    <utilization><gpu_util>N/A</gpu_util></utilization>
  </gpu>
</nvidia_smi_log>`

	inv, err := parseNvidiaSMIXML([]byte(partial))
	if err != nil {
		t.Fatalf("parseNvidiaSMIXML: %v", err)
	}
	if inv.Count() != 1 || inv.Models[0] != "NVIDIA T400" {
		t.Fatalf("models = %v", inv.Models)
	}
	if inv.MemoryMB[0] != -1 {
		t.Fatalf("unsupported memory = %d, want -1 to mark it unknown rather than zero", inv.MemoryMB[0])
	}
	if inv.FreeMB != 0 || inv.UtilPct != 0 {
		t.Fatalf("unknown live values should stay at zero, got free=%d util=%d", inv.FreeMB, inv.UtilPct)
	}
}

// A machine with the driver installed but no cards attached must be reported as having none,
// not as an unparseable failure that hides a real problem.
func TestParseNvidiaSMIXMLWithNoGPUs(t *testing.T) {
	_, err := parseNvidiaSMIXML([]byte(`<nvidia_smi_log><attached_gpus>0</attached_gpus></nvidia_smi_log>`))
	if err == nil {
		t.Fatal("XML with no GPUs should not report an inventory")
	}
}

func TestParseNvidiaSMIXMLRejectsGarbage(t *testing.T) {
	if _, err := parseNvidiaSMIXML([]byte("this is not XML at all")); err == nil {
		t.Fatal("non-XML input should be an error, not an empty inventory")
	}
}

func TestParseMiB(t *testing.T) {
	tests := map[string]int{
		"40960 MiB": 40960,
		"1 GiB":     1024,
		"N/A":       -1,
		"":          -1,
		"  512 MiB": 512,
	}
	for input, want := range tests {
		if got := parseMiB(input); got != want {
			t.Errorf("parseMiB(%q) = %d, want %d", input, got, want)
		}
	}
}
