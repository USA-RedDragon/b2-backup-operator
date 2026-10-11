package main

import (
	"crypto/tls"
	"flag"
	"fmt"
	"os"
	"strings"

	_ "k8s.io/client-go/plugin/pkg/client/auth"

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

	"k8s.io/apimachinery/pkg/types"

	b2backupv1 "github.com/USA-RedDragon/b2-backup-operator/api/v1"
	"github.com/USA-RedDragon/b2-backup-operator/internal/b2"
	"github.com/USA-RedDragon/b2-backup-operator/internal/backup"
	"github.com/USA-RedDragon/b2-backup-operator/internal/controller"
	// +kubebuilder:scaffold:imports
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func parseNamespacedName(s, defaultNS string) (types.NamespacedName, error) {
	parts := strings.Split(s, "/")
	wellFormed := len(parts) == 1 || len(parts) == 2
	for _, part := range parts {
		wellFormed = wellFormed && part != ""
	}
	if !wellFormed {
		return types.NamespacedName{}, fmt.Errorf("expected name or namespace/name, got %q", s)
	}
	if len(parts) == 1 {
		if defaultNS == "" {
			return types.NamespacedName{}, fmt.Errorf(
				"%q has no namespace and POD_NAMESPACE is unset; use namespace/name", s)
		}
		return types.NamespacedName{Namespace: defaultNS, Name: s}, nil
	}
	return types.NamespacedName{Namespace: parts[0], Name: parts[1]}, nil
}

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))

	utilruntime.Must(b2backupv1.AddToScheme(scheme))
	// +kubebuilder:scaffold:scheme
}

func main() {
	var metricsAddr string
	var metricsCertPath, metricsCertName, metricsCertKey string
	var webhookCertPath, webhookCertName, webhookCertKey string
	var enableLeaderElection bool
	var probeAddr string
	var secureMetrics bool
	var enableHTTP2 bool
	var tlsOpts []func(*tls.Config)

	var credentialsSecret, b2AuthURL, backupImage, cacheStorageClass string
	defaults := backup.DefaultConfig()
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
		Development: false,
	}
	flag.StringVar(&credentialsSecret, "b2-credentials-secret", "b2-credentials",
		"Secret holding the default B2 application key under "+b2backupv1.CredentialsKeyID+" and "+
			b2backupv1.CredentialsKey+", as \"name\" or \"namespace/name\". A bare name resolves in "+
			"the operator's own namespace. A Repository may override this with spec.credentialsSecretRef.")
	flag.StringVar(&b2AuthURL, "b2-auth-url", "",
		"Override where b2_authorize_account is called. Empty uses "+b2.DefaultAuthURL+".")
	flag.StringVar(&backupImage, "backup-image", defaults.Image,
		"Image used for backup and init jobs. It must provide restic, ts and dumpdb.")
	flag.StringVar(&cacheStorageClass, "cache-storage-class", "",
		"StorageClass for restic cache volumes. Empty uses the cluster's default StorageClass. "+
			"Cache claims request ReadWriteOnce unless spec.cache.accessMode says otherwise.")

	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	disableHTTP2 := func(c *tls.Config) {
		setupLog.Info("Disabling HTTP/2")
		c.NextProtos = []string{"http/1.1"}
	}

	if !enableHTTP2 {
		tlsOpts = append(tlsOpts, disableHTTP2)
	}

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

	metricsServerOptions := metricsserver.Options{
		BindAddress:   metricsAddr,
		SecureServing: secureMetrics,
		TLSOpts:       tlsOpts,
	}

	if secureMetrics {
		metricsServerOptions.FilterProvider = filters.WithAuthenticationAndAuthorization
	}

	if len(metricsCertPath) > 0 {
		setupLog.Info("Initializing metrics certificate watcher using provided certificates",
			"metrics-cert-path", metricsCertPath, "metrics-cert-name", metricsCertName, "metrics-cert-key", metricsCertKey)

		metricsServerOptions.CertDir = metricsCertPath
		metricsServerOptions.CertName = metricsCertName
		metricsServerOptions.KeyName = metricsCertKey
	}

	credentials, err := parseNamespacedName(credentialsSecret, os.Getenv("POD_NAMESPACE"))
	if err != nil {
		setupLog.Error(err, "Invalid --b2-credentials-secret")
		os.Exit(1)
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsServerOptions,
		Cache:                  controller.CacheOptions(),
		Client:                 client.Options{Cache: controller.ClientCacheOptions()},
		WebhookServer:          webhookServer,
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "e283bf03.mcswain.dev",
	})
	if err != nil {
		setupLog.Error(err, "Failed to start manager")
		os.Exit(1)
	}

	clientset, err := kubernetes.NewForConfig(mgr.GetConfig())
	if err != nil {
		setupLog.Error(err, "Failed to create clientset")
		os.Exit(1)
	}
	ctx := ctrl.SetupSignalHandler()
	coreEvents := controller.NewCoreEvents(ctx, clientset, mgr.GetScheme())
	defer coreEvents.Shutdown()

	if err := (&controller.RepositoryReconciler{
		Client:                   mgr.GetClient(),
		Scheme:                   mgr.GetScheme(),
		Recorder:                 coreEvents.Recorder("repository-controller"),
		DefaultCredentialsSecret: credentials,
		BackupImage:              backupImage,
		AuthURL:                  b2AuthURL,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Failed to create controller", "controller", "repository")
		os.Exit(1)
	}
	if err := (&controller.ScheduledBackupReconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		Recorder: coreEvents.Recorder("scheduledbackup-controller"),
		Config: backup.Config{
			Image:             backupImage,
			CacheStorageClass: cacheStorageClass,
		},
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Failed to create controller", "controller", "scheduledbackup")
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
	if err := mgr.Start(ctx); err != nil {
		setupLog.Error(err, "Failed to run manager")
		coreEvents.Shutdown()
		os.Exit(1)
	}
}
