package checks

import (
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/DataDog/datadog-process-agent/config"
	"github.com/DataDog/gopsutil/cpu"
	"github.com/DataDog/gopsutil/process"
	"github.com/stretchr/testify/assert"

	"github.com/DataDog/datadog-agent/pkg/util/containers"
)

func makeProcess(pid int32, cmdline string) *process.FilledProcess {
	return &process.FilledProcess{
		Pid:         pid,
		Cmdline:     strings.Split(cmdline, " "),
		MemInfo:     &process.MemoryInfoStat{},
		CtxSwitches: &process.NumCtxSwitchesStat{},
	}
}

func TestProcessChunking(t *testing.T) {
	p := []*process.FilledProcess{
		makeProcess(1, "git clone google.com"),
		makeProcess(2, "mine-bitcoins -all -x"),
		makeProcess(3, "datadog-process-agent -ddconfig datadog.conf"),
		makeProcess(4, "foo -bar -bim"),
	}
	containers := []*containers.Container{}
	lastRun := time.Now().Add(-5 * time.Second)
	syst1, syst2 := cpu.TimesStat{}, cpu.TimesStat{}
	cfg := config.NewDefaultAgentConfig()

	for i, tc := range []struct {
		cur, last      []*process.FilledProcess
		maxSize        int
		blacklist      []string
		expectedTotal  int
		expectedChunks int
	}{
		{
			cur:            []*process.FilledProcess{p[0], p[1], p[2]},
			last:           []*process.FilledProcess{p[0], p[1], p[2]},
			maxSize:        1,
			blacklist:      []string{},
			expectedTotal:  3,
			expectedChunks: 3,
		},
		{
			cur:            []*process.FilledProcess{p[0], p[1], p[2]},
			last:           []*process.FilledProcess{p[0], p[2]},
			maxSize:        1,
			blacklist:      []string{},
			expectedTotal:  2,
			expectedChunks: 2,
		},
		{
			cur:            []*process.FilledProcess{p[0], p[1], p[2], p[3]},
			last:           []*process.FilledProcess{p[0], p[1], p[2], p[3]},
			maxSize:        10,
			blacklist:      []string{"git", "datadog"},
			expectedTotal:  2,
			expectedChunks: 1,
		},
		{
			cur:            []*process.FilledProcess{p[0], p[1], p[2], p[3]},
			last:           []*process.FilledProcess{p[0], p[1], p[2], p[3]},
			maxSize:        10,
			blacklist:      []string{"git", "datadog", "foo", "mine"},
			expectedTotal:  0,
			expectedChunks: 0,
		},
	} {
		bl := make([]*regexp.Regexp, 0, len(tc.blacklist))
		for _, s := range tc.blacklist {
			bl = append(bl, regexp.MustCompile(s))
		}
		cfg.Blacklist = bl
		cfg.MaxPerMessage = tc.maxSize

		cur := make(map[int32]*process.FilledProcess)
		for _, c := range tc.cur {
			cur[c.Pid] = c
		}
		last := make(map[int32]*process.FilledProcess)
		for _, c := range tc.last {
			last[c.Pid] = c
		}

		chunked := fmtProcesses(cfg, cur, last, containers, syst2, syst1, lastRun)
		assert.Len(t, chunked, tc.expectedChunks, "len %d", i)
		total := 0
		for _, c := range chunked {
			total += len(c)
		}
		assert.Equal(t, tc.expectedTotal, total, "total test %d", i)

		chunkedStat := fmtProcessStats(cfg, cur, last, containers, syst2, syst1, lastRun)
		assert.Len(t, chunkedStat, tc.expectedChunks, "len stat %d", i)
		total = 0
		for _, c := range chunkedStat {
			total += len(c)
		}
		assert.Equal(t, tc.expectedTotal, total, "total stat test %d", i)

	}
}

func TestPercentCalculation(t *testing.T) {
	// Capping at NUM CPU * 100 if we get odd values for delta-{Proc,Time}
	assert.True(t, floatEquals(calculatePct(100, 50, 1), 100))

	// Zero deltaTime case
	assert.True(t, floatEquals(calculatePct(100, 0, 8), 0.0))

	assert.True(t, floatEquals(calculatePct(0, 8.08, 8), 0.0))
	if runtime.GOOS != "windows" {
		assert.True(t, floatEquals(calculatePct(100, 200, 2), 100))
		assert.True(t, floatEquals(calculatePct(0.04, 8.08, 8), 3.960396))
		assert.True(t, floatEquals(calculatePct(1.09, 8.08, 8), 107.920792))
	}
}

func TestRateCalculation(t *testing.T) {
	now := time.Now()
	prev := now.Add(-1 * time.Second)
	var empty time.Time
	assert.True(t, floatEquals(calculateRate(5, 1, prev), 4))
	assert.True(t, floatEquals(calculateRate(5, 1, prev.Add(-2*time.Second)), float32(1.33333333)))
	assert.True(t, floatEquals(calculateRate(5, 1, now), 0))
	assert.True(t, floatEquals(calculateRate(5, 0, prev), 0))
	assert.True(t, floatEquals(calculateRate(5, 1, empty), 0))
}

func floatEquals(a, b float32) bool {
	var e float32 = 0.00000001 // Difference less than some epsilon
	return a-b < e && b-a < e
}
