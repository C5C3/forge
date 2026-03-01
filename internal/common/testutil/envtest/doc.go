// Package envtest provides helpers for spinning up a real Kubernetes API server
// and etcd via controller-runtime's envtest machinery in integration tests.
//
// The primary entry point is SetupEnvTest, which starts the environment, registers
// common schemes, and returns a ready-to-use rest.Config and client.Client together
// with a teardown function that must be deferred by the caller.
//
// Fake CRD manifests bundled with this package are automatically included in every
// environment so that third-party custom resources (ESO, cert-manager, MariaDB,
// Memcached, RabbitMQ) are available without additional configuration.
package envtest
