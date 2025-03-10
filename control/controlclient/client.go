// Copyright (c) 2020 Tailscale Inc & AUTHORS All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package controlclient implements the client for the Tailscale
// control plane.
//
// It handles authentication, port picking, and collects the local
// network configuration.
package controlclient

import (
	"context"

	"tailscale.com/tailcfg"
)

type LoginFlags int

const (
	LoginDefault     = LoginFlags(0)
	LoginInteractive = LoginFlags(1 << iota) // force user login and key refresh
)

// Client represents a client connection to the control server.
// Currently this is done through a pair of polling https requests in
// the Auto client, but that might change eventually.
type Client interface {
	// SetStatusFunc provides a callback to call when control sends us
	// a message.
	SetStatusFunc(func(Status))
	// Shutdown closes this session, which should not be used any further
	// afterwards.
	Shutdown()
	// Login begins an interactive or non-interactive login process.
	// Client will eventually call the Status callback with either a
	// LoginFinished flag (on success) or an auth URL (if further
	// interaction is needed).
	Login(*tailcfg.Oauth2Token, LoginFlags)
	// StartLogout starts an asynchronous logout process.
	// When it finishes, the Status callback will be called while
	// AuthCantContinue()==true.
	StartLogout()
	// Logout starts a synchronous logout process. It doesn't return
	// until the logout operation has been completed.
	Logout(context.Context) error
	// SetPaused pauses or unpauses the controlclient activity as much
	// as possible, without losing its internal state, to minimize
	// unnecessary network activity.
	// TODO: It might be better to simply shutdown the controlclient and
	// make a new one when it's time to unpause.
	SetPaused(bool)
	// AuthCantContinue returns whether authentication is blocked. If it
	// is, you either need to visit the auth URL (previously sent in a
	// Status callback) or call the Login function appropriately.
	// TODO: this probably belongs in the Status itself instead.
	AuthCantContinue() bool
	// SetHostinfo changes the Hostinfo structure that will be sent in
	// subsequent node registration requests.
	// TODO: a server-side change would let us simply upload this
	// in a separate http request. It has nothing to do with the rest of
	// the state machine.
	SetHostinfo(*tailcfg.Hostinfo)
	// SetNetinfo changes the NetIinfo structure that will be sent in
	// subsequent node registration requests.
	// TODO: a server-side change would let us simply upload this
	// in a separate http request. It has nothing to do with the rest of
	// the state machine.
	SetNetInfo(*tailcfg.NetInfo)
	// UpdateEndpoints changes the Endpoint structure that will be sent
	// in subsequent node registration requests.
	// The localPort field is unused except for integration tests in another repo.
	// TODO: a server-side change would let us simply upload this
	// in a separate http request. It has nothing to do with the rest of
	// the state machine.
	UpdateEndpoints(localPort uint16, endpoints []tailcfg.Endpoint)
	// SetDNS sends the SetDNSRequest request to the control plane server,
	// requesting a DNS record be created or updated.
	SetDNS(context.Context, *tailcfg.SetDNSRequest) error
}
