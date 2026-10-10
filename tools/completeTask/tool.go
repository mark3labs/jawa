package completeTask

import (
	kit "github.com/mark3labs/kit/pkg/kit"
	"jawa/internal/reporttools"
)

// Tool is a fail-closed discovery registration; managed setup binds runtime identity.
func Tool() kit.Tool { return reporttools.CompleteTask(reporttools.Binding{}) }
