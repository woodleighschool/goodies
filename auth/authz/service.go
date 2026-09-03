package authz

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// Store supplies raw direct and inherited grants for a principal.
type Store interface {
	Grants(context.Context, int64) ([]Grant, error)
}

// Service evaluates grants against an immutable resource catalogue.
type Service struct {
	store     Store
	resources map[Resource]struct{}
}

// NewService copies a nonempty catalogue of unique, nonblank resources.
func NewService(store Store, resources []Resource) (*Service, error) {
	if len(resources) == 0 {
		return nil, errors.New("authorization resource catalogue is empty")
	}
	catalogue := make(map[Resource]struct{}, len(resources))
	for _, resource := range resources {
		if strings.TrimSpace(string(resource)) == "" {
			return nil, fmt.Errorf("%w: resource is empty", ErrUnknownResource)
		}
		if _, exists := catalogue[resource]; exists {
			return nil, fmt.Errorf("duplicate authorization resource %q", resource)
		}
		catalogue[resource] = struct{}{}
	}
	return &Service{store: store, resources: catalogue}, nil
}

// EffectivePermissions returns every resource, taking the highest granted access.
// An invalid grant or store error discards the entire result.
func (s *Service) EffectivePermissions(ctx context.Context, principalID int64) (map[Resource]Access, error) {
	grants, err := s.store.Grants(ctx, principalID)
	if err != nil {
		return nil, fmt.Errorf("get authorization grants: %w", err)
	}
	permissions := make(map[Resource]Access, len(s.resources))
	for resource := range s.resources {
		permissions[resource] = None
	}
	for _, grant := range grants {
		if err := s.validate(grant.Resource, grant.Access); err != nil {
			return nil, err
		}
		if grant.Access.level() > permissions[grant.Resource].level() {
			permissions[grant.Resource] = grant.Access
		}
	}
	return permissions, nil
}

// HasAccess reports whether the principal has any non-none permission.
func (s *Service) HasAccess(ctx context.Context, principalID int64) (bool, error) {
	permissions, err := s.EffectivePermissions(ctx, principalID)
	if err != nil {
		return false, err
	}
	for _, access := range permissions {
		if access == View || access == Edit {
			return true, nil
		}
	}
	return false, nil
}

// Can reports whether the principal meets one resource requirement.
func (s *Service) Can(ctx context.Context, principalID int64, resource Resource, required Access) (bool, error) {
	return s.CanAll(ctx, principalID, Requirement{Resource: resource, Access: required})
}

// CanAll reports whether the principal meets every requirement.
// Empty requirements succeed only when the principal's grants can be loaded and validated.
func (s *Service) CanAll(ctx context.Context, principalID int64, requirements ...Requirement) (bool, error) {
	for _, requirement := range requirements {
		if err := s.validate(requirement.Resource, requirement.Access); err != nil {
			return false, err
		}
	}
	permissions, err := s.EffectivePermissions(ctx, principalID)
	if err != nil {
		return false, err
	}
	for _, requirement := range requirements {
		if permissions[requirement.Resource].level() < requirement.Access.level() {
			return false, nil
		}
	}
	return true, nil
}

func (s *Service) validate(resource Resource, access Access) error {
	if _, exists := s.resources[resource]; !exists {
		return fmt.Errorf("%w: %q", ErrUnknownResource, resource)
	}
	if access.level() < 0 {
		return fmt.Errorf("%w: %q", ErrInvalidAccess, access)
	}
	return nil
}

// Authorizer checks one or more required permissions.
type Authorizer interface {
	Can(ctx context.Context, userID int64, resource Resource, required Access) (bool, error)
	CanAll(ctx context.Context, userID int64, requirements ...Requirement) (bool, error)
}
