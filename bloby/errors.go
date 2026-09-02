package bloby

import "errors"

var (
	// ErrNotFound reports that a registry object does not exist.
	ErrNotFound = errors.New("bloby object not found")
	// ErrAlreadyExists reports that a registry object conflicts with an existing row.
	ErrAlreadyExists = errors.New("bloby object already exists")
	// ErrConflict reports that an object cannot be changed because it is still referenced.
	ErrConflict = errors.New("bloby object conflict")
	// ErrInvalidInput reports invalid object metadata or an invalid lifecycle operation.
	ErrInvalidInput = errors.New("invalid bloby input")
)
