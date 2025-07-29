package castai

import "time"

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

type ComponentActionResult struct {
	// The name of the component.
	Name string `json:"name,omitempty"`
	// The action that has been performed on the component.
	Action Action `json:"action,omitempty"`
	// The current version of the component installed on the cluster.
	// An empty string means the component was not installed prior the performing the action.
	CurrentVersion string `json:"current_version,omitempty"`
	// The version of the component targeted by the action.
	// An empty string means the installed component has been deleted while performing the action.
	Version string `json:"version,omitempty"`
	// The status of the component installed on the cluster.
	Status Status `json:"status,omitempty"`
	// The list of available images and their versions.
	ImageVersions map[string]string `json:"image_versions,omitempty"`
	// The Helm release name of the installed component.
	ReleaseName string `json:"release_name,omitempty"`
	// The verbose details of the outcome.
	Message string `json:"message,omitempty"`
}

// The action that can be performed on a CASTware component.
type Action string

const (
	// Unspecified action.
	Action_ACTION_UNSPECIFIED Action = "ACTION_UNSPECIFIED"
	// A fix component action.
	Action_FIX Action = "FIX"
	// An update component action.
	Action_UPDATE Action = "UPDATE"
	// An enable component action.
	Action_ENABLE Action = "ENABLE"
)

// The status of an installed CASTware component.
type Status string

const (
	// Unspecified status.
	Status_STATUS_UNSPECIFIED Status = "STATUS_UNSPECIFIED"
	// A component which is disconnected.
	Status_DISCONNECTED Status = "DISCONNECTED"
	// A component which needs an update.
	Status_UPDATE_NEEDED Status = "UPDATE_NEEDED"
	// A component which needs a user action.
	Status_ACTION_REQUIRED Status = "ACTION_REQUIRED"
	// A component which has an error.
	Status_ERROR Status = "ERROR"
	// A component which has an OK status.
	Status_OK Status = "OK"
)
