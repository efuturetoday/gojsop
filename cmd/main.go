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

// nolint:gocyclo
func main() {
	var metricsAddr string
	var metricsCertPath, metricsCertName, metricsCertKey string
	var webhookCertPath, webhookCertName, webhookCertKey string
	var enableLeaderElection bool
	var probeAddr string
	var secureMetrics bool
	var enableHTTP2 bool
	var tlsOpts []func(*tls.Config)
	flag.StringVar(&metricsAddr, "metrics-bind-address", "0", "The address the metrics endpoint binds to. "+
		"Use :8443 for HTTPS or :8080 for HTTP, or leave as 0 to disable the metrics service.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")
	flag.BoolVar(&secureMetrics, "metrics-secure", true,
		"If set, the metrics endpoint is served securely via HTTPS. Use --metrics-secure=false to use HTTP instead.")
	flag.StringVar(&webhookCertPath, "webhook-cert-path", "", "The directory that contains the webhook certificate.")
	flag.StringVar(&webhookCertName, "webhook-cert-name", "tls.crt", "The name of the webhook certificate file.")
	flag.StringVar(&webhookCertKey, "webhook-cert-key", "tls.key", "The name of the webhook key file.")
	flag.StringVar(&metricsCertPath, "metrics-cert-path", "",
		"The directory that contains the metrics server certificate.")
	flag.StringVar(&metricsCertName, "metrics-cert-name", "tls.crt", "The name of the metrics server certificate file.")
	flag.StringVar(&metricsCertKey, "metrics-cert-key", "tls.key", "The name of the metrics server key file.")
	flag.BoolVar(&enableHTTP2, "enable-http2", false,
		"If set, HTTP/2 will be enabled for the metrics and webhook servers")
	opts := zap.Options{
		Development: true,
	}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	// if the enable-http2 flag is false (the default), http/2 should be disabled
	// due to its vulnerabilities. More specifically, disabling http/2 will
	// prevent from being vulnerable to the HTTP/2 Stream Cancellation and
	// Rapid Reset CVEs. For more information see:
	// - https://github.com/advisories/GHSA-qppj-fm5r-hxr3
	// - https://github.com/advisories/GHSA-4374-p667-p6c8
	disableHTTP2 := func(c *tls.Config) {
		setupLog.Info("disabling http/2")
		c.NextProtos = []string{"http/1.1"}
	}

	if !enableHTTP2 {
		tlsOpts = append(tlsOpts, disableHTTP2)
	}

	// Initial webhook TLS options
	webhookTLSOpts := tlsOpts
	webhookServerOptions := webhook.Options{
		TLSOpts: webhookTLSOpts,
	}

	if len(webhookCertPath) > 0 {
		setupLog.Info("Initializing webhook certificate watcher using provided certificates",
			"webhook-cert-path", webhookCertPath, "webhook-cert-name", webhookCertName, "webhook-cert-key", webhookCertKey)

		webhookServerOptions.CertDir = webhookCertPath
		webhookServerOptions.CertName = webhookCertName
		webhookServerOptions.KeyName = webhookCertKey
	}

	webhookServer := webhook.NewServer(webhookServerOptions)

	// Metrics endpoint is enabled in 'config/default/kustomization.yaml'. The Metrics options configure the server.
	// More info:
	// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.23.1/pkg/metrics/server
	// - https://book.kubebuilder.io/reference/metrics.html
	metricsServerOptions := metricsserver.Options{
		BindAddress:   metricsAddr,
		SecureServing: secureMetrics,
		TLSOpts:       tlsOpts,
	}

	if secureMetrics {
		// FilterProvider is used to protect the metrics endpoint with authn/authz.
		// These configurations ensure that only authorized users and service accounts
		// can access the metrics endpoint. The RBAC are configured in 'config/rbac/kustomization.yaml'. More info:
		// https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.23.1/pkg/metrics/filters#WithAuthenticationAndAuthorization
		metricsServerOptions.FilterProvider = filters.WithAuthenticationAndAuthorization
	}

	// If the certificate is not specified, controller-runtime will automatically
	// generate self-signed certificates for the metrics server. While convenient for development and testing,
	// this setup is not recommended for production.
	//
	// TODO(user): If you enable certManager, uncomment the following lines:
	// - [METRICS-WITH-CERTS] at config/default/kustomization.yaml to generate and use certificates
	// managed by cert-manager for the metrics server.
	// - [PROMETHEUS-WITH-CERTS] at config/prometheus/kustomization.yaml for TLS certification.
	if len(metricsCertPath) > 0 {
		setupLog.Info("Initializing metrics certificate watcher using provided certificates",
			"metrics-cert-path", metricsCertPath, "metrics-cert-name", metricsCertName, "metrics-cert-key", metricsCertKey)

		metricsServerOptions.CertDir = metricsCertPath
		metricsServerOptions.CertName = metricsCertName
		metricsServerOptions.KeyName = metricsCertKey
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsServerOptions,
		WebhookServer:          webhookServer,
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
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
	// JSHook VMs get the full read+write kube.* surface; JSAdmission VMs get
	// a read-only view because the central VWC/MWC declare sideEffects: None
	// and the apiserver is allowed to retry/replay admission requests.
	kubeFull := &kubehost.KubeHost{
		Ctx:    managerCtx,
		Dyn:    dyn,
		Mapper: mgr.GetRESTMapper(),
	}
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
		Binder:       kubeFull,
		Dispatcher:   disp,
		SubscribeCtx: managerCtx,
		Recorder:     mgr.GetEventRecorderFor("jshook-controller"),
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

	caProvider := fileCABundleProvider(webhookCertPath)
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
		Binder:    &kubehost.ReadOnlyKubeHost{KubeHost: kubeFull},
		Server:    admissionServer,
		Registrar: registrar,
		Recorder:  mgr.GetEventRecorderFor("jsadmission-controller"),
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
