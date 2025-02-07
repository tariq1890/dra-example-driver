/*
 * Copyright 2023 The Kubernetes Authors.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/pprof"
	"os"
	"path"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"github.com/spf13/viper"

	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/informers"
	coreclientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/component-base/cli"
	cliflag "k8s.io/component-base/cli/flag"
	"k8s.io/component-base/featuregate"
	"k8s.io/component-base/logs"
	logsapi "k8s.io/component-base/logs/api/v1"
	"k8s.io/component-base/metrics/legacyregistry"
	"k8s.io/component-base/term"
	"k8s.io/dynamic-resource-allocation/controller"
	"k8s.io/klog/v2"

	_ "k8s.io/component-base/logs/json/register"            // for JSON log output support
	_ "k8s.io/component-base/metrics/prometheus/restclient" // for client metric registration
	_ "k8s.io/component-base/metrics/prometheus/version"    // for version metric registration
	_ "k8s.io/component-base/metrics/prometheus/workqueue"  // register work queues in the default legacy registry

	exampleclientset "github.com/kubernetes-sigs/dra-example-driver/pkg/example.com/resource/clientset/versioned"
)

type Flags struct {
	kubeconfig   *string
	kubeAPIQPS   *float32
	kubeAPIBurst *int
	workers      *int

	httpEndpoint *string
	metricsPath  *string
	profilePath  *string
}

type Clientset struct {
	core    coreclientset.Interface
	example exampleclientset.Interface
}

type Config struct {
	namespace string
	flags     *Flags
	csconfig  *rest.Config
	clientset *Clientset
	ctx       context.Context
	mux       *http.ServeMux
}

func main() {
	command := NewCommand()
	code := cli.Run(command)
	os.Exit(code)
}

// NewCommand creates a *cobra.Command object with default parameters.
func NewCommand() *cobra.Command {
	featureGate := featuregate.NewFeatureGate()
	logsconfig := logsapi.NewLoggingConfiguration()
	utilruntime.Must(logsapi.AddFeatureGates(featureGate))

	cmd := &cobra.Command{
		Use:  "dra-example-controller",
		Long: "dra-example-controller implements a DRA driver controller.",
	}

	flags := AddFlags(cmd, logsconfig, featureGate)

	cmd.PersistentPreRunE = func(cmd *cobra.Command, args []string) error {
		// Bind an environment variable to each input flag
		v := viper.New()
		v.SetEnvKeyReplacer(strings.NewReplacer("-", "_"))
		v.AutomaticEnv()
		cmd.Flags().VisitAll(func(f *pflag.Flag) {
			if !f.Changed && v.IsSet(f.Name) {
				val := v.Get(f.Name)
				if err := cmd.Flags().Set(f.Name, fmt.Sprintf("%v", val)); err != nil {
					klog.Errorf("Unable to bind environment variable to input flag: %v=%v", f.Name, val)
				}
			}
		})

		// Activate logging as soon as possible, after that
		// show flags with the final logging configuration.
		if err := logsapi.ValidateAndApply(logsconfig, featureGate); err != nil {
			return err
		}

		return nil
	}

	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		ctx := context.Background()
		mux := http.NewServeMux()

		csconfig, err := GetClientsetConfig(flags)
		if err != nil {
			return fmt.Errorf("create client configuration: %v", err)
		}

		coreclient, err := coreclientset.NewForConfig(csconfig)
		if err != nil {
			return fmt.Errorf("create core client: %v", err)
		}

		exampleclient, err := exampleclientset.NewForConfig(csconfig)
		if err != nil {
			return fmt.Errorf("create example.com client: %v", err)
		}

		config := &Config{
			ctx:       ctx,
			mux:       mux,
			flags:     flags,
			csconfig:  csconfig,
			namespace: os.Getenv("POD_NAMESPACE"),
			clientset: &Clientset{
				coreclient,
				exampleclient,
			},
		}

		if *flags.httpEndpoint != "" {
			err = SetupHTTPEndpoint(config)
			if err != nil {
				return fmt.Errorf("create http endpoint: %v", err)
			}
		}

		err = StartController(config)
		if err != nil {
			return fmt.Errorf("start controller: %v", err)
		}

		return nil
	}

	return cmd
}

func AddFlags(cmd *cobra.Command, logsconfig *logsapi.LoggingConfiguration, featureGate featuregate.MutableFeatureGate) *Flags {
	flags := &Flags{}
	sharedFlagSets := cliflag.NamedFlagSets{}

	fs := sharedFlagSets.FlagSet("logging")
	logsapi.AddFlags(logsconfig, fs)
	logs.AddFlags(fs, logs.SkipLoggingConfigurationFlags())

	fs = sharedFlagSets.FlagSet("Kubernetes client")
	flags.kubeconfig = fs.String("kubeconfig", "", "Absolute path to the kube.config file. Either this or KUBECONFIG need to be set if the driver is being run out of cluster.")
	flags.kubeAPIQPS = fs.Float32("kube-api-qps", 5, "QPS to use while communicating with the kubernetes apiserver.")
	flags.kubeAPIBurst = fs.Int("kube-api-burst", 10, "Burst to use while communicating with the kubernetes apiserver.")
	flags.workers = fs.Int("workers", 10, "Concurrency to process multiple claims")

	fs = sharedFlagSets.FlagSet("http server")
	flags.httpEndpoint = fs.String("http-endpoint", "",
		"The TCP network address where the HTTP server for diagnostics, including pprof and metrics will listen (example: `:8080`). The default is the empty string, which means the server is disabled.")
	flags.metricsPath = fs.String("metrics-path", "/metrics", "The HTTP path where Prometheus metrics will be exposed, disabled if empty.")
	flags.profilePath = fs.String("pprof-path", "", "The HTTP path where pprof profiling will be available, disabled if empty.")

	fs = sharedFlagSets.FlagSet("other")
	featureGate.AddFlag(fs)

	fs = cmd.PersistentFlags()
	for _, f := range sharedFlagSets.FlagSets {
		fs.AddFlagSet(f)
	}

	cols, _, _ := term.TerminalSize(cmd.OutOrStdout())
	cliflag.SetUsageAndHelpFunc(cmd, sharedFlagSets, cols)

	return flags
}

func GetClientsetConfig(f *Flags) (*rest.Config, error) {
	var csconfig *rest.Config

	kubeconfigEnv := os.Getenv("KUBECONFIG")
	if kubeconfigEnv != "" {
		klog.Infof("Found KUBECONFIG environment variable set, using that..")
		*f.kubeconfig = kubeconfigEnv
	}

	var err error
	if *f.kubeconfig == "" {
		csconfig, err = rest.InClusterConfig()
		if err != nil {
			return nil, fmt.Errorf("create in-cluster client configuration: %v", err)
		}
	} else {
		csconfig, err = clientcmd.BuildConfigFromFlags("", *f.kubeconfig)
		if err != nil {
			return nil, fmt.Errorf("create out-of-cluster client configuration: %v", err)
		}
	}

	csconfig.QPS = *f.kubeAPIQPS
	csconfig.Burst = *f.kubeAPIBurst

	return csconfig, nil
}

func SetupHTTPEndpoint(config *Config) error {
	if *config.flags.metricsPath != "" {
		// To collect metrics data from the metric handler itself, we
		// let it register itself and then collect from that registry.
		reg := prometheus.NewRegistry()
		gatherers := prometheus.Gatherers{
			// Include Go runtime and process metrics:
			// https://github.com/kubernetes/kubernetes/blob/9780d88cb6a4b5b067256ecb4abf56892093ee87/staging/src/k8s.io/component-base/metrics/legacyregistry/registry.go#L46-L49
			legacyregistry.DefaultGatherer,
		}
		gatherers = append(gatherers, reg)

		actualPath := path.Join("/", *config.flags.metricsPath)
		klog.InfoS("Starting metrics", "path", actualPath)
		// This is similar to k8s.io/component-base/metrics HandlerWithReset
		// except that we gather from multiple sources.
		config.mux.Handle(actualPath,
			promhttp.InstrumentMetricHandler(
				reg,
				promhttp.HandlerFor(gatherers, promhttp.HandlerOpts{})))
	}

	if *config.flags.profilePath != "" {
		actualPath := path.Join("/", *config.flags.profilePath)
		klog.InfoS("Starting profiling", "path", actualPath)
		config.mux.HandleFunc(path.Join("/", *config.flags.profilePath), pprof.Index)
		config.mux.HandleFunc(path.Join("/", *config.flags.profilePath, "cmdline"), pprof.Cmdline)
		config.mux.HandleFunc(path.Join("/", *config.flags.profilePath, "profile"), pprof.Profile)
		config.mux.HandleFunc(path.Join("/", *config.flags.profilePath, "symbol"), pprof.Symbol)
		config.mux.HandleFunc(path.Join("/", *config.flags.profilePath, "trace"), pprof.Trace)
	}

	listener, err := net.Listen("tcp", *config.flags.httpEndpoint)
	if err != nil {
		return fmt.Errorf("listen on HTTP endpoint: %v", err)
	}

	go func() {
		klog.InfoS("Starting HTTP server", "endpoint", *config.flags.httpEndpoint)
		err := http.Serve(listener, config.mux)
		if err != nil {
			klog.ErrorS(err, "HTTP server failed")
			klog.FlushAndExit(klog.ExitFlushTimeout, 1)
		}
	}()

	return nil
}

func StartController(config *Config) error {
	driver := NewDriver(config)
	informerFactory := informers.NewSharedInformerFactory(config.clientset.core, 0 /* resync period */)
	ctrl := controller.New(config.ctx, DriverAPIGroup, driver, config.clientset.core, informerFactory)
	informerFactory.Start(config.ctx.Done())
	ctrl.Run(*config.flags.workers)
	return nil
}
