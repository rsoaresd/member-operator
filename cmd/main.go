package main

import (
	"flag"
	"fmt"
	"os"
	goruntime "runtime"
	"time"

	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/codeready-toolchain/member-operator/controllers/idler"
	membercfgctrl "github.com/codeready-toolchain/member-operator/controllers/memberoperatorconfig"
	"github.com/codeready-toolchain/member-operator/controllers/memberstatus"
	"github.com/codeready-toolchain/member-operator/controllers/nstemplateset"
	"github.com/codeready-toolchain/member-operator/controllers/useraccount"
	"github.com/codeready-toolchain/member-operator/deploy"
	"github.com/codeready-toolchain/member-operator/pkg/apis"
	"github.com/codeready-toolchain/member-operator/pkg/klog"
	"github.com/codeready-toolchain/member-operator/pkg/metrics"
	"github.com/codeready-toolchain/member-operator/version"
	"github.com/codeready-toolchain/toolchain-common/controllers/toolchaincluster"
	"github.com/codeready-toolchain/toolchain-common/controllers/toolchainclustercache"
	"github.com/codeready-toolchain/toolchain-common/controllers/toolchainclusterresources"
	commonclient "github.com/codeready-toolchain/toolchain-common/pkg/client"
	"github.com/codeready-toolchain/toolchain-common/pkg/cluster"
	commonconfig "github.com/codeready-toolchain/toolchain-common/pkg/configuration"
	membercfg "github.com/codeready-toolchain/toolchain-common/pkg/configuration/memberoperatorconfig"
	"github.com/codeready-toolchain/toolchain-common/pkg/status"
	"go.uber.org/zap/zapcore"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	_ "k8s.io/client-go/plugin/pkg/client/auth"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
	"k8s.io/client-go/scale"
	klogv1 "k8s.io/klog"
	klogv2 "k8s.io/klog/v2"
	kmetrics "k8s.io/metrics/pkg/apis/metrics/v1beta1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
	runtimecluster "sigs.k8s.io/controller-runtime/pkg/cluster"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	//+kubebuilder:scaffold:imports
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))

	utilruntime.Must(apis.AddToScheme(scheme))
	//+kubebuilder:scaffold:scheme

	metrics.RegisterCustomMetrics()
}

func printVersion() {
	setupLog.Info(fmt.Sprintf("Operator Version: %s", version.Version))
	setupLog.Info(fmt.Sprintf("Go Version: %s", goruntime.Version()))
	setupLog.Info(fmt.Sprintf("Go OS/Arch: %s/%s", goruntime.GOOS, goruntime.GOARCH))
	setupLog.Info(fmt.Sprintf("Commit: %s", version.Commit))
	setupLog.Info(fmt.Sprintf("BuildTime: %s", version.BuildTime))
}

//+kubebuilder:rbac:groups=toolchain.dev.openshift.com,resources=toolchainclusters,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=toolchain.dev.openshift.com,resources=toolchainclusters/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=toolchain.dev.openshift.com,resources=toolchainclusters/finalizers,verbs=update

//+kubebuilder:rbac:groups=admissionregistration.k8s.io,resources=mutatingwebhookconfigurations;validatingwebhookconfigurations,verbs=get;list;watch;update;patch;create;delete
//+kubebuilder:rbac:groups=scheduling.k8s.io,resources=priorityclasses,verbs=get;list;watch;update;patch;create;delete
//+kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;update;patch;create;delete;deletecollection
//+kubebuilder:rbac:groups="",resources=secrets;services;services/finalizers;serviceaccounts,verbs=get;list;watch;update;patch;create;delete
//+kubebuilder:rbac:groups=apps,resources=deployments;deployments/finalizers;replicasets,verbs=get;list;watch;update;patch;create;delete
//+kubebuilder:rbac:groups=monitoring.coreos.com,resources=servicemonitors,verbs=get;list;watch;update;patch;create;delete

func main() {
	var metricsAddr string
	var enableLeaderElection bool
	var probeAddr string
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "The address the metric endpoint binds to.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")

	opts := zap.Options{
		Development: true,
		Encoder: zapcore.NewJSONEncoder(zapcore.EncoderConfig{
			TimeKey:        "ts",
			LevelKey:       "level",
			NameKey:        "logger",
			CallerKey:      "caller",
			FunctionKey:    zapcore.OmitKey,
			MessageKey:     "msg",
			StacktraceKey:  "stacktrace",
			LineEnding:     zapcore.DefaultLineEnding,
			EncodeLevel:    zapcore.LowercaseLevelEncoder,
			EncodeTime:     zapcore.ISO8601TimeEncoder,
			EncodeDuration: zapcore.SecondsDurationEncoder,
			EncodeCaller:   zapcore.ShortCallerEncoder,
		}),
	}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))
	// also set the client-go logger so we get the same JSON output
	klogv2.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	// see https://github.com/kubernetes/klog#coexisting-with-klogv2
	// BEGIN : hack to redirect klogv1 calls to klog v2
	// Tell klog NOT to log into STDERR. Otherwise, we risk
	// certain kinds of API errors getting logged into a directory not
	// available in a `FROM scratch` Docker container, causing us to abort
	var klogv1Flags flag.FlagSet
	klogv1.InitFlags(&klogv1Flags)
	if err := klogv1Flags.Set("logtostderr", "false"); err != nil { // By default klog v1 logs to stderr, switch that off
		setupLog.Error(err, "")
		os.Exit(1)
	}
	if err := klogv1Flags.Set("stderrthreshold", "FATAL"); err != nil { // stderrthreshold defaults to ERROR, so we don't get anything in stderr
		setupLog.Error(err, "")
		os.Exit(1)
	}
	klogv1.SetOutputBySeverity("INFO", klog.Writer{}) // tell klog v1 to use the custom writer
	// END : hack to redirect klogv1 calls to klog v2

	printVersion()

	// Get a config to talk to the apiserver
	cfg, err := config.GetConfig()
	if err != nil {
		setupLog.Error(err, "")
		os.Exit(1)
	}

	namespace, err := commonconfig.GetWatchNamespace()
	if err != nil {
		setupLog.Error(err, "failed to get watch namespace")
		os.Exit(1)
	}

	crtConfig, err := getCRTConfiguration(cfg)
	if err != nil {
		setupLog.Error(err, "failed to get toolchain configuration")
		os.Exit(1)
	}
	crtConfig.Print()

	discoveryClient, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		setupLog.Error(err, "failed to create discovery client")
		os.Exit(1)
	}

	// Webhook server will be created with default values (port 9443) as per doc - https://github.com/kubernetes-sigs/controller-runtime/blob/main/pkg/manager/manager.go#L244-L247
	// Cache Options design doc - https://github.com/kubernetes-sigs/controller-runtime/blob/main/designs/cache_options.md
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme: scheme,
		Metrics: metricsserver.Options{
			BindAddress: metricsAddr,
		},
		Cache:                  cache.Options{DefaultNamespaces: map[string]cache.Config{namespace: {}}},
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "2fc71baf.toolchain.member.operator",
		// disable caching of Node metrics in the client to avoid getting the following error every second
		// "failed to watch *v1beta1.NodeMetrics: the server does not allow this method on the requested resource (get nodes.metrics.k8s.io)"
		Client: client.Options{Cache: &client.CacheOptions{DisableFor: []client.Object{&kmetrics.NodeMetrics{}}}},
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	// create a new client with a cache that watches (as opposed to the standard client) resources in all namespaces.
	// This client should be used only for resources and kinds that are retrieved from other namespaces than the watched one.
	// This will help keeping a reasonable memory usage for this operator since the cache won't store all other namespace scoped
	// resources (secrets, etc.).
	allNamespacesCluster, err := runtimecluster.New(cfg, func(options *runtimecluster.Options) {
		options.Scheme = scheme
	})
	if err != nil {
		setupLog.Error(err, "unable to start allNamespaceCluster")
		os.Exit(1)
	}
	err = mgr.Add(allNamespacesCluster)
	if err != nil {
		setupLog.Error(err, "unable to add allNamespaceCluster to manager")
		os.Exit(1)
	}

	scalesClient, err := newScalesClient(cfg)
	if err != nil {
		setupLog.Error(err, "unable to create scales client")
		os.Exit(1)
	}

	restClient, err := newRestClient(cfg)
	if err != nil {
		setupLog.Error(err, "unable to create the REST client")
		os.Exit(1)
	}

	dynamicClient, err := dynamic.NewForConfig(cfg)
	if err != nil {
		setupLog.Error(err, "unable to create dynamic client")
		os.Exit(1)
	}

	// Setup all Controllers
	if err = (&toolchainclusterresources.Reconciler{
		Client:    mgr.GetClient(),
		Scheme:    mgr.GetScheme(),
		Templates: &deploy.ToolchainClusterTemplateFS,
	}).SetupWithManager(mgr, namespace); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "ToolchainClusterResources")
		os.Exit(1)
	}
	if err = toolchainclustercache.NewReconciler(
		mgr,
		namespace,
		crtConfig.ToolchainCluster().HealthCheckTimeout(),
	).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "ToolchainClusterCache")
		os.Exit(1)
	}

	if err := (&toolchaincluster.Reconciler{
		Client:     mgr.GetClient(),
		Scheme:     mgr.GetScheme(),
		RequeAfter: 10 * time.Second,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "ToolchainCluster")
		os.Exit(1)
	}
	if err := (&idler.Reconciler{
		Scheme:              mgr.GetScheme(),
		AllNamespacesClient: allNamespacesCluster.GetClient(),
		Client:              mgr.GetClient(),
		ScalesClient:        scalesClient,
		DynamicClient:       dynamicClient,
		DiscoveryClient:     discoveryClient,
		RestClient:          restClient,
		GetHostCluster:      cluster.GetHostCluster,
		Namespace:           namespace,
	}).SetupWithManager(mgr, allNamespacesCluster); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "Idler")
		os.Exit(1)
	}

	if err = (&memberstatus.Reconciler{
		Client:              mgr.GetClient(),
		Scheme:              mgr.GetScheme(),
		GetHostCluster:      cluster.GetHostCluster,
		AllNamespacesClient: allNamespacesCluster.GetClient(),
		VersionCheckManager: status.VersionCheckManager{GetGithubClientFunc: commonclient.NewGitHubClient},
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "MemberStatus")
		os.Exit(1)
	}
	if err = (nstemplateset.NewReconciler(&nstemplateset.APIClient{
		Client:              mgr.GetClient(),
		AllNamespacesClient: allNamespacesCluster.GetClient(),
		Scheme:              mgr.GetScheme(),
		GetHostCluster:      cluster.GetHostCluster,
	})).SetupWithManager(mgr, allNamespacesCluster, discoveryClient); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "NSTemplateSet")
		os.Exit(1)
	}
	if err = (&useraccount.Reconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "UserAccount")
		os.Exit(1)
	}
	if err = (&membercfgctrl.Reconciler{
		Client: mgr.GetClient(),
		Log:    ctrl.Log.WithName("controllers").WithName("MemberOperatorConfig"),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "MemberOperatorConfig")
		os.Exit(1)
	}
	//+kubebuilder:scaffold:builder

	stopChannel := ctrl.SetupSignalHandler()

	go func() {
		setupLog.Info("Waiting for cache to sync")
		if !mgr.GetCache().WaitForCacheSync(stopChannel) {
			setupLog.Error(fmt.Errorf("timed out waiting for main cache to sync"), "")
			os.Exit(1)
		}

		// create or update Member status during the operator deployment
		setupLog.Info("Creating/updating the MemberStatus resource")
		memberStatusName := memberstatus.MemberStatusName
		if err := memberstatus.CreateOrUpdateResources(stopChannel, mgr.GetClient(), namespace, memberStatusName); err != nil {
			setupLog.Error(err, "cannot create/update MemberStatus resource")
			os.Exit(1)
		}
		setupLog.Info("Created/updated the MemberStatus resource")
	}()

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("starting manager")
	if err := mgr.Start(stopChannel); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}

}

func newRestClient(config *rest.Config) (*rest.RESTClient, error) {
	httpClient, err := rest.HTTPClientFor(config)
	if err != nil {
		return nil, err
	}

	return rest.RESTClientForConfigAndClient(config, httpClient)
}

func newScalesClient(config *rest.Config) (scale.ScalesGetter, error) {
	c, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, err
	}
	// Polymorphic scale client
	groupResources, err := restmapper.GetAPIGroupResources(c.Discovery())
	if err != nil {
		return nil, err
	}
	mapper := restmapper.NewDiscoveryRESTMapper(groupResources)
	resolver := scale.NewDiscoveryScaleKindResolver(c.Discovery())
	return scale.NewForConfig(config, mapper, dynamic.LegacyAPIPathResolverFunc, resolver)
}

// getCRTConfiguration creates the client used for configuration and
// returns the loaded crt configuration
func getCRTConfiguration(config *rest.Config) (membercfg.Configuration, error) {
	// create client that will be used for retrieving the member operator config maps
	cl, err := client.New(config, client.Options{
		Scheme: scheme,
	})
	if err != nil {
		return membercfg.Configuration{}, err
	}

	return membercfg.GetConfiguration(cl)
}
