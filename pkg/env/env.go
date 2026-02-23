package env

import (
	"os"
)

// Package env provides some utility functions to interact with the environment
// of the process.

// GetStringVal retrieves a string value from given environment envVar
// Returns default value if envVar is not set.
func GetStringVal(envVar string, defaultValue string) string {
	if val := os.Getenv(envVar); val != "" {
		return val
	}
	return defaultValue
}
