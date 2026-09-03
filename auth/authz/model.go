// Package authz evaluates permissions against an application-owned resource catalogue.
package authz

import "errors"

// Resource identifies an application capability protected by authorization.
type Resource string

// Access is the ordered permission granted for a resource.
type Access string

// Access levels are ordered None < View < Edit.
const (
	None Access = "none"
	View Access = "view"
	Edit Access = "edit"
)

// Authorization errors distinguish invalid policy from denied access.
var (
	ErrUnknownResource = errors.New("unknown authorization resource")
	ErrInvalidAccess   = errors.New("invalid authorization access")
	ErrForbidden       = errors.New("forbidden")
)

// Requirement is one resource permission required by an operation.
type Requirement struct {
	Resource Resource `json:"resource"`
	Access   Access   `json:"access"`
}

// Grant is one direct or inherited resource permission supplied by the application.
type Grant struct {
	Resource Resource
	Access   Access
}

func (access Access) level() int {
	switch access {
	case None:
		return 0
	case View:
		return 1
	case Edit:
		return 2
	default:
		return -1
	}
}
