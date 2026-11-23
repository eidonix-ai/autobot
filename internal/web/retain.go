package web

import (
	"context"
	"log"
	"time"
)

// Retain deletes run details that have outlived their keeping (see store.PruneRuns): once at
// start, then every day, until ctx ends.
func (s *Server) Retain(ctx context.Context) {
	t := time.NewTicker(24 * time.Hour)
	defer t.Stop()
	for {
		func() {
			defer survive("run retention", nil)
			p, err := s.st.PruneRuns(time.Now())
			if err != nil {
				log.Printf("run retention: %v", err)
			} else if p.TestLogs+p.StepOutputs+p.Conversations > 0 {
				log.Printf("run retention: deleted %d test-run logs and %d conversations, shortened %d step outputs", p.TestLogs, p.Conversations, p.StepOutputs)
			}
		}()
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
