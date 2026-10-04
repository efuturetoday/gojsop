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
	"slices"
	"strings"
	"time"

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	// to ensure that exec-entrypoint and run can make use of them.
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	admissionregv1 "k8s.io/api/admissionregistration/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/dynamic"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/metrics/filters"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	corev1alpha1 "github.com/o-haase/gojsop/api/v1alpha1"
	"github.com/o-haase/gojsop/internal/jsaccess"
	"github.com/o-haase/gojsop/internal/jsadmission"
	jsadmissionctrl "github.com/o-haase/gojsop/internal/jsadmission/controller"
	webhookv1alpha1 "github.com/o-haase/gojsop/internal/jsadmission/webhook/v1alpha1"
	"github.com/o-haase/gojsop/internal/jsengine/kubehost"
	jshookctrl "github.com/o-haase/gojsop/internal/jshook/controller"
	"github.com/o-haase/gojsop/internal/jshook/dispatcher"
	"github.com/o-haase/gojsop/internal/jsregistry"
	"github.com/o-haase/gojsop/internal/jsrun"
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

// operatorNamespace is the namespace the operator runs in, home of the
// ServiceAccounts of hooks and policies.
func operatorNamespace() string {
	if ns := os.Getenv("POD_NAMESPACE"); ns != "" {
		return ns
	}
	return admissionServiceFromEnv().Namespace
}

// fileCABundleProvider reads the PEM CA bundle off disk every time it's
// called; the registrar polls it, so a cert-manager renewal reaches the
// webhook configurations within one CAResync interval.
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
	buildBackoffBase     time.Duration
	buildBackoffMax      time.Duration
	engineCacheDir       string
	maxConcurrentCalls   int
	admissionExclude     string
	zap                  zap.Options
}

// backoff is the retry spacing of a failed JS build, handed to both
// reconcilers.
func (f runFlags) backoff() jsrun.Backoff {
	return jsrun.Backoff{Base: f.buildBackoffBase, Max: f.buildBackoffMax}
}

// validate rejects flag values the operator cannot run with.
func (f runFlags) validate() error {
	if f.buildBackoffBase <= 0 {
		return fmt.Errorf("--build-backoff-base must be positive, got %s", f.buildBackoffBase)
	}
	if f.buildBackoffMax < f.buildBackoffBase {
		return fmt.Errorf("--build-backoff-max (%s) must not be below --build-backoff-base (%s)",
			f.buildBackoffMax, f.buildBackoffBase)
	}
	if f.maxConcurrentCalls <= 0 {
		return fmt.Errorf("--max-concurrent-calls must be positive, got %d", f.maxConcurrentCalls)
	}
	return nil
}

// excludedNamespaces are the namespaces no admission policy sees: the
// operator's own, so a broken policy cannot stop its pod from being
// re-created, plus --admission-exclude-namespaces.
// jsadmission.R15
func (f runFlags) excludedNamespaces(own string) []string {
	out := []string{own}
	for ns := range strings.SplitSeq(f.admissionExclude, ",") {
		ns = strings.TrimSpace(ns)
		if ns != "" && !slices.Contains(out, ns) {
			out = append(out, ns)
		}
	}
	return out
}

// parseFlags binds every flag and parses os.Args.
func parseFlags() runFlags {
	f, err := parseFlagSet(flag.CommandLine, os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	return f
}

// parseFlagSet binds every flag on fs and parses args.
func parseFlagSet(fs *flag.FlagSet, args []string) (runFlags, error) {
	f := runFlags{zap: zap.Options{Development: true}}
	fs.StringVar(&f.metricsAddr, "metrics-bind-address", "0", "The address the metrics endpoint binds to. "+
		"Use :8443 for HTTPS or :8080 for HTTP, or leave as 0 to disable the metrics service.")
	fs.StringVar(&f.probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	fs.BoolVar(&f.enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")
	fs.BoolVar(&f.secureMetrics, "metrics-secure", true,
		"If set, the metrics endpoint is served securely via HTTPS. Use --metrics-secure=false to use HTTP instead.")
	fs.StringVar(&f.webhookCertPath, "webhook-cert-path", "", "The directory that contains the webhook certificate.")
	fs.StringVar(&f.webhookCertName, "webhook-cert-name", "tls.crt", "The name of the webhook certificate file.")
	fs.StringVar(&f.webhookCertKey, "webhook-cert-key", "tls.key", "The name of the webhook key file.")
	fs.StringVar(&f.metricsCertPath, "metrics-cert-path", "",
		"The directory that contains the metrics server certificate.")
	fs.StringVar(&f.metricsCertName, "metrics-cert-name", "tls.crt", "The name of the metrics server certificate file.")
	fs.StringVar(&f.metricsCertKey, "metrics-cert-key", "tls.key", "The name of the metrics server key file.")
	fs.BoolVar(&f.enableHTTP2, "enable-http2", false,
		"If set, HTTP/2 will be enabled for the metrics and webhook servers")
	fs.DurationVar(&f.buildBackoffBase, "build-backoff-base", time.Second,
		"Base delay before a failed JS build is retried; doubles with every failed attempt in a row.")
	fs.DurationVar(&f.buildBackoffMax, "build-backoff-max", 5*time.Minute,
		"Upper bound of the delay between retries of a failed JS build.")
	fs.StringVar(&f.engineCacheDir, "engine-cache-dir", "",
		"Directory for the compiled machine code of the JS engine (engine.wasm). With it the first JS VM after a "+
			"restart starts in about 15 ms instead of 320 ms. Must be private to the operator, e.g. an emptyDir. "+
			"Empty keeps the cache in memory only.")
	fs.IntVar(&f.maxConcurrentCalls, "max-concurrent-calls", jsregistry.DefaultMaxConcurrentCalls,
		"Process-wide number of JS calls that may run at once, across every JSHook and JSAdmission. "+
			"Every call holds a VM of its own, so this bounds the memory of a burst.")
	fs.StringVar(&f.admissionExclude, "admission-exclude-namespaces", "kube-system,cert-manager",
		"Comma-separated namespaces no JSAdmission ever sees, so a broken policy or a down operator cannot "+
			"block them. The operator's own namespace is always excluded.")
	f.zap.BindFlags(fs)
	if err := fs.Parse(args); err != nil {
		return f, err
	}
	if err := f.validate(); err != nil {
		return f, err
	}
	return f, nil
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
	if err := jsregistry.ConfigureEngine(f.engineCacheDir); err != nil {
		setupLog.Error(err, "unable to start the JS engine", "engineCacheDir", f.engineCacheDir)
		os.Exit(1)
	}
	registry := jsregistry.New(jsregistry.Options{MaxConcurrentCalls: f.maxConcurrentCalls})
	// One factory per process. SharedFactory hands every reconcile a binder
	// over the same dynamic client + RESTMapper; ForHook returns the full
	// read+write kube.* surface, ForAdmission returns a read-only view (the
	// central VWC/MWC declare sideEffects: None and the apiserver is allowed
	// to retry/replay admission requests).
	// Every hook and policy runs as a ServiceAccount of its own with exactly
	// the rights it declares (kube-access.R10). The access manager reads and
	// writes without the cache: a cached client would watch every
	// ServiceAccount of the cluster, and the operator may only see its own
	// namespace.
	directClient, err := client.New(mgr.GetConfig(), client.Options{Scheme: mgr.GetScheme()})
	if err != nil {
		setupLog.Error(err, "unable to build direct client")
		os.Exit(1)
	}
	access := &jsaccess.Manager{
		Client:    directClient,
		Scheme:    mgr.GetScheme(),
		Namespace: operatorNamespace(),
		Config:    mgr.GetConfig(),
	}
	mgr.GetWebhookServer().Register(jsaccess.CheckPath, &webhook.Admission{
		Handler: &jsaccess.Checker{Review: jsaccess.ClientReviewer(directClient)},
	})
	kubeFactory := kubehost.NewSharedFactory(managerCtx, dyn, mgr.GetRESTMapper())
	kubeFactory.As = access.ClientFor
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
		Scripts:      registry,
		KubeHost:     kubeFactory,
		Access:       access,
		Dispatcher:   disp,
		SubscribeCtx: managerCtx,
		Recorder:     mgr.GetEventRecorder("jshook-controller"),
		Backoff:      f.backoff(),
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
	registrar.ExcludeNamespaces = f.excludedNamespaces(admissionServiceFromEnv().Namespace)
	// The Registrar writes the two central WebhookConfigurations, so it needs
	// exactly one writer. manager.RunnableFunc does not implement
	// LeaderElectionRunnable, which puts it in the leader-elected group — here
	// that default is what we want. jsadmission.R20
	if err := mgr.Add(manager.RunnableFunc(func(ctx context.Context) error {
		registrar.Start(ctx)
		<-ctx.Done()
		return nil
	})); err != nil {
		setupLog.Error(err, "unable to add registrar")
		os.Exit(1)
	}

	// The serving half of JSAdmission runs on every replica: a non-leader
	// that answers 404 would deny cluster-wide under failurePolicy: Fail.
	// jsadmission.R20
	if err := (&jsadmissionctrl.JSAdmissionServerReconciler{
		Client:          mgr.GetClient(),
		Loader:          loaderChain,
		Scripts:         registry,
		KubeHost:        kubeFactory,
		ServiceAccounts: true,
		Server:          admissionServer,
		Recorder:        mgr.GetEventRecorder("jsadmission-server"),
		Backoff:         f.backoff(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "JSAdmissionServer")
		os.Exit(1)
	}

	if err := (&jsadmissionctrl.JSAdmissionReconciler{
		Client:    mgr.GetClient(),
		Scheme:    mgr.GetScheme(),
		Loader:    loaderChain,
		Scripts:   registry,
		Registrar: registrar,
		Access:    access,
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
