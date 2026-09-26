package progress

import (
	"fmt"
	"strings"
	"sync/atomic"
	"time"
)

type Spinner struct {
	message      atomic.Value
	messageWidth int

	parts []string

	// value and stopped are shared with the goroutine NewSpinner starts: it
	// advances the frame and observes the stop flag while String renders from
	// the consumer's goroutine, so plain int/time.Time fields are a data race
	// the moment a pull renders while the spinner ticks (2026-09-26 audit).
	value   atomic.Int64
	stopped atomic.Bool

	ticker  *time.Ticker
	started time.Time
}

func NewSpinner(message string) *Spinner {
	s := &Spinner{
		parts: []string{
			"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏",
		},
		started: time.Now(),
	}
	s.SetMessage(message)
	go s.start()
	return s
}

func (s *Spinner) SetMessage(message string) {
	s.message.Store(message)
}

func (s *Spinner) String() string {
	var sb strings.Builder

	if message, ok := s.message.Load().(string); ok && len(message) > 0 {
		message := strings.TrimSpace(message)
		if s.messageWidth > 0 && len(message) > s.messageWidth {
			message = message[:s.messageWidth]
		}

		fmt.Fprintf(&sb, "%s", message)
		if padding := s.messageWidth - sb.Len(); padding > 0 {
			sb.WriteString(strings.Repeat(" ", padding))
		}

		sb.WriteString(" ")
	}

	if !s.stopped.Load() {
		spinner := s.parts[int(s.value.Load()%int64(len(s.parts)))]
		sb.WriteString(spinner)
		sb.WriteString(" ")
	}

	return sb.String()
}

func (s *Spinner) start() {
	s.ticker = time.NewTicker(100 * time.Millisecond)
	for range s.ticker.C {
		s.value.Add(1)
		if s.stopped.Load() {
			return
		}
	}
}

func (s *Spinner) Stop() {
	s.stopped.Store(true)
}
