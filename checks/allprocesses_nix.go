// +build !windows

package checks

import (
	"github.com/DataDog/datadog-process-agent/config"
	"github.com/DataDog/gopsutil/process"
)

func getAllProcesses(*config.AgentConfig) (map[int32]*process.FilledProcess, error) {
	return process.AllProcesses()
}
