// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Package messaging resolves the shared commonv1.MessagingSpec into the
// rabbit:// transport URL an oslo.messaging consumer connects with, and renders
// the non-secret half of the [oslo_messaging_rabbit] config section.
//
// The spec has two modes. In managed mode (clusterRef) the helper reads the
// referenced RabbitmqCluster's status.defaultUser.secretReference, then the
// username, password, host and port from the Secret it names, and assembles the
// URL from them. In brownfield mode (secretRef) it copies a complete rabbit://
// URL out of the referenced Secret verbatim, so an operator-external broker
// keeps whatever vhost, port and credentials its administrator chose.
//
// Either mode materialises the same single object: the derived Secret
// "<instance>-transport-url" carrying the URL under the key
// commonv1.DefaultTransportURLSecretKey. That Secret is the only thing the
// package writes; the RabbitmqCluster and the upstream Secrets are read-only to
// it. Every read and write goes through the caller's Client in the caller's
// Namespace, which is the consumer's own client and namespace, so the package
// has no notion of a second cluster: a consumer that runs on another cluster or
// in another namespace than the bus is handed a brownfield secretRef by its
// projector, reconcileNeutronMessaging in operators/c5c3, which reads the bus
// through ResolveTransportURL and writes that brownfield Secret itself.
package messaging
