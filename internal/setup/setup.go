package setup

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"

	"github.com/deziss/tasch/internal/config"
	"github.com/deziss/tasch/pkg/discovery"
	"github.com/deziss/tasch/pkg/profiler"
	"github.com/shirou/gopsutil/v3/cpu"
	"github.com/shirou/gopsutil/v3/mem"
)

// RunInteractive runs the interactive setup wizard and returns the config.
func RunInteractive() (*config.Config, error) {
	reader := bufio.NewReader(os.Stdin)
	cfg := config.DefaultConfig()

	fmt.Println()
	fmt.Println("  Welcome to Tasch Distributed Scheduler!")
	fmt.Println()

	// Role selection
	fmt.Println("  Select role:")
	fmt.Println("    1) Master  — schedules and dispatches jobs")
	fmt.Println("    2) Worker  — executes jobs on this machine")
	fmt.Println("    3) Both    — master + worker on this machine")
	fmt.Println()
	role := promptChoice(reader, "  Role [3]: ", "3")
	switch role {
	case "1":
		cfg.Role = "master"
	case "2":
		cfg.Role = "worker"
	default:
		cfg.Role = "both"
	}

	// Node name
	hostname, _ := os.Hostname()
	cfg.NodeName = prompt(reader, fmt.Sprintf("  Node name [%s]: ", hostname), hostname)

	// Master address
	localIP := discovery.GetLocalIP()
	if localIP == "" {
		localIP = "127.0.0.1"
	}

	if cfg.Role == "worker" {
		cfg.MasterAddr = prompt(reader, "  Master address (required): ", "")
		for cfg.MasterAddr == "" {
			fmt.Println("  Master address is required for worker nodes.")
			cfg.MasterAddr = prompt(reader, "  Master address: ", "")
		}
	} else {
		cfg.MasterAddr = prompt(reader, fmt.Sprintf("  Master address [%s]: ", localIP), localIP)
	}

	// Ports
	cfg.Ports.Gossip = promptInt(reader, "  Gossip port [7946]: ", 7946)
	cfg.Ports.GRPC = promptInt(reader, "  gRPC port [50051]: ", 50051)
	cfg.Ports.ZMQ = promptInt(reader, "  ZMQ port [5555]: ", 5555)

	// Show hardware
	fmt.Println()
	printHardware()

	return cfg, nil
}

// RunNonInteractive creates a config from flags.
func RunNonInteractive(role, nodeName, masterAddr string, gossipPort, grpcPort, zmqPort int) (*config.Config, error) {
	cfg := config.DefaultConfig()

	if role != "" {
		cfg.Role = role
	}
	if nodeName != "" {
		cfg.NodeName = nodeName
	} else {
		cfg.NodeName, _ = os.Hostname()
	}
	if masterAddr != "" {
		cfg.MasterAddr = masterAddr
	} else if cfg.Role != "worker" {
		cfg.MasterAddr = discovery.GetLocalIP()
	}
	if gossipPort > 0 {
		cfg.Ports.Gossip = gossipPort
	}
	if grpcPort > 0 {
		cfg.Ports.GRPC = grpcPort
	}
	if zmqPort > 0 {
		cfg.Ports.ZMQ = zmqPort
	}

	if cfg.Role == "worker" && cfg.MasterAddr == "" {
		return nil, fmt.Errorf("--master-addr is required for worker role")
	}

	return cfg, nil
}

func printHardware() {
	fmt.Println("  Detected hardware:")

	cores, _ := cpu.Counts(true)
	cpuInfo, _ := cpu.Info()
	model := "Unknown"
	if len(cpuInfo) > 0 {
		model = cpuInfo[0].ModelName
	}
	fmt.Printf("    CPU:  %d cores (%s)\n", cores, model)

	v, _ := mem.VirtualMemory()
	if v != nil {
		fmt.Printf("    RAM:  %dGB\n", v.Total/1024/1024/1024)
	}

	// GPU detection
	gpuCount, gpuModels, gpuMemory, version, vendor := profiler.DetectGPUs()
	if gpuCount > 0 {
		versionLabel := "CUDA"
		if vendor == "amd" {
			versionLabel = "ROCm"
		}
		for i, m := range gpuModels {
			memStr := ""
			if i < len(gpuMemory) {
				memStr = fmt.Sprintf(" (%dMB)", gpuMemory[i])
			}
			fmt.Printf("    GPU %d: %s%s\n", i, m, memStr)
		}
		if version != "" {
			fmt.Printf("    %s: %s\n", versionLabel, version)
		}
	} else {
		fmt.Println("    GPU:  None detected")
		if _, err := exec.LookPath("nvidia-smi"); err != nil {
			if _, err2 := exec.LookPath("rocm-smi"); err2 != nil {
				fmt.Println("          (nvidia-smi and rocm-smi not found)")
			}
		}
	}

	fmt.Printf("    OS:   %s/%s\n", runtime.GOOS, runtime.GOARCH)
	fmt.Println()
}

func prompt(reader *bufio.Reader, message, defaultVal string) string {
	fmt.Print(message)
	input, _ := reader.ReadString('\n')
	input = strings.TrimSpace(input)
	if input == "" {
		return defaultVal
	}
	return input
}

func promptChoice(reader *bufio.Reader, message, defaultVal string) string {
	return prompt(reader, message, defaultVal)
}

func promptInt(reader *bufio.Reader, message string, defaultVal int) int {
	s := prompt(reader, message, strconv.Itoa(defaultVal))
	v, err := strconv.Atoi(s)
	if err != nil {
		return defaultVal
	}
	return v
}
