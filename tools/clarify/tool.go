package clarify

import (
	kit "github.com/mark3labs/kit/pkg/kit"
	"jawa/internal/reporttools"
)

// Tool fails closed until managed setup binds trusted runtime identity.
func Tool() kit.Tool { return reporttools.Clarify(reporttools.Binding{}) }
