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
