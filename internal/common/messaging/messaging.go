// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package messaging

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"time"

	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/c5c3/cobaltcore/internal/common/conditions"
	"github.com/c5c3/cobaltcore/internal/common/multicluster"
	commonv1 "github.com/c5c3/cobaltcore/internal/common/types"
)

// RabbitmqClusterGVK is the GroupVersionKind of the RabbitmqCluster CR a managed
// MessagingSpec points at. It is addressed unstructured: this repository takes no
// dependency on the RabbitMQ Cluster Operator's Go module, so the kind needs no
// scheme registration. It lives here as the single definition every operator and
// the c5c3 infrastructure projection share.
var RabbitmqClusterGVK = schema.GroupVersionKind{
	Group:   "rabbitmq.com",
	Version: "v1beta1",
	Kind:    "RabbitmqCluster",
}

// TransportURLEnvVarName is the oslo.config env override key for
// [DEFAULT].transport_url. The OS_<GROUP>__<OPTION> form wins over the ConfigMap
// value at runtime, so service containers read the transport URL (which carries
// the broker password) from the derived Secret instead of from the ConfigMap.
const TransportURLEnvVarName = "OS_DEFAULT__TRANSPORT_URL"

// ReasonWaitingForMessagingCredentials is the readiness-condition reason set
// while the RabbitmqCluster, the default-user Secret or the brownfield Secret is
// missing a piece of the transport URL, so a derived Secret is never
// materialised with partial credentials.
const ReasonWaitingForMessagingCredentials = "WaitingForMessagingCredentials"

// defaultAMQPPort is the port an oslo.messaging consumer connects to when the
// transport URL names none.
const defaultAMQPPort int32 = 5672

// TransportURLSecretName returns the name of the derived transport-URL Secret for
// the given instance ("<instanceName>-transport-url").
func TransportURLSecretName(instanceName string) string {
	return instanceName + "-transport-url"
}

// TransportURLEnvVar returns the EnvVar that overrides [DEFAULT].transport_url by
// sourcing the URL from the derived <instanceName>-transport-url Secret produced
// by ReconcileTransportURLSecret. Every pod-spec builder that needs the message
// bus uses this helper so the override key and the Secret wiring stay in one
// place and the broker password never lands in the ConfigMap.
func TransportURLEnvVar(instanceName string) corev1.EnvVar {
	return corev1.EnvVar{
		Name: TransportURLEnvVarName,
		ValueFrom: &corev1.EnvVarSource{
			SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{
					Name: TransportURLSecretName(instanceName),
				},
				Key: commonv1.DefaultTransportURLSecretKey,
			},
		},
	}
}

// BuildTransportURL assembles the rabbit:// transport URL for the given broker
// credentials and endpoint and returns it together with the SHA-256 digest of
// that URL as a lowercase hex string. The deployment reconcilers stamp the digest
// into a pod-template annotation so a rotated broker credential rolls the
// Deployment: the URL is consumed via an env var, which only takes effect on a
// Pod restart.
//
// url.UserPassword percent-encodes the reserved characters "@", ":", "/" and "?"
// in the userinfo component per RFC 3986, which is what oslo.messaging's URL
// parser expects; a password containing any of them therefore survives the round
// trip. The path is the root vhost "/", the vhost the RabbitMQ Cluster Operator
// grants its default user.
func BuildTransportURL(username, password, host string, port int32) (transportURL, digest string) {
	busURL := &url.URL{
		Scheme: "rabbit",
		User:   url.UserPassword(username, password),
		Host:   net.JoinHostPort(host, strconv.Itoa(int(port))),
		Path:   "/",
	}
	transportURL = busURL.String()
	return transportURL, digestOf(transportURL)
}

// digestOf returns the SHA-256 of a transport URL as a lowercase hex string.
func digestOf(transportURL string) string {
	sum := sha256.Sum256([]byte(transportURL))
	return hex.EncodeToString(sum[:])
}

// RabbitSection returns the key/value map for the [oslo_messaging_rabbit] INI
// section. The three quorum options are always emitted: quorum queues are the
// only queue type RabbitMQ 4 offers for the durable and the transient queues
// alike, and the queue manager keeps their declaration in one place inside the
// consumer.
//
// A non-nil tls block adds ssl = true and ssl_ca_file pointing at caFilePath, the
// in-pod path the CA bundle is projected under. MessagingTLSSpec carries no mode,
// so its mere presence enables the client trust.
//
// The map never contains a transport_url key: the URL arrives exclusively through
// the TransportURLEnvVar env override, keeping the broker password out of the
// rendered ConfigMap.
func RabbitSection(tls *commonv1.MessagingTLSSpec, caFilePath string) map[string]string {
	section := map[string]string{
		"rabbit_quorum_queue":           "true",
		"rabbit_transient_quorum_queue": "true",
		"use_queue_manager":             "true",
	}
	if tls != nil {
		section["ssl"] = "true"
		section["ssl_ca_file"] = caFilePath
	}
	return section
}

// EgressPort returns the TCP port of a transport URL: the explicit URL port when
// present, otherwise 5672. A URL that does not parse, or one whose port is not a
// valid port number, falls back to 5672 as well, so the consumer's NetworkPolicy
// always carries a port rather than opening all of them.
func EgressPort(transportURL string) int32 {
	const maxPort = 65535
	u, err := url.Parse(transportURL)
	if err != nil {
		return defaultAMQPPort
	}
	if portStr := u.Port(); portStr != "" {
		// ParseInt with bitSize 32 bounds the result to int32; the explicit
		// range check rejects non-port values before the conversion.
		if n, perr := strconv.ParseInt(portStr, 10, 32); perr == nil && n > 0 && n <= maxPort {
			return int32(n)
		}
	}
	return defaultAMQPPort
}

// TransportURLSecretFlowParams carries everything ReconcileTransportURLSecret
// needs. The service-specific parts — the owner CR, the instance naming, the
// MessagingSpec and the condition vocabulary — are supplied by the caller; the
// read-assemble-materialise flow is identical across operators.
type TransportURLSecretFlowParams struct {
	Client client.Client
	Scheme *runtime.Scheme
	// Owner is the CR that owns the derived Secret.
	Owner client.Object
	// InstanceName drives the derived Secret name.
	InstanceName string
	// Namespace is the namespace of the upstream and the derived objects alike.
	Namespace string
	// Messaging is the shared MessagingSpec (RabbitmqCluster ref or brownfield
	// Secret ref).
	Messaging *commonv1.MessagingSpec
	// Conditions is the CR's condition slice, mutated in place.
	Conditions *[]metav1.Condition
	// Generation is stamped onto every condition the flow writes.
	Generation int64
	// ConditionType is the readiness condition the flow reports on (for example
	// "SecretsReady").
	ConditionType string
	// RequeueAfter is the polling interval while the broker credentials are not
	// yet available.
	RequeueAfter time.Duration
}

// ResolveTransportURL resolves the shared bus into the rabbit:// transport URL
// and that URL's SHA-256 digest, in managed mode (clusterRef) as well as in
// brownfield mode (secretRef). It writes nothing. Of the params it reads only
// Client, Namespace and Messaging; the remaining fields may stay zero.
//
// A non-empty waitMessage means an upstream object is not there yet: the
// RabbitmqCluster, its default-user Secret, one of the four keys that Secret
// carries, or the brownfield Secret and its key. The caller then requeues, and
// transportURL and digest are empty.
//
// It exists for a projector that hands a consumer in another namespace or on
// another cluster a brownfield secretRef, such as reconcileNeutronMessaging in
// the c5c3 operator: that caller reads the bus credentials here and writes the
// consumer's Secret itself, on the consumer's own client.
func ResolveTransportURL(ctx context.Context, p TransportURLSecretFlowParams) (transportURL, digest, waitMessage string, err error) {
	if p.Messaging == nil {
		return "", "", "", fmt.Errorf("messaging spec is nil")
	}

	switch {
	case p.Messaging.ClusterRef != nil:
		return resolveManaged(ctx, p)
	case p.Messaging.SecretRef != nil:
		return resolveBrownfield(ctx, p)
	default:
		// The CRD's XValidation rule enforces the XOR, so this is only reachable
		// when admission was bypassed. Fail rather than hand back an empty URL.
		return "", "", "", fmt.Errorf("messaging spec sets neither clusterRef nor secretRef")
	}
}

// ReconcileTransportURLSecret derives the rabbit:// transport URL from the
// referenced RabbitmqCluster (managed mode) or from the referenced brownfield
// Secret, and writes it to the derived <instance>-transport-url Secret under
// commonv1.DefaultTransportURLSecretKey. When an upstream object or one of its
// required keys is missing it sets the readiness condition False with reason
// WaitingForMessagingCredentials and requeues; it never writes a derived Secret
// with a partial URL.
//
// It returns the transport URL it materialised together with that URL's SHA-256
// digest. The digest lets the deployment step roll the Pods when the broker
// credential rotates without reading the Secret content itself; the URL saves the
// caller a read-back when it needs a property of the URL, such as the broker port
// its NetworkPolicy opens. Both are empty on the requeue/error paths where no
// derived Secret was materialised. It never sets the condition True: the caller's
// Secrets step reports readiness once all of its Secrets are in place.
func ReconcileTransportURLSecret(ctx context.Context, p TransportURLSecretFlowParams) (ctrl.Result, string, string, error) {
	transportURL, digest, waitMsg, err := ResolveTransportURL(ctx, p)
	if err != nil {
		return ctrl.Result{}, "", "", err
	}
	if waitMsg != "" {
		conditions.SetCondition(p.Conditions, metav1.Condition{
			Type:               p.ConditionType,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: p.Generation,
			Reason:             ReasonWaitingForMessagingCredentials,
			Message:            waitMsg,
		})
		return ctrl.Result{RequeueAfter: p.RequeueAfter}, "", "", nil
	}

	derivedKey := client.ObjectKey{
		Namespace: p.Namespace,
		Name:      TransportURLSecretName(p.InstanceName),
	}

	existing := &corev1.Secret{}
	err = p.Client.Get(ctx, derivedKey, existing)
	if apierrors.IsNotFound(err) {
		derived := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      derivedKey.Name,
				Namespace: derivedKey.Namespace,
			},
			Type: corev1.SecretTypeOpaque,
			Data: map[string][]byte{
				commonv1.DefaultTransportURLSecretKey: []byte(transportURL),
			},
		}
		if cerr := multicluster.Claim(p.Client, p.Scheme, p.Owner, derived); cerr != nil {
			return ctrl.Result{}, "", "", fmt.Errorf("setting owner reference on derived Secret %s/%s: %w",
				derived.Namespace, derived.Name, cerr)
		}
		if cerr := p.Client.Create(ctx, derived); cerr != nil {
			return ctrl.Result{}, "", "", fmt.Errorf("creating derived Secret %s/%s: %w",
				derived.Namespace, derived.Name, cerr)
		}
		return ctrl.Result{}, transportURL, digest, nil
	}
	if err != nil {
		return ctrl.Result{}, "", "", fmt.Errorf("getting derived Secret %s/%s: %w",
			derivedKey.Namespace, derivedKey.Name, err)
	}

	// The same claim the create branch makes, on the object that is actually
	// about to be rewritten. The derived name is <instance>-transport-url and the
	// instance is the owner's bare name, so two CRs of different kinds sharing a
	// name in one namespace derive the same Secret: without this the two
	// controllers would overwrite each other's URL on every pass, roll their
	// workloads on the flipped digest, and never surface an error. A Secret
	// somebody else owns is refused rather than adopted, and one nobody owns is
	// picked up so the broker password is reaped with the CR that wrote it.
	before := existing.DeepCopy()
	if cerr := multicluster.Claim(p.Client, p.Scheme, p.Owner, existing); cerr != nil {
		return ctrl.Result{}, "", "", fmt.Errorf("refusing to overwrite derived Secret %s/%s: %w",
			existing.Namespace, existing.Name, cerr)
	}

	// The derived Secret must contain exactly the one transport_url key; replace
	// Data wholesale on any drift (value change OR extra keys present).
	current, ok := existing.Data[commonv1.DefaultTransportURLSecretKey]
	drifted := len(existing.Data) != 1 || !ok || string(current) != transportURL
	if drifted {
		existing.Data = map[string][]byte{
			commonv1.DefaultTransportURLSecretKey: []byte(transportURL),
		}
	}

	// The claim mutates metadata in place, and the Update below is the only thing
	// that carries those mutations to the cluster, so a data-only gate would
	// recompute and throw away the ownership on every pass.
	if drifted || !apiequality.Semantic.DeepEqual(before.ObjectMeta, existing.ObjectMeta) {
		if uerr := p.Client.Update(ctx, existing); uerr != nil {
			return ctrl.Result{}, "", "", fmt.Errorf("updating derived Secret %s/%s: %w",
				existing.Namespace, existing.Name, uerr)
		}
	}

	return ctrl.Result{}, transportURL, digest, nil
}

// resolveManaged assembles the transport URL from the referenced RabbitmqCluster:
// its status.defaultUser.secretReference names the Secret the RabbitMQ Cluster
// Operator writes the default user's credentials and endpoint into.
//
// A non-empty waitMsg means an upstream piece is not there yet and the caller
// requeues; transportURL and digest are then empty and nothing was written.
func resolveManaged(ctx context.Context, p TransportURLSecretFlowParams) (transportURL, digest, waitMsg string, err error) {
	clusterKey := client.ObjectKey{
		Namespace: p.Namespace,
		Name:      p.Messaging.ClusterRef.Name,
	}

	cluster := &unstructured.Unstructured{}
	cluster.SetGroupVersionKind(RabbitmqClusterGVK)
	if gerr := p.Client.Get(ctx, clusterKey, cluster); gerr != nil {
		if apierrors.IsNotFound(gerr) {
			return "", "", fmt.Sprintf("RabbitmqCluster %s/%s not found",
				clusterKey.Namespace, clusterKey.Name), nil
		}
		return "", "", "", fmt.Errorf("reading RabbitmqCluster %s/%s: %w",
			clusterKey.Namespace, clusterKey.Name, gerr)
	}

	// The operator fills status.defaultUser only once the cluster's default user
	// exists, so an absent or empty name is the ordinary "not ready yet" shape
	// rather than an error.
	name := nestedString(cluster, "status", "defaultUser", "secretReference", "name")
	if name == "" {
		return "", "", fmt.Sprintf("RabbitmqCluster %s/%s has no status.defaultUser.secretReference.name yet",
			clusterKey.Namespace, clusterKey.Name), nil
	}
	// The operator reports the cluster's own namespace here; a status that omits
	// it names a Secret beside the cluster, which is p.Namespace.
	namespace := nestedString(cluster, "status", "defaultUser", "secretReference", "namespace")
	if namespace == "" {
		namespace = p.Namespace
	}
	secretKey := client.ObjectKey{Namespace: namespace, Name: name}

	secret := &corev1.Secret{}
	if gerr := p.Client.Get(ctx, secretKey, secret); gerr != nil {
		if apierrors.IsNotFound(gerr) {
			return "", "", fmt.Sprintf("default-user Secret %s/%s not found",
				secretKey.Namespace, secretKey.Name), nil
		}
		return "", "", "", fmt.Errorf("reading default-user Secret %s/%s: %w",
			secretKey.Namespace, secretKey.Name, gerr)
	}

	// All four halves come from the one Secret read above, so the credentials and
	// the endpoint are taken from a single, consistent object version.
	values := make(map[string]string, 4)
	for _, key := range []string{"username", "password", "host", "port"} {
		value := string(secret.Data[key])
		if value == "" {
			return "", "", fmt.Sprintf("default-user Secret %s/%s missing key %q",
				secretKey.Namespace, secretKey.Name, key), nil
		}
		values[key] = value
	}

	port, perr := strconv.ParseInt(values["port"], 10, 32)
	if perr != nil {
		return "", "", "", fmt.Errorf("parsing default-user port %q: %w", values["port"], perr)
	}

	transportURL, digest = BuildTransportURL(values["username"], values["password"], values["host"], int32(port))
	return transportURL, digest, "", nil
}

// nestedString reads a string field out of an unstructured object and returns ""
// when the field is absent or holds something other than a string. Both shapes
// mean the same thing to the managed flow: the status is not filled in yet.
func nestedString(obj *unstructured.Unstructured, fields ...string) string {
	value, found, err := unstructured.NestedString(obj.Object, fields...)
	if err != nil || !found {
		return ""
	}
	return value
}

// resolveBrownfield copies the complete transport URL out of the referenced
// Secret. The value is taken verbatim, so the vhost, the port and the query
// parameters the broker's administrator chose survive untouched; only the scheme
// is checked, because an amqp:// or amqps:// URL is an oslo.messaging driver the
// consumer is not configured for.
//
// A non-empty waitMsg means the Secret or its key is not there yet and the caller
// requeues; transportURL and digest are then empty and nothing was written.
func resolveBrownfield(ctx context.Context, p TransportURLSecretFlowParams) (transportURL, digest, waitMsg string, err error) {
	upstreamKey := client.ObjectKey{
		Namespace: p.Namespace,
		Name:      p.Messaging.SecretRef.Name,
	}
	dataKey := p.Messaging.SecretRef.Key
	if dataKey == "" {
		dataKey = commonv1.DefaultTransportURLSecretKey
	}

	secret := &corev1.Secret{}
	if gerr := p.Client.Get(ctx, upstreamKey, secret); gerr != nil {
		if apierrors.IsNotFound(gerr) {
			return "", "", fmt.Sprintf("brownfield transport-URL Secret %s/%s not found",
				upstreamKey.Namespace, upstreamKey.Name), nil
		}
		return "", "", "", fmt.Errorf("reading brownfield transport-URL Secret %s/%s: %w",
			upstreamKey.Namespace, upstreamKey.Name, gerr)
	}

	value := string(secret.Data[dataKey])
	if value == "" {
		return "", "", fmt.Sprintf("brownfield transport-URL Secret %s/%s missing key %q",
			upstreamKey.Namespace, upstreamKey.Name, dataKey), nil
	}

	// The reported scheme is empty when the value does not parse at all. The value
	// itself is never quoted into the error: it carries the broker password.
	parsed, perr := url.Parse(value)
	scheme := ""
	if perr == nil {
		scheme = parsed.Scheme
	}
	if scheme != "rabbit" {
		return "", "", "", fmt.Errorf("brownfield transport URL in Secret %s/%s key %q: scheme must be rabbit, got %q",
			upstreamKey.Namespace, upstreamKey.Name, dataKey, scheme)
	}

	return value, digestOf(value), "", nil
}
