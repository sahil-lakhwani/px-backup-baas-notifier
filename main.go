package main

import (
	"flag"
	"log"
	"net/url"
	"os"
	"time"

	"github.com/caarlos0/env/v6"
	"github.com/go-logr/logr"
	"github.com/portworx/px-backup-baas-notifier/pkg/notification"
	"github.com/portworx/px-backup-baas-notifier/pkg/schedule"
	"go.uber.org/zap/zapcore"

	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
)

var (
	Logger  logr.Logger
	nsLabel string
)

var appEnvConfig = AppEnvConfig{RetryDelaySeconds: 8, TokenDuration: "365d"}

const (
	ScheduleTimeout = 20
)

func init() {
	opts := zap.Options{
		Development: true,
		TimeEncoder: zapcore.TimeEncoderOfLayout(time.RFC3339),
	}
	Logger = zap.New(zap.UseFlagOptions(&opts))

	if err := env.Parse(&appEnvConfig); err != nil {
		log.Fatal(err)
	}

	nsLabel = "" //os.Getenv("nsLabel")
	// if nsLabel == "" {
	// 	Logger.Error(errors.New("nsLabel env not found"), "Namespace Identifier label 'nsLabel' must be set as env")
	// 	os.Exit(1)
	// } else {
	// 	if res := strings.Split(nsLabel, ":"); len(res) != 2 {
	// 		Logger.Error(errors.New("Invalid env 'nsLabel'"), "Namespace Identifier label 'nsLabel' must be in `key:val` format ")
	// 		os.Exit(1)
	// 	}
	// }
}

func main() {

	config, err := kubeconfig()
	if err != nil {
		Logger.Error(err, "Could not load config from kubeconfig")
		os.Exit(1)
	}

	schedule := schedule.NewSchedule(appEnvConfig.SchedulerURL, int64(ScheduleTimeout), schedule.TokenConfig{
		ClientID:      appEnvConfig.ClientID,
		UserName:      appEnvConfig.UserName,
		Password:      appEnvConfig.Password,
		TokenDuration: appEnvConfig.TokenDuration,
		TokenURL:      appEnvConfig.TokenURL,
	}, Logger)

	if !isUrl(appEnvConfig.WebhookURL) {
		Logger.Info("Invalid webhook url configured", "webhookUrl", appEnvConfig.WebhookURL)
		os.Exit(1)
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		Logger.Error(err, "error creating clientset")
		os.Exit(1)
	}

	dynClient, err := dynamic.NewForConfig(config)
	if err != nil {
		Logger.Error(err, "Error getting dyn client")
		os.Exit(1)
	}

	infFactory := dynamicinformer.NewDynamicSharedInformerFactory(dynClient, 0*time.Minute)

	stopch := make(<-chan struct{})

	notifyClient := notification.NewClient(appEnvConfig.WebhookURL, appEnvConfig.IngressURL)

	c := newController(clientset, dynClient, infFactory, stopch, notifyClient, *schedule)

	c.run(stopch)

}

func isUrl(str string) bool {
	u, err := url.Parse(str)
	return err == nil && u.Scheme != "" && u.Host != ""
}

func kubeconfig() (*rest.Config, error) {
	if appEnvConfig.KubeConfigFile == "" {
		appEnvConfig.KubeConfigFile = os.Getenv("HOME") + "/.kube/config"
	}

	kubeconfig := flag.String("kubeconfig", appEnvConfig.KubeConfigFile, "absolute path to the kubeconfig file")
	config, err := clientcmd.BuildConfigFromFlags("", *kubeconfig)
	if err != nil {
		Logger.Info("Error loading kube configuration from directory")
		config, err = rest.InClusterConfig()
		if err != nil {
			return nil, err
		}
		Logger.Info("Loaded config from cluster sucessfully.")
	}
	Logger.Info("Loaded config from kubeconfig file.")
	return config, nil
}
