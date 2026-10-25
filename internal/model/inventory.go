package model

// Inventory is what an agent reports about what runs on its host. It lives
// here, not in fleet, so the agent does not import the server's fleet code
// (and through it the rule engine and Alertmanager).
type Inventory struct {
	Processes []string `json:"processes"`
	Ports     []int    `json:"ports"`
	Images    []string `json:"images"`
}
