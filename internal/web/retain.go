package web

import (
	"context"
	"github.com/eidonix-ai/autobot/internal/index"
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
			p, err := s.st.PruneRuns(time.Now(), func(ws int64) bool { return index.Ready(s.st, ws) })
			if err != nil {
				log.Printf("run retention: %v", err)
			} else if p.TestLogs+p.StepOutputs+p.Conversations+p.Pages+p.DryCases > 0 {
				log.Printf("run retention: deleted %d test-run logs, %d conversations, %d published pages and %d dry-run case files, shortened %d step outputs", p.TestLogs, p.Conversations, p.Pages, p.DryCases, p.StepOutputs)
			}
		}()
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
