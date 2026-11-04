package activity

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"agent-bridge/internal/formatter"
)

type StepStatus string

const (
	StepRunning StepStatus = "running"
	StepDone    StepStatus = "done"
	StepFailed  StepStatus = "failed"
)

type Step struct {
	ID        string
	Label     string
	Status    StepStatus
	Duration  time.Duration
	ExitCode  int
	Stderr    string
	StartTime time.Time
}

type Card struct {
	ProjectName string
	ModelName   string
	StartTime   time.Time
	EndTime     time.Time
	Steps       []Step
	Done        bool
	Verbosity   string // quiet, normal, verbose

	mu sync.Mutex
}

func NewCard(project, model, verbosity string) *Card {
	return &Card{
		ProjectName: project,
		ModelName:   model,
		StartTime:   time.Now(),
		Verbosity:   verbosity,
	}
}

func (c *Card) AddOrUpdateStep(id, label string, status StepStatus, duration time.Duration, exitCode int, stderr string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	for i, s := range c.Steps {
		if s.ID == id {
			c.Steps[i].Label = label
			c.Steps[i].Status = status
			c.Steps[i].Duration = duration
			c.Steps[i].ExitCode = exitCode
			c.Steps[i].Stderr = stderr
			return
		}
	}

	c.Steps = append(c.Steps, Step{
		ID:        id,
		Label:     label,
		Status:    status,
		Duration:  duration,
		ExitCode:  exitCode,
		Stderr:    stderr,
		StartTime: time.Now(),
	})
}

func (c *Card) MarkDone(summary string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.Done = true
	c.EndTime = time.Now()
}

// RenderHTML produces the Telegram HTML representation of the activity card.
func (c *Card) RenderHTML() string {
	c.mu.Lock()
	defer c.mu.Unlock()

	now := time.Now()
	var elapsed time.Duration
	if c.Done && !c.EndTime.IsZero() {
		elapsed = c.EndTime.Sub(c.StartTime)
	} else {
		elapsed = now.Sub(c.StartTime)
	}

	elapsedStr := formatDuration(elapsed)

	if c.Verbosity == "quiet" {
		if c.Done {
			return fmt.Sprintf("<b>%s</b> · Done in %s", formatter.EscapeHTML(c.ProjectName), elapsedStr)
		}
		return fmt.Sprintf("<b>%s</b> · running %s...", formatter.EscapeHTML(c.ProjectName), elapsedStr)
	}

	var sb strings.Builder
	// Header line
	statusLine := fmt.Sprintf("running %s", elapsedStr)
	if c.Done {
		statusLine = fmt.Sprintf("Done in %s", elapsedStr)
	}
	sb.WriteString(fmt.Sprintf("<b>%s</b>  ·  model: <code>%s</code>  ·  %s\n",
		formatter.EscapeHTML(c.ProjectName),
		formatter.EscapeHTML(c.ModelName),
		statusLine,
	))

	totalSteps := len(c.Steps)
	startIdx := 0
	if totalSteps > 12 {
		startIdx = totalSteps - 12
		sb.WriteString(fmt.Sprintf("<i>+%d earlier steps</i>\n", startIdx))
	}

	for i := startIdx; i < totalSteps; i++ {
		step := c.Steps[i]
		switch step.Status {
		case StepRunning:
			durSoFar := now.Sub(step.StartTime)
			sb.WriteString(fmt.Sprintf("▶ %s  (%s so far)\n",
				formatter.EscapeHTML(step.Label),
				formatDuration(durSoFar),
			))
		case StepDone:
			if step.Duration > 0 {
				sb.WriteString(fmt.Sprintf("✔ %s  (%s)\n",
					formatter.EscapeHTML(step.Label),
					formatDuration(step.Duration),
				))
			} else {
				sb.WriteString(fmt.Sprintf("✔ %s\n", formatter.EscapeHTML(step.Label)))
			}
		case StepFailed:
			durStr := ""
			if step.Duration > 0 {
				durStr = fmt.Sprintf("  (%s)", formatDuration(step.Duration))
			}
			sb.WriteString(fmt.Sprintf("✖ %s%s (exit %d)\n",
				formatter.EscapeHTML(step.Label),
				durStr,
				step.ExitCode,
			))
			if step.Stderr != "" {
				tail := formatter.FormatStderrTail(step.Stderr, 10)
				sb.WriteString(tail + "\n")
			}
		}
	}

	return strings.TrimRight(sb.String(), "\n")
}

func formatDuration(d time.Duration) string {
	d = d.Round(time.Second)
	m := d / time.Minute
	s := (d % time.Minute) / time.Second
	if m > 0 {
		return fmt.Sprintf("%dm %02ds", m, s)
	}
	return fmt.Sprintf("%ds", s)
}

// Debouncer ensures card updates are sent at most once every interval.
type Debouncer struct {
	interval time.Duration
	updateFn func(html string) error
	timer    *time.Timer
	pending  bool
	card     *Card
	lastSent time.Time
	mu       sync.Mutex
	stopCh   chan struct{}
}

func NewDebouncer(card *Card, interval time.Duration, updateFn func(html string) error) *Debouncer {
	return &Debouncer{
		interval: interval,
		updateFn: updateFn,
		card:     card,
		stopCh:   make(chan struct{}),
	}
}

func (d *Debouncer) Trigger() {
	d.mu.Lock()
	defer d.mu.Unlock()

	now := time.Now()
	if d.lastSent.IsZero() || now.Sub(d.lastSent) >= d.interval {
		d.lastSent = now
		_ = d.updateFn(d.card.RenderHTML())
		return
	}

	if !d.pending {
		d.pending = true
		remaining := d.interval - now.Sub(d.lastSent)
		d.timer = time.AfterFunc(remaining, func() {
			d.mu.Lock()
			d.pending = false
			d.lastSent = time.Now()
			d.mu.Unlock()
			_ = d.updateFn(d.card.RenderHTML())
		})
	}
}

// Flush stops any pending timer and immediately performs the update.
func (d *Debouncer) Flush() {
	d.mu.Lock()
	if d.timer != nil {
		d.timer.Stop()
		d.pending = false
	}
	d.lastSent = time.Now()
	d.mu.Unlock()

	_ = d.updateFn(d.card.RenderHTML())
}
