package castai

import (
	"reflect"
	"time"
)

const (
	// UnknownActionType is returned by ClusterAction.GetType when the action is not recognized.
	UnknownActionType = "Unknown"
)

type ApiError struct {
	Message         string        `json:"message"`
	FieldViolations []interface{} `json:"fieldViolations"`
}

type User struct {
	ID       string `json:"id"`
	Username string `json:"username"`
}

type Component struct {
	Id            string   `json:"id"`
	Name          string   `json:"name"`
	HelmChart     string   `json:"helmChart"`
	Dependencies  []string `json:"dependencies"`
	LatestVersion string   `json:"latestVersion"`
}

type Cluster struct {
	// The cluster's ID.
	Id string `json:"id,omitempty"`
	// The name of the external cluster.
	Name string `json:"name,omitempty"`
	// The cluster's organization ID.
	OrganizationId string `json:"organization_id,omitempty"`
	// The date when cluster was registered.
	CreatedAt *time.Time `json:"created_at,omitempty"`
	// Current status of the cluster.
	Status string `json:"status,omitempty"`
	// The date agent snapshot was last received.
	AgentSnapshotReceivedAt *time.Time `json:"agent_snapshot_received_at,omitempty"`
	// Agent status.
	AgentStatus string `json:"agent_status,omitempty"`
}

type GetClusterActionsResponse struct {
	Items []*ClusterAction `json:"items"`
}

type AckClusterActionRequest struct {
	Error  *string `json:"error"`
	Target string  `json:"target"`
}

type ClusterAction struct {
	ID                   string                `json:"id"`
	ActionChartUpsert    *ActionChartUpsert    `json:"actionChartUpsert,omitempty"`
	ActionChartUninstall *ActionChartUninstall `json:"actionChartUninstall,omitempty"`
	ActionChartRollback  *ActionChartRollback  `json:"actionChartRollback,omitempty"`
	CreatedAt            time.Time             `json:"createdAt"`
	DoneAt               *time.Time            `json:"doneAt,omitempty"`
	Error                *string               `json:"error,omitempty"`
}

func (c *ClusterAction) Data() interface{} {
	if c.ActionChartUpsert != nil {
		return c.ActionChartUpsert
	}
	if c.ActionChartUninstall != nil {
		return c.ActionChartUninstall
	}
	if c.ActionChartRollback != nil {
		return c.ActionChartRollback
	}
	return nil
}

// IsValid checks if the action is OK to use.
// If this value is nil, most likely current version of cluster controller does not know about the action type and cannot execute it.
// It can also be a case of invalid data from server, but we cannot distinguish between the two at the moment.
func (c *ClusterAction) IsValid() bool {
	return c.Data() != nil
}

// GetType tries to deduct the type of the action based on its data. In case this fails, UnknownActionType is returned.
func (c *ClusterAction) GetType() string {
	if c.Data() != nil {
		return reflect.TypeOf(c.Data()).String()
	}
	return UnknownActionType
}

type ActionChartUpsert struct {
	Namespace            string            `json:"namespace"`
	ReleaseName          string            `json:"releaseName"`
	ValuesOverrides      map[string]string `json:"valuesOverrides,omitempty"`
	ChartSource          ChartSource       `json:"chartSource"`
	CreateNamespace      bool              `json:"createNamespace"`
	ResetThenReuseValues bool              `json:"resetThenReuseValues,omitempty"`
}

type ChartSource struct {
	RepoURL string `json:"repoUrl"`
	Name    string `json:"name"`
	Version string `json:"version"`
}

type ActionChartUninstall struct {
	Namespace   string `json:"namespace"`
	ReleaseName string `json:"releaseName"`
}

type ActionChartRollback struct {
	Namespace   string `json:"namespace"`
	ReleaseName string `json:"releaseName"`
	Version     string `json:"version"`
}
