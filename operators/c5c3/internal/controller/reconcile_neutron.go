// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	c5c3v1alpha1 "github.com/c5c3/cobaltcore/operators/c5c3/api/v1alpha1"
)

// This file holds the Neutron projection. It carries the naming convention the
// network service's objects derive from; the sub-reconciler that projects the
// Neutron child follows in a later step.

// neutronNameSuffix is appended to the ControlPlane name to derive the name of
// the projected Neutron CR (and, through it, its credential and registration
// objects).
const neutronNameSuffix = "-neutron"

// neutronName returns the name of the Neutron CR the reconciler projects for the
// given ControlPlane (see neutronNameSuffix).
func neutronName(cp *c5c3v1alpha1.ControlPlane) string {
	return cp.Name + neutronNameSuffix
}
