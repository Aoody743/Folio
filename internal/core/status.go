package core

import (
	"bufio"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

type Usage struct {
	UsedBytes  uint64  `json:"used_bytes"`
	TotalBytes uint64  `json:"total_bytes"`
	Percent    float64 `json:"percent"`
}
type SystemStatus struct {
	Hostname              string    `json:"hostname"`
	OS                    string    `json:"os"`
	DistributionID        string    `json:"distribution_id,omitempty"`
	DistributionName      string    `json:"distribution_name,omitempty"`
	Architecture          string    `json:"architecture"`
	CPUCores              int       `json:"cpu_cores"`
	CPUPercent            *float64  `json:"cpu_percent"`
	Memory                *Usage    `json:"memory"`
	Disk                  *Usage    `json:"disk"`
	DiskPath              string    `json:"disk_path"`
	UptimeSeconds         *float64  `json:"uptime_seconds"`
	LoadAverage           []float64 `json:"load_average"`
	UpdatedAt             string    `json:"updated_at"`
	SampledAt             string    `json:"sampled_at"`
	SampleIntervalSeconds int       `json:"sample_interval_seconds"`
	Available             bool      `json:"available"`
	Errors                []string  `json:"errors,omitempty"`
}
type statusSampler struct {
	mu                          sync.RWMutex
	value                       SystemStatus
	stop                        chan struct{}
	done                        chan struct{}
	previousTotal, previousIdle uint64
	once                        sync.Once
}

func newStatusSampler() *statusSampler {
	s := &statusSampler{stop: make(chan struct{}), done: make(chan struct{})}
	s.sample()
	go func() {
		defer close(s.done)
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for {
			select {
			case <-s.stop:
				return
			case <-t.C:
				s.sample()
			}
		}
	}()
	return s
}
func (s *statusSampler) close() { s.once.Do(func() { close(s.stop); <-s.done }) }
func (s *statusSampler) get() SystemStatus {
	s.mu.RLock()
	defer s.mu.RUnlock()
	v := s.value
	v.LoadAverage = append([]float64(nil), v.LoadAverage...)
	v.Errors = append([]string(nil), v.Errors...)
	return v
}
func (s *statusSampler) sample() {
	host, _ := os.Hostname()
	stamp := nowString()
	v := SystemStatus{Hostname: host, OS: runtime.GOOS, Architecture: runtime.GOARCH, CPUCores: runtime.NumCPU(), DiskPath: "/", UpdatedAt: stamp, SampledAt: stamp, SampleIntervalSeconds: 1, Available: true, LoadAverage: []float64{}}
	if runtime.GOOS == "linux" {
		if data, err := os.ReadFile("/etc/os-release"); err == nil {
			v.DistributionID, v.DistributionName = parseDistribution(string(data))
		}
	}
	if data, e := os.ReadFile("/proc/stat"); e == nil {
		line := strings.SplitN(string(data), "\n", 2)[0]
		fields := strings.Fields(line)
		var total, idle uint64
		for i, f := range fields[1:] {
			n, _ := strconv.ParseUint(f, 10, 64)
			if i < 8 {
				total += n
			}
			if i == 3 || i == 4 {
				idle += n
			}
		}
		if s.previousTotal > 0 && total > s.previousTotal && idle >= s.previousIdle {
			p := 100 * (1 - float64(idle-s.previousIdle)/float64(total-s.previousTotal))
			v.CPUPercent = &p
		}
		s.previousTotal = total
		s.previousIdle = idle
	} else {
		v.Errors = append(v.Errors, "CPU statistics unavailable")
	}
	if data, e := os.ReadFile("/proc/meminfo"); e == nil {
		mem := map[string]uint64{}
		scan := bufio.NewScanner(strings.NewReader(string(data)))
		for scan.Scan() {
			f := strings.Fields(scan.Text())
			if len(f) >= 2 {
				n, _ := strconv.ParseUint(f[1], 10, 64)
				mem[strings.TrimSuffix(f[0], ":")] = n * 1024
			}
		}
		total := mem["MemTotal"]
		available := mem["MemAvailable"]
		if available == 0 {
			available = mem["MemFree"] + mem["Buffers"] + mem["Cached"]
		}
		if available > total {
			available = total
		}
		if total > 0 {
			u := Usage{UsedBytes: total - available, TotalBytes: total, Percent: 100 * float64(total-available) / float64(total)}
			v.Memory = &u
		}
	} else {
		v.Errors = append(v.Errors, "Memory statistics unavailable")
	}
	var fs syscall.Statfs_t
	if e := syscall.Statfs("/", &fs); e == nil {
		total := fs.Blocks * uint64(fs.Bsize)
		free := fs.Bfree * uint64(fs.Bsize)
		if total > 0 {
			u := Usage{UsedBytes: total - free, TotalBytes: total, Percent: 100 * float64(total-free) / float64(total)}
			v.Disk = &u
		}
	} else {
		v.Errors = append(v.Errors, "Disk statistics unavailable")
	}
	if data, e := os.ReadFile("/proc/uptime"); e == nil {
		f := strings.Fields(string(data))
		if len(f) > 0 {
			if seconds, e := strconv.ParseFloat(f[0], 64); e == nil {
				v.UptimeSeconds = &seconds
			}
		}
	}
	if data, e := os.ReadFile("/proc/loadavg"); e == nil {
		for i, f := range strings.Fields(string(data)) {
			if i >= 3 {
				break
			}
			n, e := strconv.ParseFloat(f, 64)
			if e == nil {
				v.LoadAverage = append(v.LoadAverage, n)
			}
		}
	}
	if v.Memory == nil && v.CPUPercent == nil {
		v.Available = false
	}
	s.mu.Lock()
	s.value = v
	s.mu.Unlock()
}

func parseDistribution(data string) (id, name string) {
	for _, line := range strings.Split(data, "\n") {
		key, value, ok := strings.Cut(line, "=")
		if !ok || (key != "ID" && key != "PRETTY_NAME") {
			continue
		}
		value = strings.TrimSpace(value)
		if strings.HasPrefix(value, "\"") {
			if decoded, err := strconv.Unquote(value); err == nil {
				value = decoded
			} else {
				continue
			}
		} else {
			value = strings.Trim(value, "'")
		}
		if key == "ID" {
			id = value
		} else {
			name = value
		}
	}
	return
}
