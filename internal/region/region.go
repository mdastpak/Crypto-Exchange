// Package region names the market that a release serves (#272): Iran, or
// the international market outside it. The release reads it once, from
// deployment.region, and packages ask it through typed checks, such as
// Region.Iran, never by comparing strings.
//
// A feature that belongs to one market, such as the Shahkar check of
// Iranian mobile numbers, declares its regions in the configuration's
// validation, which refuses a release that switches it on outside them.
package region

import (
	"fmt"
	"slices"
	"strings"
)

// Region is the market of a release.
type Region string

// The regions.
const (
	// Iran serves users in Iran: Iranian identity checks, Iranian SMS,
	// toman (IRT) prices, and the Iranian exchanges.
	Iran Region = "ir"
	// International serves users outside Iran, without the Iran-only
	// features.
	International Region = "international"
)

// All are the regions, in the order of the documentation.
var All = []Region{Iran, International}

// Default is the region of a release that sets none: the releases so far
// serve Iran.
const Default = Iran

// Parse returns the region of a value of deployment.region.
func Parse(value string) (Region, error) {
	r := Region(strings.ToLower(strings.TrimSpace(value)))
	if !slices.Contains(All, r) {
		return "", fmt.Errorf("%q is not a region; the regions are ir and international", value)
	}
	return r, nil
}

// Iran reports a release for users in Iran.
func (r Region) Iran() bool { return r == Iran }

// International reports a release for users outside Iran.
func (r Region) International() bool { return r == International }

// In reports whether r is one of regions.
func (r Region) In(regions ...Region) bool { return slices.Contains(regions, r) }

// String returns the value of deployment.region.
func (r Region) String() string { return string(r) }
