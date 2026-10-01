/*
Copyright 2026.

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

package main

import (
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	// to ensure that exec-entrypoint and run can make use of them.
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	admissionregv1 "k8s.io/api/admissionregistration/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/dynamic"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/metrics/filters"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	corev1alpha1 "github.com/o-haase/gojsop/api/v1alpha1"
	"github.com/o-haase/gojsop/internal/jsadmission"
	jsadmissionctrl "github.com/o-haase/gojsop/internal/jsadmission/controller"
	webhookv1alpha1 "github.com/o-haase/gojsop/internal/jsadmission/webhook/v1alpha1"
	"github.com/o-haase/gojsop/internal/jsengine/kubehost"
	jshookctrl "github.com/o-haase/gojsop/internal/jshook/controller"
	"github.com/o-haase/gojsop/internal/jshook/dispatcher"
	"github.com/o-haase/gojsop/internal/jsregistry"
	"github.com/o-haase/gojsop/internal/jssource"
	// +kubebuilder:scaffold:imports
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(admissionregv1.AddToScheme(scheme))

	utilruntime.Must(corev1alpha1.AddToScheme(scheme))
	// +kubebuilder:scaffold:scheme
}

// admissionServiceFromEnv reads the cluster-side webhook Service coordinates
// from env vars (set in the manager Deployment manifest). Falls back to the
// Kubebuilder-default Service name in the gojsop-system namespace.
func admissionServiceFromEnv() admissionregv1.ServiceReference {
	ns := os.Getenv("WEBHOOK_SERVICE_NAMESPACE")
	if ns == "" {
		ns = "gojsop-system"
	}
	name := os.Getenv("WEBHOOK_SERVICE_NAME")
	if name == "" {
		name = "gojsop-webhook-service"
	}
	port := int32(443)
	return admissionregv1.ServiceReference{Namespace: ns, Name: name, Port: &port}
}

// fileCABundleProvider reads the PEM CA bundle off disk every time it's
// called so cert-manager rotations propagate within one Sync window.
func fileCABundleProvider(certDir string) jsadmission.CABundleProvider {
	if certDir == "" {
		certDir = "/tmp/k8s-webhook-server/serving-certs"
	}
	caPath := filepath.Join(certDir, "ca.crt")
	tlsCrtPath := filepath.Join(certDir, "tls.crt")
	return func(ctx context.Context) ([]byte, error) {
		// Prefer ca.crt — cert-manager-issued Secrets carry the CA there.
		if data, err := os.ReadFile(caPath); err == nil {
			return data, nil
		}
		// Fallback: serving cert (works when the issuer chain == leaf).
		data, err := os.ReadFile(tlsCrtPath)
		if err != nil {
			return nil, fmt.Errorf("read ca bundle (tried %s and %s): %w", caPath, tlsCrtPath, err)
		}
		return data, nil
	}
}

// runFlags holds every CLI flag main() consumes. Centralizing them here keeps
// main() readable and makes it possible to test option construction.
type runFlags struct {
	metricsAddr          string
	metricsCertPath      string
	metricsCertName      string
	metricsCertKey       string
	webhookCertPath      string
	webhookCertName      string
	webhookCertKey       string
	probeAddr            string
	enableLeaderElection bool
	secureMetrics        bool
	enableHTTP2          bool
	zap                  zap.Options
}

// parseFlags binds every flag and parses os.Args.
func parseFlags() runFlags {
	f := runFlags{zap: zap.Options{Development: true}}
	flag.StringVar(&f.metricsAddr, "metrics-bind-address", "0", "The address the metrics endpoint binds to. "+
		"Use :8443 for HTTPS or :8080 for HTTP, or leave as 0 to disable the metrics service.")
	flag.StringVar(&f.probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.BoolVar(&f.enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")
	flag.BoolVar(&f.secureMetrics, "metrics-secure", true,
		"If set, the metrics endpoint is served securely via HTTPS. Use --metrics-secure=false to use HTTP instead.")
	flag.StringVar(&f.webhookCertPath, "webhook-cert-path", "", "The directory that contains the webhook certificate.")
	flag.StringVar(&f.webhookCertName, "webhook-cert-name", "tls.crt", "The name of the webhook certificate file.")
	flag.StringVar(&f.webhookCertKey, "webhook-cert-key", "tls.key", "The name of the webhook key file.")
	flag.StringVar(&f.metricsCertPath, "metrics-cert-path", "",
		"The directory that contains the metrics server certificate.")
	flag.StringVar(&f.metricsCertName, "metrics-cert-name", "tls.crt", "The name of the metrics server certificate file.")
	flag.StringVar(&f.metricsCertKey, "metrics-cert-key", "tls.key", "The name of the metrics server key file.")
	flag.BoolVar(&f.enableHTTP2, "enable-http2", false,
		"If set, HTTP/2 will be enabled for the metrics and webhook servers")
	f.zap.BindFlags(flag.CommandLine)
	flag.Parse()
	return f
}

// tlsOptions assembles the TLS option chain. HTTP/2 is disabled by default to
// avoid the Stream Cancellation and Rapid Reset CVEs:
//   - https://github.com/advisories/GHSA-qppj-fm5r-hxr3
//   - https://github.com/advisories/GHSA-4374-p667-p6c8
func tlsOptions(enableHTTP2 bool) []func(*tls.Config) {
	if enableHTTP2 {
		return nil
	}
	return []func(*tls.Config){func(c *tls.Config) {
		setupLog.Info("disabling http/2")
		c.NextProtos = []string{"http/1.1"}
	}}
}

// buildWebhookServer wires the webhook server with explicit certs when the
// caller passes --webhook-cert-path; otherwise controller-runtime generates
// self-signed certs (development only).
func buildWebhookServer(f runFlags, tlsOpts []func(*tls.Config)) webhook.Server {
	opts := webhook.Options{TLSOpts: tlsOpts}
	if f.webhookCertPath != "" {
		setupLog.Info("Initializing webhook certificate watcher using provided certificates",
			"webhook-cert-path", f.webhookCertPath, "webhook-cert-name", f.webhookCertName, "webhook-cert-key", f.webhookCertKey)
		opts.CertDir = f.webhookCertPath
		opts.CertName = f.webhookCertName
		opts.KeyName = f.webhookCertKey
	}
	return webhook.NewServer(opts)
}

// buildMetricsOptions configures the metrics server. SecureServing pulls in
// the AuthN/AuthZ filter so only authorized clients can scrape; cert-manager
// or controller-runtime self-signed certs back the TLS handshake.
//
// More info:
//   - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.23.1/pkg/metrics/server
//   - https://book.kubebuilder.io/reference/metrics.html
func buildMetricsOptions(f runFlags, tlsOpts []func(*tls.Config)) metricsserver.Options {
	opts := metricsserver.Options{
		BindAddress:   f.metricsAddr,
		SecureServing: f.secureMetrics,
		TLSOpts:       tlsOpts,
	}
	if f.secureMetrics {
		opts.FilterProvider = filters.WithAuthenticationAndAuthorization
	}
	if f.metricsCertPath != "" {
		setupLog.Info("Initializing metrics certificate watcher using provided certificates",
			"metrics-cert-path", f.metricsCertPath, "metrics-cert-name", f.metricsCertName, "metrics-cert-key", f.metricsCertKey)
		opts.CertDir = f.metricsCertPath
		opts.CertName = f.metricsCertName
		opts.KeyName = f.metricsCertKey
	}
	return opts
}

func main() {
	f := parseFlags()
	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&f.zap)))

	tlsOpts := tlsOptions(f.enableHTTP2)
	webhookServer := buildWebhookServer(f, tlsOpts)
	metricsServerOptions := buildMetricsOptions(f, tlsOpts)

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsServerOptions,
		WebhookServer:          webhookServer,
		HealthProbeBindAddress: f.probeAddr,
		LeaderElection:         f.enableLeaderElection,
		LeaderElectionID:       "82081b43.gojsop.io",
		// LeaderElectionReleaseOnCancel defines if the leader should step down voluntarily
		// when the Manager ends. This requires the binary to immediately end when the
		// Manager is stopped, otherwise, this setting is unsafe. Setting this significantly
		// speeds up voluntary leader transitions as the new leader don't have to wait
		// LeaseDuration time first.
		//
		// In the default scaffold provided, the program ends immediately after
		// the manager stops, so would be fine to enable this option. However,
		// if you are doing or is intended to do any operation such as perform cleanups
		// after the manager stops then its usage might be unsafe.
		// LeaderElectionReleaseOnCancel: true,
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	dyn, err := dynamic.NewForConfig(mgr.GetConfig())
	if err != nil {
		setupLog.Error(err, "unable to build dynamic client")
		os.Exit(1)
	}
	managerCtx := ctrl.SetupSignalHandler()
	registry := jsregistry.NewRegistry()
	// One factory per process. SharedFactory hands every reconcile a binder
	// over the same dynamic client + RESTMapper; ForHook returns the full
	// read+write kube.* surface, ForAdmission returns a read-only view (the
	// central VWC/MWC declare sideEffects: None and the apiserver is allowed
	// to retry/replay admission requests).
	kubeFactory := kubehost.NewSharedFactory(managerCtx, dyn, mgr.GetRESTMapper())
	disp := dispatcher.New(dyn, dispatcher.FromMetaMapper(mgr.GetRESTMapper()), registry)
	// Loader chain is shared between JSHook and JSAdmission so configMapRef
	// resolves the same way on both surfaces. The cache-backed manager
	// client gives us hot reads + watch-driven invalidation.
	loaderChain := jssource.NewChain(
		jssource.InlineLoader{},
		jssource.ConfigMapLoader{Reader: mgr.GetClient()},
	)
	if err := (&jshookctrl.JSHookReconciler{
		Client:       mgr.GetClient(),
		Scheme:       mgr.GetScheme(),
		Loader:       loaderChain,
		Registry:     registry,
		KubeHost:     kubeFactory,
		Dispatcher:   disp,
		SubscribeCtx: managerCtx,
		Recorder:     mgr.GetEventRecorder("jshook-controller"),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "JSHook")
		os.Exit(1)
	}

	// Admission webhook plumbing. The HTTP handler shares the runtime Registry
	// with JSHook; the central VWC/MWC are aggregated via the Registrar.
	admissionLog := ctrl.Log.WithName("admission")
	admissionServer := jsadmission.NewServer(registry, admissionLog)
	mgr.GetWebhookServer().Register(jsadmission.PathPrefixValidate, admissionServer.ValidateHandler())
	mgr.GetWebhookServer().Register(jsadmission.PathPrefixMutate, admissionServer.MutateHandler())

	caProvider := fileCABundleProvider(f.webhookCertPath)
	registrar := jsadmission.NewRegistrar(mgr.GetClient(), admissionServiceFromEnv(), caProvider, admissionLog)
	// Exclude the controller's own namespace from every policy so a broken
	// admission policy cannot prevent the manager pod from being re-created.
	// Other infra namespaces are the user's call.
	registrar.ExcludeNamespaces = []string{admissionServiceFromEnv().Namespace}
	registrar.Start(managerCtx)

	if err := (&jsadmissionctrl.JSAdmissionReconciler{
		Client:    mgr.GetClient(),
		Scheme:    mgr.GetScheme(),
		Loader:    loaderChain,
		Registry:  registry,
		KubeHost:  kubeFactory,
		Server:    admissionServer,
		Registrar: registrar,
		Recorder:  mgr.GetEventRecorder("jsadmission-controller"),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "JSAdmission")
		os.Exit(1)
	}
	// nolint:goconst
	if os.Getenv("ENABLE_WEBHOOKS") != "false" {
		if err := webhookv1alpha1.SetupJSAdmissionWebhookWithManager(mgr); err != nil {
			setupLog.Error(err, "unable to create webhook", "webhook", "JSAdmission")
			os.Exit(1)
		}
	}
	// +kubebuilder:scaffold:builder

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("starting manager")
	if err := mgr.Start(managerCtx); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}
