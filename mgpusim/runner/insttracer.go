package runner

import (
	"io"
	"log"
	"os"
	"time"

	"github.com/tebeka/atexit"
	"gitlab.com/akita/akita"
	"gitlab.com/akita/util/global"
	"gitlab.com/akita/util/tracing"
)

// instTracer can trace the number of instruction completed.
type instTracer struct {
	count    uint64
	maxCount uint64

	lastWGFinishTime akita.VTimeInSec

	inflightInst map[uint64]tracing.Task

	runner *Runner

	logger *log.Logger

	timeLimit time.Duration

	// nextCheckpointIdx is the next index into checkpointPercents that
	// has not yet fired. Because percentages are sorted ascending and the
	// retired count is monotonically non-decreasing, we just walk the slice
	// once.
	nextCheckpointIdx int
}

// newInstTracer creates a tracer that can count the number of instructions.
func newInstTracer(
	runner *Runner,
	logDir string,
) *instTracer {
	var logger *log.Logger
	if logDir != "" {
		f, err := os.OpenFile(logDir,
			os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			log.Fatalf("failed to open log file: %v", err)
		}

		logger = log.New(f, "", log.LstdFlags|log.Lshortfile)
	}
	t := &instTracer{
		runner:       runner,
		logger:       logger,
		inflightInst: map[uint64]tracing.Task{},
	}
	return t
}

// newInstStopper with stop the execution after a given number of instructions
// is retired.
func newInstStopper(
	maxInst uint64,
	runner *Runner,
	logDir string,
) *instTracer {
	var logger *log.Logger
	if logDir != "" {
		f, err := os.OpenFile(logDir,
			os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			log.Fatalf("failed to open log file: %v", err)
		}

		logger = log.New(f, "", log.LstdFlags|log.Lshortfile)
	}
	t := &instTracer{
		maxCount:     maxInst,
		runner:       runner,
		logger:       logger,
		inflightInst: map[uint64]tracing.Task{},
	}

	timeLimitParam := global.GetInstance().GetTimeLimit()
	t.timeLimit = time.Duration(timeLimitParam) * time.Hour
	// t.timeLimit = time.Duration(timeLimitParam) * time.Second // for testing
	return t
}

func (t *instTracer) StartTask(task tracing.Task) {
	if task.Kind != "inst" {
		return
	}

	t.inflightInst[task.ID] = task
}

func (t *instTracer) StepTask(task tracing.Task) {
	// Do nothing
}

func (t *instTracer) EndTask(task tracing.Task) {

	_, found := t.inflightInst[task.ID]
	if !found {
		return
	}

	delete(t.inflightInst, task.ID)

	t.count++
	t.lastWGFinishTime = task.EndTime

	// Dump extra report snapshots at each -checkpoint percentage
	// (e.g. -checkpoint=25,50,75 saves <metric-file-name>_{25,50,75}%.csv).
	// A `for` (not `if`) handles the edge case where multiple checkpoints
	// fire on the same retired instruction with very small -max-inst.
	for t.maxCount > 0 &&
		t.nextCheckpointIdx < len(checkpointPercents) &&
		t.count >= t.maxCount*uint64(checkpointPercents[t.nextCheckpointIdx])/100 {
		percent := checkpointPercents[t.nextCheckpointIdx]
		t.nextCheckpointIdx++
		t.runner.reportStatsCheckpoint(percent)
	}

	// only for instStopper
	if t.maxCount > 0 &&
		t.count >= t.maxCount {
		terminated = true

		if t.logger != nil {
			duration := time.Since(t.runner.SimulationStartTime)
			t.logger.Printf("Duration: %v, Inst: %d", duration, t.count)

			if t.logger.Writer() != nil {
				if c, ok := t.logger.Writer().(io.Closer); ok {
					_ = c.Close()
				}
			}
		}
		atexit.Exit(0)
	}

	// duration := time.Since(t.runner.SimulationStartTime).Hours()
	// timeLimitInHours := t.timeLimit.Hours()

	// if timeLimitInHours != 0 &&
	// 	duration >= timeLimitInHours {
	// 	fmt.Println("Duration: ", duration, " hours, Time limit: ", timeLimitInHours, " hours.")

	// 	terminated = true

	// 	if t.logger != nil {
	// 		duration := time.Since(t.runner.SimulationStartTime)
	// 		t.logger.Printf("Duration: %v, Inst: %d", duration, t.count)

	// 		if t.logger.Writer() != nil {
	// 			if c, ok := t.logger.Writer().(io.Closer); ok {
	// 				_ = c.Close()
	// 			}
	// 		}
	// 	}
	// 	atexit.Exit(0)
	// }

	// works both for instStopper and instCountTracer
	if t.count%1000000 == 0 {
		if t.logger != nil {
			duration := time.Since(t.runner.SimulationStartTime)
			t.logger.Printf("Duration: %v, Inst: %d", duration, t.count)
		}
		t.runner.reportStats()
	}
}
