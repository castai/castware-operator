package config

import (
	"fmt"
	"strings"
)

// ComponentName is the operator's component identifier, used in log fields and
// the IngestLogs component path. Kept here (rather than internal/component) to
// avoid an import cycle for code that only needs the version string.
const ComponentName = "castware-operator"

type CastwareOperatorVersion struct {
	GitCommit string
	GitRef    string
	Version   string
}

func (c *CastwareOperatorVersion) String() string {
	return fmt.Sprintf("GitCommit=%q GitRef=%q Version=%q", c.GitCommit, c.GitRef, c.Version)
}

// LogVersion returns the version in the "<component>/<version>" form used by
// other CAST AI components (e.g. "castware-operator/v0.9.2"), matching the
// agent's "castai-agent/v0.159.4" convention. The leading "v" is preserved.
func (c *CastwareOperatorVersion) LogVersion() string {
	v := c.Version
	if !strings.HasPrefix(v, "v") {
		v = "v" + v
	}
	return fmt.Sprintf("%s/%s", ComponentName, v)
}
