package models

// FleetGroup is the wire shape of one fleet group.
//
// Groups are the organising structure fleet-lite and kaufmann previously each
// kept a copy of; this service is becoming their single owner. Fleet data —
// VIN, plate, make — stays in the oracle; a group here is a name, a colour and
// a set of vehicle token ids.
type FleetGroup struct {
	// ID is <tenant-uuid>_<slug>, the R1 convention both source systems
	// already use. It is minted from the name at creation and never changes —
	// a rename keeps the id, because scope_group_ids, source_group_id and
	// published attestations all hold it.
	ID           string `json:"id"`
	TenantID     string `json:"tenantId"`
	Name         string `json:"name"`
	Color        string `json:"color"`
	VehicleCount int    `json:"vehicleCount"`
	CreatedAt    string `json:"createdAt"`
	UpdatedAt    string `json:"updatedAt"`
}

// FleetGroupVehicles is one group with its full member set — the shape of
// GET /v1/tenants/{id}/vehicle-groups, which exists so a caller can read a
// tenant's whole group structure in one request. Both apps need that view on
// their vehicle-list screens, and the P3 groups-diff needs it to compare a
// tenant without a per-group request storm.
type FleetGroupVehicles struct {
	FleetGroup
	TokenIDs []int64 `json:"tokenIds"`
}

// CreateGroupInput is the body of POST /v1/tenants/{id}/groups.
type CreateGroupInput struct {
	Name  string `json:"name"`
	Color string `json:"color"`
}

// UpdateGroupInput patches a group. Pointers so absent fields stay untouched.
// There is deliberately no way to change the id.
type UpdateGroupInput struct {
	Name  *string `json:"name,omitempty"`
	Color *string `json:"color,omitempty"`
}

// GroupVehiclesInput is the body of POST .../groups/{groupId}/vehicles.
type GroupVehiclesInput struct {
	TokenIDs []int64 `json:"tokenIds"`
}
