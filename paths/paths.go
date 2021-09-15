// Copyright (c) 2020 Tailscale Inc & AUTHORS All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package paths returns platform and user-specific default paths to
// Tailscale files and directories.
package paths

import (
	"os"
	"path/filepath"
	"runtime"
	"sync/atomic"

	"tailscale.com/version/distro"
)

// AppSharedDir is a string set by the iOS or Android app on start
// containing a directory we can read/write in.
var AppSharedDir atomic.Value

// DefaultTailscaledSocket returns the path to the tailscaled Unix socket
// or the empty string if there's no reasonable default.
func DefaultTailscaledSocket() string {
	if runtime.GOOS == "windows" {
		return ""
	}
	if runtime.GOOS == "darwin" {
		return "/var/run/tailscaled.socket"
	}
	if distro.Get() == distro.Synology {
		// TODO(maisem): be smarter about this. We can parse /etc/VERSION.
		const dsm6Sock = "/var/packages/Tailscale/etc/tailscaled.sock"
		const dsm7Sock = "/var/packages/Tailscale/var/tailscaled.sock"
		if fi, err := os.Stat(dsm6Sock); err == nil && !fi.IsDir() {
			return dsm6Sock
		}
		if fi, err := os.Stat(dsm7Sock); err == nil && !fi.IsDir() {
			return dsm7Sock
		}
	}
	if fi, err := os.Stat("/var/run"); err == nil && fi.IsDir() {
		return "/var/run/tailscale/tailscaled.sock"
	}
	return "tailscaled.sock"
}

var stateFileFunc func() string

// DefaultTailscaledStateFile returns the default path to the
// tailscaled state file, or the empty string if there's no reasonable
// default value.
func DefaultTailscaledStateFile() string {
	if f := stateFileFunc; f != nil {
		return f()
	}
	if runtime.GOOS == "windows" {
		programData := filepath.Join(os.Getenv("ProgramData"), "Tailscale", "server-state.conf")
		if _, err := os.Stat(programData); err == nil {
			return programData
		}

		// This is where Tailscale 1.14 and earlier stored the server-state. (Issue 2856)
		// We still recognize it as a fallback.
		localAppData := filepath.Join(os.Getenv("LocalAppData"), "Tailscale", "server-state.conf")
		if _, err := os.Stat(localAppData); err == nil {
			return localAppData
		}

		// Neither already exists, so use ProgramData.
		return programData
	}
	return ""
}
