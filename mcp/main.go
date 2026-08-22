// Copyright 2025 The 115driver Authors. All rights reserved.
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

package main

import (
	"time"

	"github.com/SheltonZhu/115driver/internal/mcpapp"
	"github.com/SheltonZhu/115driver/pkg/driver"
)

// main keeps the historical ./mcp source entry point working. The canonical
// install/release entry point is ./cmd/115driver-mcp-server.
func main() {
	mcpapp.Main()
}

// Compatibility wrappers keep the existing package-main tests focused on the
// same behavior while the implementation is shared by both command entry points.
func readConfigValue(configPath, profile, key string) string {
	return mcpapp.ReadConfigValue(configPath, profile, key)
}

func credentialFromCookie(cookie string) (*driver.Credential, error) {
	return mcpapp.CredentialFromCookie(cookie)
}

func validateOptions(urlUploadMaxBytes, downloadMaxBytes int64, downloadTimeout time.Duration) error {
	return mcpapp.ValidateOptions(urlUploadMaxBytes, downloadMaxBytes, downloadTimeout)
}
