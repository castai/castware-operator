package config

import "fmt"

type CastwareOperatorVersion struct {
	GitCommit string
	GitRef    string
	Version   string
}

func (c *CastwareOperatorVersion) String() string {
	return fmt.Sprintf("GitCommit=%q GitRef=%q Version=%q", c.GitCommit, c.GitRef, c.Version)
}
