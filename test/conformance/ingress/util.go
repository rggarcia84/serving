/*
Copyright 2019 The Knative Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package ingress

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	cryptorand "crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io/ioutil"
	"math/big"
	"math/rand"
	"net"
	"net/http"
	"net/http/httputil"
	"strconv"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrs "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/wait"
	pkgTest "knative.dev/pkg/test"
	"knative.dev/serving/pkg/apis/networking"
	"knative.dev/serving/pkg/apis/networking/v1alpha1"
	"knative.dev/serving/test"
	"knative.dev/serving/test/types"
	v1a1test "knative.dev/serving/test/v1alpha1"
)

var rootCAs = x509.NewCertPool()

// CreateRuntimeService creates a Kubernetes service that will respond to the protocol
// specified with the given portName.  It returns the service name, the port on
// which the service is listening, and a "cancel" function to clean up the
// created resources.
func CreateRuntimeService(t *testing.T, clients *test.Clients, portName string) (string, int, context.CancelFunc) {
	t.Helper()
	name := test.ObjectNameForTest(t)

	// Avoid zero, but pick a low port number.
	port := 50 + rand.Intn(50)
	t.Logf("[%s] Using port %d", name, port)

	// Pick a high port number.
	containerPort := 8000 + rand.Intn(100)
	t.Logf("[%s] Using containerPort %d", name, containerPort)

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: test.ServingNamespace,
			Labels: map[string]string{
				"test-pod": name,
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{
				Name:  "foo",
				Image: pkgTest.ImagePath("runtime"),
				Ports: []corev1.ContainerPort{{
					Name:          portName,
					ContainerPort: int32(containerPort),
				}},
				// This is needed by the runtime image we are using.
				Env: []corev1.EnvVar{{
					Name:  "PORT",
					Value: strconv.Itoa(containerPort),
				}},
				ReadinessProbe: &corev1.Probe{
					Handler: corev1.Handler{
						HTTPGet: &corev1.HTTPGetAction{
							Path: "/healthz",
							Port: intstr.FromInt(containerPort),
						},
					},
				},
			}},
		},
	}

	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: test.ServingNamespace,
			Labels: map[string]string{
				"test-pod": name,
			},
		},
		Spec: corev1.ServiceSpec{
			Type: "ClusterIP",
			Ports: []corev1.ServicePort{{
				Name:       portName,
				Port:       int32(port),
				TargetPort: intstr.FromInt(int(containerPort)),
			}},
			Selector: map[string]string{
				"test-pod": name,
			},
		},
	}

	return name, port, createPodAndService(t, clients, pod, svc)
}

// CreateTimeoutService creates a Kubernetes service that will respond to the protocol
// specified with the given portName.  It returns the service name, the port on
// which the service is listening, and a "cancel" function to clean up the
// created resources.
func CreateTimeoutService(t *testing.T, clients *test.Clients) (string, int, context.CancelFunc) {
	t.Helper()
	name := test.ObjectNameForTest(t)

	// Avoid zero, but pick a low port number.
	port := 50 + rand.Intn(50)
	t.Logf("[%s] Using port %d", name, port)

	// Pick a high port number.
	containerPort := 8000 + rand.Intn(100)
	t.Logf("[%s] Using containerPort %d", name, containerPort)

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: test.ServingNamespace,
			Labels: map[string]string{
				"test-pod": name,
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{
				Name:  "foo",
				Image: pkgTest.ImagePath("timeout"),
				Ports: []corev1.ContainerPort{{
					Name:          networking.ServicePortNameHTTP1,
					ContainerPort: int32(containerPort),
				}},
				// This is needed by the timeout image we are using.
				Env: []corev1.EnvVar{{
					Name:  "PORT",
					Value: strconv.Itoa(containerPort),
				}},
				ReadinessProbe: &corev1.Probe{
					Handler: corev1.Handler{
						HTTPGet: &corev1.HTTPGetAction{
							Port: intstr.FromInt(containerPort),
						},
					},
				},
			}},
		},
	}

	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: test.ServingNamespace,
			Labels: map[string]string{
				"test-pod": name,
			},
		},
		Spec: corev1.ServiceSpec{
			Type: "ClusterIP",
			Ports: []corev1.ServicePort{{
				Name:       networking.ServicePortNameHTTP1,
				Port:       int32(port),
				TargetPort: intstr.FromInt(int(containerPort)),
			}},
			Selector: map[string]string{
				"test-pod": name,
			},
		},
	}

	return name, port, createPodAndService(t, clients, pod, svc)
}

// CreateFlakyService creates a Kubernetes service where the backing pod will
// succeed only every Nth request.
func CreateFlakyService(t *testing.T, clients *test.Clients, period int) (string, int, context.CancelFunc) {
	t.Helper()
	name := test.ObjectNameForTest(t)

	// Avoid zero, but pick a low port number.
	port := 50 + rand.Intn(50)
	t.Logf("[%s] Using port %d", name, port)

	// Pick a high port number.
	containerPort := 8000 + rand.Intn(100)
	t.Logf("[%s] Using containerPort %d", name, containerPort)

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: test.ServingNamespace,
			Labels: map[string]string{
				"test-pod": name,
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{
				Name:  "foo",
				Image: pkgTest.ImagePath("flaky"),
				Ports: []corev1.ContainerPort{{
					Name:          networking.ServicePortNameHTTP1,
					ContainerPort: int32(containerPort),
				}},
				// This is needed by the runtime image we are using.
				Env: []corev1.EnvVar{{
					Name:  "PORT",
					Value: strconv.Itoa(containerPort),
				}, {
					Name:  "PERIOD",
					Value: strconv.Itoa(period),
				}},
				ReadinessProbe: &corev1.Probe{
					Handler: corev1.Handler{
						HTTPGet: &corev1.HTTPGetAction{
							Path: "/",
							Port: intstr.FromInt(containerPort),
						},
					},
				},
			}},
		},
	}

	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: test.ServingNamespace,
			Labels: map[string]string{
				"test-pod": name,
			},
		},
		Spec: corev1.ServiceSpec{
			Type: "ClusterIP",
			Ports: []corev1.ServicePort{{
				Name:       networking.ServicePortNameHTTP1,
				Port:       int32(port),
				TargetPort: intstr.FromInt(int(containerPort)),
			}},
			Selector: map[string]string{
				"test-pod": name,
			},
		},
	}

	return name, port, createPodAndService(t, clients, pod, svc)
}

// CreateWebsocketService creates a Kubernetes service that will upgrade the connection
// to use websockets and echo back the received messages with the provided suffix.
func CreateWebsocketService(t *testing.T, clients *test.Clients, suffix string) (string, int, context.CancelFunc) {
	t.Helper()
	name := test.ObjectNameForTest(t)

	// Avoid zero, but pick a low port number.
	port := 50 + rand.Intn(50)
	t.Logf("[%s] Using port %d", name, port)

	// Pick a high port number.
	containerPort := 8000 + rand.Intn(100)
	t.Logf("[%s] Using containerPort %d", name, containerPort)

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: test.ServingNamespace,
			Labels: map[string]string{
				"test-pod": name,
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{
				Name:  "foo",
				Image: pkgTest.ImagePath("wsserver"),
				Ports: []corev1.ContainerPort{{
					Name:          networking.ServicePortNameHTTP1,
					ContainerPort: int32(containerPort),
				}},
				// This is needed by the runtime image we are using.
				Env: []corev1.EnvVar{{
					Name:  "PORT",
					Value: strconv.Itoa(containerPort),
				}, {
					Name:  "SUFFIX",
					Value: suffix,
				}},
				ReadinessProbe: &corev1.Probe{
					Handler: corev1.Handler{
						HTTPGet: &corev1.HTTPGetAction{
							Path: "/",
							Port: intstr.FromInt(containerPort),
						},
					},
				},
			}},
		},
	}

	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: test.ServingNamespace,
			Labels: map[string]string{
				"test-pod": name,
			},
		},
		Spec: corev1.ServiceSpec{
			Type: "ClusterIP",
			Ports: []corev1.ServicePort{{
				Name:       networking.ServicePortNameHTTP1,
				Port:       int32(port),
				TargetPort: intstr.FromInt(int(containerPort)),
			}},
			Selector: map[string]string{
				"test-pod": name,
			},
		},
	}

	return name, port, createPodAndService(t, clients, pod, svc)
}

// CreateGRPCService creates a Kubernetes service that will upgrade the connection
// to use GRPC and echo back the received messages with the provided suffix.
func CreateGRPCService(t *testing.T, clients *test.Clients, suffix string) (string, int, context.CancelFunc) {
	t.Helper()
	name := test.ObjectNameForTest(t)

	// Avoid zero, but pick a low port number.
	port := 50 + rand.Intn(50)
	t.Logf("[%s] Using port %d", name, port)

	// Pick a high port number.
	containerPort := 8000 + rand.Intn(100)
	t.Logf("[%s] Using containerPort %d", name, containerPort)

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: test.ServingNamespace,
			Labels: map[string]string{
				"test-pod": name,
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{
				Name:  "foo",
				Image: pkgTest.ImagePath("grpc-ping"),
				Ports: []corev1.ContainerPort{{
					Name:          networking.ServicePortNameH2C,
					ContainerPort: int32(containerPort),
				}},
				// This is needed by the runtime image we are using.
				Env: []corev1.EnvVar{{
					Name:  "PORT",
					Value: strconv.Itoa(containerPort),
				}, {
					Name:  "SUFFIX",
					Value: suffix,
				}},
				ReadinessProbe: &corev1.Probe{
					Handler: corev1.Handler{
						TCPSocket: &corev1.TCPSocketAction{
							Port: intstr.FromInt(containerPort),
						},
					},
				},
			}},
		},
	}

	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: test.ServingNamespace,
			Labels: map[string]string{
				"test-pod": name,
			},
		},
		Spec: corev1.ServiceSpec{
			Type: "ClusterIP",
			Ports: []corev1.ServicePort{{
				Name:       networking.ServicePortNameH2C,
				Port:       int32(port),
				TargetPort: intstr.FromInt(int(containerPort)),
			}},
			Selector: map[string]string{
				"test-pod": name,
			},
		},
	}

	return name, port, createPodAndService(t, clients, pod, svc)
}

// createPodAndService is a helper for creating the pod and service resources, setting
// up their context.CancelFunc, and waiting for it to become ready.
func createPodAndService(t *testing.T, clients *test.Clients, pod *corev1.Pod, svc *corev1.Service) context.CancelFunc {
	t.Helper()

	test.CleanupOnInterrupt(func() { clients.KubeClient.Kube.CoreV1().Pods(pod.Namespace).Delete(pod.Name, &metav1.DeleteOptions{}) })
	pod, err := clients.KubeClient.Kube.CoreV1().Pods(pod.Namespace).Create(pod)
	if err != nil {
		t.Fatalf("Error creating Pod: %v", err)
	}
	cancel := func() {
		err := clients.KubeClient.Kube.CoreV1().Pods(pod.Namespace).Delete(pod.Name, &metav1.DeleteOptions{})
		if err != nil {
			t.Errorf("Error cleaning up Pod %s", pod.Name)
		}
	}

	test.CleanupOnInterrupt(func() {
		clients.KubeClient.Kube.CoreV1().Services(svc.Namespace).Delete(svc.Name, &metav1.DeleteOptions{})
	})
	svc, err = clients.KubeClient.Kube.CoreV1().Services(svc.Namespace).Create(svc)
	if err != nil {
		cancel()
		t.Fatalf("Error creating Service: %v", err)
	}

	// Wait for the Pod to show up in the Endpoints resource.
	waitErr := wait.PollImmediate(test.PollInterval, test.PollTimeout, func() (bool, error) {
		ep, err := clients.KubeClient.Kube.CoreV1().Endpoints(svc.Namespace).Get(svc.Name, metav1.GetOptions{})
		if apierrs.IsNotFound(err) {
			return false, nil
		} else if err != nil {
			return true, err
		}
		for _, subset := range ep.Subsets {
			if len(subset.Addresses) == 0 {
				return false, nil
			}
		}
		return len(ep.Subsets) > 0, nil
	})
	if waitErr != nil {
		cancel()
		t.Fatalf("Error waiting for Endpoints to contain a Pod IP: %v", waitErr)
	}

	return func() {
		err := clients.KubeClient.Kube.CoreV1().Services(svc.Namespace).Delete(svc.Name, &metav1.DeleteOptions{})
		if err != nil {
			t.Errorf("Error cleaning up Service %s: %v", svc.Name, err)
		}
		cancel()
	}
}

// CreateIngress creates a Knative Ingress resource
func CreateIngress(t *testing.T, clients *test.Clients, spec v1alpha1.IngressSpec) (*v1alpha1.Ingress, context.CancelFunc) {
	t.Helper()
	name := test.ObjectNameForTest(t)

	// Create a simple Ingress over the Service.
	ing := &v1alpha1.Ingress{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: test.ServingNamespace,
			Annotations: map[string]string{
				networking.IngressClassAnnotationKey: test.ServingFlags.IngressClass,
			},
		},
		Spec: spec,
	}
	test.CleanupOnInterrupt(func() { clients.NetworkingClient.Ingresses.Delete(ing.Name, &metav1.DeleteOptions{}) })
	ing, err := clients.NetworkingClient.Ingresses.Create(ing)
	if err != nil {
		t.Fatalf("Error creating Ingress: %v", err)
	}

	return ing, func() {
		err := clients.NetworkingClient.Ingresses.Delete(ing.Name, &metav1.DeleteOptions{})
		if err != nil {
			t.Errorf("Error cleaning up Ingress %s: %v", ing.Name, err)
		}
	}
}

func CreateIngressReadyDialContext(t *testing.T, clients *test.Clients, spec v1alpha1.IngressSpec) (*v1alpha1.Ingress, func(context.Context, string, string) (net.Conn, error), context.CancelFunc) {
	t.Helper()
	ing, cancel := CreateIngress(t, clients, spec)

	if err := v1a1test.WaitForIngressState(clients.NetworkingClient, ing.Name, v1a1test.IsIngressReady, t.Name()); err != nil {
		cancel()
		t.Fatalf("Error waiting for ingress state: %v", err)
	}
	ing, err := clients.NetworkingClient.Ingresses.Get(ing.Name, metav1.GetOptions{})
	if err != nil {
		cancel()
		t.Fatalf("Error getting Ingress: %v", err)
	}

	// Create a dialer based on the Ingress' public load balancer.
	return ing, CreateDialContext(t, ing, clients), cancel
}

func CreateIngressReady(t *testing.T, clients *test.Clients, spec v1alpha1.IngressSpec) (*v1alpha1.Ingress, *http.Client, context.CancelFunc) {
	t.Helper()

	// Create a client with a dialer based on the Ingress' public load balancer.
	ing, dialer, cancel := CreateIngressReadyDialContext(t, clients, spec)

	// TODO(mattmoor): How to get ing?
	var tlsConfig *tls.Config
	if len(ing.Spec.TLS) > 0 {
		// CAs are added to this as TLS secrets are created.
		tlsConfig = &tls.Config{
			RootCAs: rootCAs,
		}
	}

	return ing, &http.Client{
		Transport: &http.Transport{
			DialContext:     dialer,
			TLSClientConfig: tlsConfig,
		},
	}, cancel
}

// UpdateIngress updates a Knative Ingress resource
func UpdateIngress(t *testing.T, clients *test.Clients, name string, spec v1alpha1.IngressSpec) {
	t.Helper()

	ing, err := clients.NetworkingClient.Ingresses.Get(name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Error getting Ingress: %v", err)
	}

	ing.Spec = spec
	if _, err := clients.NetworkingClient.Ingresses.Update(ing); err != nil {
		t.Fatalf("Error updating Ingress: %v", err)
	}
}

func UpdateIngressReady(t *testing.T, clients *test.Clients, name string, spec v1alpha1.IngressSpec) {
	t.Helper()
	UpdateIngress(t, clients, name, spec)

	if err := v1a1test.WaitForIngressState(clients.NetworkingClient, name, v1a1test.IsIngressReady, t.Name()); err != nil {
		t.Fatalf("Error waiting for ingress state: %v", err)
	}
}

// This is based on https://golang.org/src/crypto/tls/generate_cert.go
func CreateTLSSecret(t *testing.T, clients *test.Clients, hosts []string) (string, context.CancelFunc) {
	t.Helper()

	priv, err := ecdsa.GenerateKey(elliptic.P256(), cryptorand.Reader)
	if err != nil {
		t.Fatalf("ecdsa.GenerateKey() = %v", err)
	}

	serialNumberLimit := new(big.Int).Lsh(big.NewInt(1), 128)
	serialNumber, err := cryptorand.Int(cryptorand.Reader, serialNumberLimit)
	if err != nil {
		t.Fatalf("Failed to generate serial number: %v", err)
	}

	template := x509.Certificate{
		SerialNumber: serialNumber,
		Subject: pkix.Name{
			Organization: []string{"Knative Ingress Conformance Testing"},
		},

		// Only let it live briefly.
		NotBefore: time.Now(),
		NotAfter:  time.Now().Add(5 * time.Minute),

		IsCA:                  true,
		KeyUsage:              x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,

		DNSNames: hosts,
	}

	derBytes, err := x509.CreateCertificate(cryptorand.Reader, &template, &template, &priv.PublicKey, priv)
	if err != nil {
		t.Fatalf("x509.CreateCertificate() = %v", err)
	}

	cert, err := x509.ParseCertificate(derBytes)
	if err != nil {
		t.Fatalf("ParseCertificate() = %v", err)
	}
	// Ideally we'd undo this in "cancel", but there doesn't
	// seem to be a mechanism to remove things from a pool.
	rootCAs.AddCert(cert)

	certPEM := &bytes.Buffer{}
	if err := pem.Encode(certPEM, &pem.Block{Type: "CERTIFICATE", Bytes: derBytes}); err != nil {
		t.Fatalf("Failed to write data to cert.pem: %s", err)
	}

	privBytes, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatalf("Unable to marshal private key: %v", err)
	}
	privPEM := &bytes.Buffer{}
	if err := pem.Encode(privPEM, &pem.Block{Type: "PRIVATE KEY", Bytes: privBytes}); err != nil {
		t.Fatalf("Failed to write data to key.pem: %s", err)
	}

	name := test.ObjectNameForTest(t)
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: test.ServingNamespace,
			Labels: map[string]string{
				"test-secret": name,
			},
		},
		Type: corev1.SecretTypeTLS,
		StringData: map[string]string{
			corev1.TLSCertKey:       certPEM.String(),
			corev1.TLSPrivateKeyKey: privPEM.String(),
		},
	}
	test.CleanupOnInterrupt(func() {
		clients.KubeClient.Kube.CoreV1().Secrets(secret.Namespace).Delete(secret.Name, &metav1.DeleteOptions{})
	})
	if _, err := clients.KubeClient.Kube.CoreV1().Secrets(secret.Namespace).Create(secret); err != nil {
		t.Fatalf("Error creating Secret: %v", err)
	}
	return name, func() {
		err := clients.KubeClient.Kube.CoreV1().Secrets(secret.Namespace).Delete(secret.Name, &metav1.DeleteOptions{})
		if err != nil {
			t.Errorf("Error cleaning up Secret %s: %v", secret.Name, err)
		}
	}
}

// CreateDialContext looks up the endpoint information to create a "dialer" for
// the provided Ingress' public ingress loas balancer.  It can be used to
// contact external-visibility services with an HTTP client via:
//
//	client := &http.Client{
//		Transport: &http.Transport{
//			DialContext: CreateDialContext(t, ing, clients),
//		},
//	}
func CreateDialContext(t *testing.T, ing *v1alpha1.Ingress, clients *test.Clients) func(context.Context, string, string) (net.Conn, error) {
	t.Helper()
	if ing.Status.PublicLoadBalancer == nil || len(ing.Status.PublicLoadBalancer.Ingress) < 1 {
		t.Fatal("Ingress does not have a public load balancer assigned.")
	}

	// TODO(mattmoor): I'm open to tricks that would let us cleanly test multiple
	// public load balancers or LBs with multiple ingresses (below), but want to
	// keep our simple tests simple, thus the [0]s...

	// We expect an ingress LB with the form foo.bar.svc.cluster.local (though
	// we aren't strictly sensitive to the suffix, this is just illustrative).
	internalDomain := ing.Status.PublicLoadBalancer.Ingress[0].DomainInternal
	parts := strings.SplitN(internalDomain, ".", 3)
	if len(parts) < 3 {
		t.Fatalf("Too few parts in internal domain: %s", internalDomain)
	}
	name, namespace := parts[0], parts[1]

	svc, err := clients.KubeClient.Kube.CoreV1().Services(namespace).Get(name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Unable to retrieve Kubernetes service %s/%s: %v", namespace, name, err)
	}
	if len(svc.Status.LoadBalancer.Ingress) < 1 {
		t.Fatal("Service does not have any ingresses (not type LoadBalancer?).")
	}
	ingress := svc.Status.LoadBalancer.Ingress[0]

	return func(_ context.Context, _ string, address string) (net.Conn, error) {
		_, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, err
		}
		if ingress.IP != "" {
			return net.Dial("tcp", ingress.IP+":"+port)
		}
		if ingress.Hostname != "" {
			return net.Dial("tcp", ingress.Hostname+":"+port)
		}
		return nil, errors.New("Service ingress does not contain dialing information.")
	}
}

type RequestOption func(*http.Request)

func RuntimeRequest(t *testing.T, client *http.Client, url string, opts ...RequestOption) *types.RuntimeInfo {
	t.Helper()

	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Errorf("Error creating Request: %v", err)
		return nil
	}

	for _, opt := range opts {
		opt(req)
	}

	resp, err := client.Do(req)
	if err != nil {
		t.Errorf("Error making GET request: %v", err)
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("Got non-OK status: %d", resp.StatusCode)
		DumpResponse(t, resp)
		return nil
	}

	b, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		t.Errorf("Unable to read response body: %v", err)
		DumpResponse(t, resp)
		return nil
	}
	ri := &types.RuntimeInfo{}
	if err := json.Unmarshal(b, ri); err != nil {
		t.Errorf("Unable to parse runtime image's response payload: %v", err)
		return nil
	}
	return ri
}

func DumpResponse(t *testing.T, resp *http.Response) {
	t.Helper()

	b, err := httputil.DumpResponse(resp, true)
	if err != nil {
		t.Errorf("Error dumping response: %v", err)
	}
	t.Log(string(b))
}
