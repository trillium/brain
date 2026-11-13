package uow

import "github.com/steveyegge/beads/internal/storage/schema"

// Every database this package opens runs the brain migration series after
// upstream's main series (internal/storage/schema/brain_series.go).
func init() { schema.EnableBrainSeries() }
