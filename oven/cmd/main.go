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
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	// to ensure that exec-entrypoint and run can make use of them.
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/metrics/filters"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	servingv1alpha1 "github.com/durokukie/kukie-gpu-operator/oven/api/v1alpha1"
	"github.com/durokukie/kukie-gpu-operator/oven/internal/controller"
	// +kubebuilder:scaffold:imports
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))

	utilruntime.Must(servingv1alpha1.AddToScheme(scheme))
	// +kubebuilder:scaffold:scheme
}

// nolint:gocyclo
func main() {
	var metricsAddr string
	var metricsCertPath, metricsCertName, metricsCertKey string
	var webhookCertPath, webhookCertName, webhookCertKey string
	var webhookPort int
	var enableLeaderElection bool
	var probeAddr string
	var secureMetrics bool
	var enableHTTP2 bool
	var tlsOpts []func(*tls.Config)
	reconcilerConfig := bindConfigFlags(flag.CommandLine)
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
	flag.IntVar(&webhookPort, "webhook-port", 9443, "Port the webhook server listens on. "+
		"Defaults to 9443. Set -1 to disable the webhook server.")
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

	config, err := reconcilerConfig()
	if err != nil {
		setupLog.Error(err, "invalid flag")
		os.Exit(1)
	}

	// if the enable-http2 flag is false (the default), http/2 should be disabled
	// due to its vulnerabilities. More specifically, disabling http/2 will
	// prevent from being vulnerable to the HTTP/2 Stream Cancellation and
	// Rapid Reset CVEs. For more information see:
	// - https://github.com/advisories/GHSA-qppj-fm5r-hxr3
	// - https://github.com/advisories/GHSA-4374-p667-p6c8
	disableHTTP2 := func(c *tls.Config) {
		setupLog.Info("Disabling HTTP/2")
		c.NextProtos = []string{"http/1.1"}
	}

	if !enableHTTP2 {
		tlsOpts = append(tlsOpts, disableHTTP2)
	}

	// Initial webhook TLS options
	webhookTLSOpts := tlsOpts
	webhookServerOptions := webhook.Options{
		TLSOpts: webhookTLSOpts,
		Port:    webhookPort,
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
	// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.25.0/pkg/metrics/server
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
		// https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.25.0/pkg/metrics/filters#WithAuthenticationAndAuthorization
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
		Scheme: scheme,
		// A few reads per finished measurement. Cached, this would hold every ConfigMap of the cluster in the oven's memory.
		// Nodes likewise: one read per finished measurement and per look for a baseline to reuse. Caching them would also
		// need list and watch on every node.
		Client:                 client.Options{Cache: &client.CacheOptions{DisableFor: []client.Object{&corev1.ConfigMap{}, &corev1.Node{}}}},
		Metrics:                metricsServerOptions,
		WebhookServer:          webhookServer,
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "c29ebaea.kukie.io",
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
		setupLog.Error(err, "Failed to start manager")
		os.Exit(1)
	}

	clientset, err := kubernetes.NewForConfig(mgr.GetConfig())
	if err != nil {
		setupLog.Error(err, "Failed to create the client for Pod logs")
		os.Exit(1)
	}

	if err := (&controller.ServedModelReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
		Config: config,
		Logs:   controller.NewPodLogReader(clientset),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Failed to create controller", "controller", "servedmodel")
		os.Exit(1)
	}
	// +kubebuilder:scaffold:builder

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "Failed to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "Failed to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("Starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "Failed to run manager")
		os.Exit(1)
	}
}

// bindConfigFlags registers the flags that make up the reconciler's Config and returns what to call after parsing. The
// flags write into the Config itself, so there is no list of fields in between for a new flag to go missing from.
func bindConfigFlags(fs *flag.FlagSet) func() (controller.Config, error) {
	var c controller.Config
	var gpuNodeSelector, gpuTypeLabels string
	fs.StringVar(&c.DownloadImage, "download-image", "",
		"Image for the model download Job (needs python3 and the hf CLI). Empty uses spec.engine.image of each ServedModel.")
	fs.StringVar(&gpuNodeSelector, "gpu-node-selector", "",
		"Node selector for the download Job and the vLLM Deployment as key=value[,key=value]. "+
			"Example for kukie-gpu: kukie.io/accelerator=nvidia-l40s. Empty lets the scheduler pick.")
	fs.DurationVar(&c.DownloadDeadline, "download-deadline", controller.DefaultDownloadDeadline,
		"Time limit for one model download Job, retries included. Raise it for very large models.")
	fs.DurationVar(&c.Measure.Deadline, "measure-deadline", controller.DefaultMeasureDeadline,
		"Time limit for one measurement Job. It fills the GPU, so it must not run forever.")
	fs.StringVar(&c.Measure.Concurrency, "measure-concurrency", "",
		"Concurrent request steps of a measurement, space separated. Empty means 1 2 4 8 16 32 64.")
	fs.IntVar(&c.Measure.InputLen, "measure-input-tokens", 0, "Input tokens per measured request. 0 means 1000.")
	fs.IntVar(&c.Measure.OutputLen, "measure-output-tokens", 0, "Output tokens per measured request. 0 means 200.")
	fs.IntVar(&c.Measure.TTFTMillis, "measure-ttft-ms", 0, "SLA: time to first token p95 in ms. 0 means 1000.")
	fs.IntVar(&c.Measure.ITLMillis, "measure-itl-ms", 0, "SLA: inter-token latency p95 in ms. 0 means 50.")
	fs.DurationVar(&c.Measure.ReuseMaxAge, "measure-reuse-max-age", controller.DefaultMeasureReuseMaxAge,
		"A Ready ServedModel with no baseline for its spec takes the newest kept measurement result of its name under the "+
			"same model, image, GPUs, GPU type, engine args and measurement settings, if it is no older than this. 0 turns reuse off.")
	fs.StringVar(&gpuTypeLabels, "gpu-type-labels", strings.Join(controller.DefaultGPUTypeLabels, ","),
		"Node labels that name the GPU, in order of preference, comma separated. The first one the measured node has is "+
			"recorded with the result and must match for reuse.")
	fs.DurationVar(&c.MaxRuntime, "max-runtime", controller.DefaultMaxRuntime,
		"How long a ServedModel without spec.maxRuntime may run before it stops by itself. 0 means it never does.")
	fs.DurationVar(&c.NodeWaitTimeout, "node-wait-timeout", controller.DefaultNodeWaitTimeout,
		"How long an Unschedulable Pod is reported as Pending/WaitingForNode before Failed/Unschedulable. 0 means no waiting.")
	return func() (controller.Config, error) {
		if c.MaxRuntime < 0 || c.NodeWaitTimeout < 0 || c.Measure.ReuseMaxAge < 0 {
			return c, errors.New("--max-runtime, --node-wait-timeout and --measure-reuse-max-age must not be negative")
		}
		for label := range strings.SplitSeq(gpuTypeLabels, ",") {
			if label = strings.TrimSpace(label); label != "" {
				c.GPUTypeLabels = append(c.GPUTypeLabels, label)
			}
		}
		nodeSelector, err := parseKeyValues(gpuNodeSelector)
		if err != nil {
			return c, fmt.Errorf("--gpu-node-selector: %w", err)
		}
		c.GPUNodeSelector = nodeSelector
		return c, nil
	}
}

// parseKeyValues turns "k1=v1,k2=v2" into a map; empty input gives nil.
func parseKeyValues(s string) (map[string]string, error) {
	if strings.TrimSpace(s) == "" {
		return nil, nil
	}
	out := map[string]string{}
	for pair := range strings.SplitSeq(s, ",") {
		key, value, ok := strings.Cut(strings.TrimSpace(pair), "=")
		if !ok || key == "" {
			return nil, fmt.Errorf("expected key=value, got %q", pair)
		}
		out[key] = value
	}
	return out, nil
}
