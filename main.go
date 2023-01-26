/*
Copyright 2020 The Flux authors

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
	"fmt"
	"os"
	"time"

	flag "github.com/spf13/pflag"
	"helm.sh/helm/v3/pkg/kube"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	_ "k8s.io/client-go/plugin/pkg/client/auth/gcp"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	crtlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

	"github.com/fluxcd/pkg/runtime/acl"
	"github.com/fluxcd/pkg/runtime/client"
	helper "github.com/fluxcd/pkg/runtime/controller"
	"github.com/fluxcd/pkg/runtime/events"
	"github.com/fluxcd/pkg/runtime/leaderelection"
	"github.com/fluxcd/pkg/runtime/logger"
	"github.com/fluxcd/pkg/runtime/metrics"
	"github.com/fluxcd/pkg/runtime/pprof"
	"github.com/fluxcd/pkg/runtime/probes"
	sourcev1 "github.com/fluxcd/source-controller/api/v1beta2"
	corev1 "k8s.io/api/core/v1"

	v2 "github.com/fluxcd/helm-controller/api/v2beta1"
	"github.com/fluxcd/helm-controller/controllers"
	"github.com/fluxcd/helm-controller/internal/features"
	intkube "github.com/fluxcd/helm-controller/internal/kube"
	feathelper "github.com/fluxcd/pkg/runtime/features"
	// +kubebuilder:scaffold:imports
)

const controllerName = "helm-controller"

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))

	utilruntime.Must(sourcev1.AddToScheme(scheme))
	utilruntime.Must(v2.AddToScheme(scheme))
	// +kubebuilder:scaffold:scheme
}

func main() {
	var (
		metricsAddr             string
		eventsAddr              string
		healthAddr              string
		concurrent              int
		requeueDependency       time.Duration
		gracefulShutdownTimeout time.Duration
		watchAllNamespaces      bool
		httpRetry               int
		clientOptions           client.Options
		kubeConfigOpts          client.KubeConfigOptions
		featureGates            feathelper.FeatureGates
		logOptions              logger.Options
		aclOptions              acl.Options
		leaderElectionOptions   leaderelection.Options
		rateLimiterOptions      helper.RateLimiterOptions
	)

	flag.StringVar(&metricsAddr, "metrics-addr", ":8080", "The address the metric endpoint binds to.")
	flag.StringVar(&eventsAddr, "events-addr", "", "The address of the events receiver.")
	flag.StringVar(&healthAddr, "health-addr", ":9440", "The address the health endpoint binds to.")
	flag.IntVar(&concurrent, "concurrent", 4, "The number of concurrent HelmRelease reconciles.")
	flag.DurationVar(&requeueDependency, "requeue-dependency", 30*time.Second, "The interval at which failing dependencies are reevaluated.")
	flag.DurationVar(&gracefulShutdownTimeout, "graceful-shutdown-timeout", 600*time.Second, "The duration given to the reconciler to finish before forcibly stopping.")
	flag.BoolVar(&watchAllNamespaces, "watch-all-namespaces", true,
		"Watch for custom resources in all namespaces, if set to false it will only watch the runtime namespace.")
	flag.IntVar(&httpRetry, "http-retry", 9, "The maximum number of retries when failing to fetch artifacts over HTTP.")
	flag.StringVar(&intkube.DefaultServiceAccountName, "default-service-account", "", "Default service account used for impersonation.")
	clientOptions.BindFlags(flag.CommandLine)
	logOptions.BindFlags(flag.CommandLine)
	aclOptions.BindFlags(flag.CommandLine)
	leaderElectionOptions.BindFlags(flag.CommandLine)
	rateLimiterOptions.BindFlags(flag.CommandLine)
	kubeConfigOpts.BindFlags(flag.CommandLine)
	featureGates.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(logger.NewLogger(logOptions))

	err := featureGates.WithLogger(setupLog).
		SupportedFeatures(features.FeatureGates())
	if err != nil {
		setupLog.Error(err, "unable to load feature gates")
		os.Exit(1)
	}

	metricsRecorder := metrics.NewRecorder()
	crtlmetrics.Registry.MustRegister(metricsRecorder.Collectors()...)

	watchNamespace := ""
	if !watchAllNamespaces {
		watchNamespace = os.Getenv("RUNTIME_NAMESPACE")
	}

	disableCacheFor := []ctrlclient.Object{}
	shouldCache, err := features.Enabled(features.CacheSecretsAndConfigMaps)
	if err != nil {
		setupLog.Error(err, "unable to check feature gate CacheSecretsAndConfigMaps")
		os.Exit(1)
	}
	if !shouldCache {
		disableCacheFor = append(disableCacheFor, &corev1.Secret{}, &corev1.ConfigMap{})
	}

	// set the managedFields owner for resources reconciled from Helm charts
	kube.ManagedFieldsManager = controllerName

	restConfig := client.GetConfigOrDie(clientOptions)
	mgr, err := ctrl.NewManager(restConfig, ctrl.Options{
		Scheme:                        scheme,
		MetricsBindAddress:            metricsAddr,
		HealthProbeBindAddress:        healthAddr,
		Port:                          9443,
		LeaderElection:                leaderElectionOptions.Enable,
		LeaderElectionReleaseOnCancel: leaderElectionOptions.ReleaseOnCancel,
		LeaseDuration:                 &leaderElectionOptions.LeaseDuration,
		RenewDeadline:                 &leaderElectionOptions.RenewDeadline,
		RetryPeriod:                   &leaderElectionOptions.RetryPeriod,
		GracefulShutdownTimeout:       &gracefulShutdownTimeout,
		LeaderElectionID:              fmt.Sprintf("%s-leader-election", controllerName),
		Namespace:                     watchNamespace,
		Logger:                        ctrl.Log,
		ClientDisableCacheFor:         disableCacheFor,
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	probes.SetupChecks(mgr, setupLog)
	pprof.SetupHandlers(mgr, setupLog)

	var eventRecorder *events.Recorder
	if eventRecorder, err = events.NewRecorder(mgr, ctrl.Log, eventsAddr, controllerName); err != nil {
		setupLog.Error(err, "unable to create event recorder")
		os.Exit(1)
	}

	if err = (&controllers.HelmReleaseReconciler{
		Client:              mgr.GetClient(),
		Config:              mgr.GetConfig(),
		Scheme:              mgr.GetScheme(),
		EventRecorder:       eventRecorder,
		MetricsRecorder:     metricsRecorder,
		NoCrossNamespaceRef: aclOptions.NoCrossNamespaceRefs,
		ClientOpts:          clientOptions,
		KubeConfigOpts:      kubeConfigOpts,
	}).SetupWithManager(mgr, controllers.HelmReleaseReconcilerOptions{
		MaxConcurrentReconciles:   concurrent,
		DependencyRequeueInterval: requeueDependency,
		HTTPRetry:                 httpRetry,
		RateLimiter:               helper.GetRateLimiter(rateLimiterOptions),
	}); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", v2.HelmReleaseKind)
		os.Exit(1)
	}
	// +kubebuilder:scaffold:builder

	setupLog.Info("starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}
