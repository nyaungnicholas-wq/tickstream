package bench

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
)

// Disclosure is the mandatory hardware/environment block. A latency number
// without it is marketing, not measurement (spec §9.2.7) — and a placeholder
// disclosure is WORSE than none, so Validate hard-fails on any empty field
// and the harness refuses to emit results without a valid one.
type Disclosure struct {
	CPUModel   string
	Cores      int
	RAM        string
	OS         string
	GoVersion  string
	GOMAXPROCS int
	RaceBuild  bool // must be false for recorded numbers
	Idle       bool // operator-attested
}

// Collect auto-fills the disclosure from the real machine. The CPU model is
// read from the OS (sysctl / /proc/cpuinfo) — never typed by a human.
func Collect(idle bool) (Disclosure, error) {
	d := Disclosure{
		Cores:      runtime.NumCPU(),
		GoVersion:  runtime.Version(),
		GOMAXPROCS: runtime.GOMAXPROCS(0),
		RaceBuild:  raceEnabled,
		Idle:       idle,
	}
	switch runtime.GOOS {
	case "darwin":
		cpu, err := sysctl("machdep.cpu.brand_string")
		if err != nil {
			return d, fmt.Errorf("read CPU model: %w", err)
		}
		d.CPUModel = cpu
		if mem, err := sysctl("hw.memsize"); err == nil {
			if b, perr := strconv.ParseUint(mem, 10, 64); perr == nil {
				d.RAM = fmt.Sprintf("%d GB", b>>30)
			}
		}
		if rel, err := sysctl("kern.osrelease"); err == nil {
			d.OS = "macOS (Darwin " + rel + ")"
		}
	case "linux":
		d.CPUModel = linuxCPUModel()
		d.RAM = linuxRAM()
		if out, err := exec.Command("uname", "-r").Output(); err == nil {
			d.OS = "Linux " + strings.TrimSpace(string(out))
		}
	default:
		return d, fmt.Errorf("unsupported GOOS %q for hardware disclosure", runtime.GOOS)
	}
	return d, d.Validate()
}

// Validate refuses any incomplete disclosure. No "<fill me in>" ever ships.
func (d Disclosure) Validate() error {
	var missing []string
	if strings.TrimSpace(d.CPUModel) == "" {
		missing = append(missing, "CPUModel")
	}
	if strings.TrimSpace(d.RAM) == "" {
		missing = append(missing, "RAM")
	}
	if strings.TrimSpace(d.OS) == "" {
		missing = append(missing, "OS")
	}
	if strings.TrimSpace(d.GoVersion) == "" {
		missing = append(missing, "GoVersion")
	}
	if d.Cores == 0 || d.GOMAXPROCS == 0 {
		missing = append(missing, "Cores/GOMAXPROCS")
	}
	if len(missing) > 0 {
		return fmt.Errorf("REFUSING to emit results: disclosure fields empty: %s", strings.Join(missing, ", "))
	}
	if d.RaceBuild {
		return fmt.Errorf("REFUSING to emit results: built with -race (race-instrumented timings are 2-20x slower and meaningless)")
	}
	return nil
}

func sysctl(key string) (string, error) {
	out, err := exec.Command("sysctl", "-n", key).Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func linuxCPUModel() string {
	data, err := os.ReadFile("/proc/cpuinfo")
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "model name") {
			if _, val, ok := strings.Cut(line, ":"); ok {
				return strings.TrimSpace(val)
			}
		}
	}
	return ""
}

func linuxRAM() string {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "MemTotal:") {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				if kb, perr := strconv.ParseUint(fields[1], 10, 64); perr == nil {
					return fmt.Sprintf("%d GB", kb>>20)
				}
			}
		}
	}
	return ""
}
