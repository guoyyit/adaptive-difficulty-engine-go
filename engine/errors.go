package engine

import "errors"

// Engine-level errors.
var (
	// ErrConfigVersion is returned when a configuration update does not
	// carry a strictly greater version than the current one.
	ErrConfigVersion = errors.New("config version must be strictly greater than current version")
)
