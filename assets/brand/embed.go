// Package brandassets exposes the canonical visual assets that must travel
// inside an LCTK executable instead of depending on an external installation.
package brandassets

import _ "embed"

var (
	// FaviconICO is the complete multi-frame browser icon served by the local
	// OAuth page. Embedding keeps that security-sensitive page self-contained.
	//
	//go:embed web/favicon.ico
	FaviconICO []byte

	// LockupOnLightSVG is the trusted static horizontal signature rendered by
	// the local OAuth page. Dynamic project and client values remain escaped by
	// html/template and are never inserted into this asset.
	//
	//go:embed source/lctk-lockup-on-light.svg
	LockupOnLightSVG []byte
)
