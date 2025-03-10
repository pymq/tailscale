// Copyright (c) 2020 Tailscale Inc & AUTHORS All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Code generated by the following command; DO NOT EDIT.
//   tailscale.com/cmd/cloner -type Resolver

package dnstype

import (
	"inet.af/netaddr"
)

// Clone makes a deep copy of Resolver.
// The result aliases no memory with the original.
func (src *Resolver) Clone() *Resolver {
	if src == nil {
		return nil
	}
	dst := new(Resolver)
	*dst = *src
	dst.BootstrapResolution = append(src.BootstrapResolution[:0:0], src.BootstrapResolution...)
	return dst
}

// A compilation failure here means this code must be regenerated, with the command at the top of this file.
var _ResolverNeedsRegeneration = Resolver(struct {
	Addr                string
	BootstrapResolution []netaddr.IP
}{})

// Clone duplicates src into dst and reports whether it succeeded.
// To succeed, <src, dst> must be of types <*T, *T> or <*T, **T>,
// where T is one of Resolver.
func Clone(dst, src interface{}) bool {
	switch src := src.(type) {
	case *Resolver:
		switch dst := dst.(type) {
		case *Resolver:
			*dst = *src.Clone()
			return true
		case **Resolver:
			*dst = src.Clone()
			return true
		}
	}
	return false
}
