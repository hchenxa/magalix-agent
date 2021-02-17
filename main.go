package main

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/MagalixCorp/magalix-agent/v2/agent"
	"github.com/MagalixCorp/magalix-agent/v2/auditor"
	"github.com/MagalixCorp/magalix-agent/v2/auditor/target"
	"github.com/MagalixCorp/magalix-agent/v2/client"
	"github.com/MagalixCorp/magalix-agent/v2/entities"
	"github.com/MagalixCorp/magalix-agent/v2/executor"
	"github.com/MagalixCorp/magalix-agent/v2/gateway"
	"github.com/MagalixCorp/magalix-agent/v2/kuber"
	"github.com/MagalixCorp/magalix-agent/v2/metrics"
	"github.com/MagalixCorp/magalix-agent/v2/utils"
	"github.com/MagalixTechnologies/core/logger"
	"github.com/MagalixTechnologies/uuid-go"
	"github.com/docopt/docopt-go"
	"github.com/open-policy-agent/frameworks/constraint/pkg/client/drivers/local"
	"github.com/pkg/errors"
	"go.uber.org/zap/zapcore"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/util/cert"

	opa "github.com/open-policy-agent/frameworks/constraint/pkg/client"
)

var usage = `agent - magalix services agent.

Usage:
  agent -h | --help
  agent [options] (--kube-url= | --kube-incluster) [--skip-namespace=]... [--source=]...

Options:
  --gateway <address>                        Connect to specified Magalix Kubernetes Agent gateway.
                                              [default: wss://gateway.agent.magalix.cloud]
  --account-id <identifier>                  Your account ID in Magalix.
                                              [default: $ACCOUNT_ID]
  --cluster-id <identifier>                  Your cluster ID in Magalix.
                                              [default: $CLUSTER_ID]
  --client-secret <secret>                   Unique and secret client token.
                                              [default: $SECRET]
  --kube-url <url>                           Use specified URL and token for access to kubernetes
                                              cluster.
  --kube-insecure                            Insecure skip SSL verify.
  --kube-root-ca-cert <filepath>             Filepath to root CA cert.
  --kube-token <token>                        Use specified token for access to kubernetes cluster.
  --kube-incluster                           Automatically determine kubernetes clientset
                                              configuration. Works only if program is
                                              running inside kubernetes cluster.
  --kube-timeout <duration>                  Timeout of requests to kubernetes apis.
                                              [default: 30s]
  --skip-namespace <pattern>                 Skip namespace matching a pattern (e.g. system-*),
                                              can be specified multiple times.
  --source <source>                          Specify source for metrics instead of
                                              automatically detected.
                                              Supported sources are:
                                              * kubelet;
  --kubelet-port <port>                      Override kubelet port for
                                              automatically discovered nodes.
                                              [default: 10255]
  --kubelet-backoff-sleep <duration>         Timeout of backoff policy.
                                              Timeout will be multiplied from 1 to 10.
                                              [default: 300ms]
  --kubelet-backoff-max-retries <retries>    Max retries of backoff policy, then consider failed.
                                              [default: 5]
  --metrics-interval <duration>              Metrics request and send interval.
                                              [default: 1m]
  --events-buffer-flush-interval <duration>  Events batch writer flush interval(Deprecated).
                                              [default: 10s]
  --events-buffer-size <size>                Events batch writer buffer size(Deprecated).
                                              [default: 20]
  --executor-workers <number>                 Executor concurrent workers count
                                              [default: 5]
  --timeout-proto-handshake <duration>       Timeout to do a websocket handshake.
                                              [default: 10s]
  --timeout-proto-write <duration>           Timeout to write a message to websocket channel.
                                              [default: 60s]
  --timeout-proto-read <duration>            Timeout to read a message from websocket channel.
                                              [default: 60s]
  --timeout-proto-reconnect <duration>       Timeout between reconnecting retries.
                                              [default: 1s]
  --timeout-proto-backoff <duration>         Timeout of backoff policy.
                                              Timeout will be multiplied from 1 to 10.
                                              [default: 300ms]
  --opt-in-analysis-data                     Send anonymous data for analysis.(Deprecated)
  --analysis-data-interval <duration>        Analysis data send interval.(Deprecated)
                                              [default: 5m]
  --packets-v2                               Enable v2 packets (without ids). (Deprecated)
  --disable-metrics                          Disable metrics collecting and sending.
  --disable-events                           Disable events collecting and sending.(Deprecated)
  --disable-scalar                           Disable in-agent scalar. (Deprecated)
  --port <port>                              Port to start the server on for liveness and readiness probes
                                               [default: 80]
  --dry-run                                  Disable automation execution.
  --no-send-logs                             Disable sending logs to the backend.
  --debug                                    Enable debug messages.
  --trace                                    Enable debug and trace messages.
  --trace-log <path>                         Write log messages to specified file. (Deprecated)
  --log-level <string>                       Log level
                                              [default: warn]
  -h --help                                  Show this help.
  --version                                  Show version.
`

var version = "[manual build]"

// @TODO: Should be changed to be unique per cluster/account id
const webHookName = "com.magalix.webhook"

var startID string

func main() {
	args, err := docopt.ParseArgs(usage, nil, getVersion())
	if err != nil {
		panic(err)
	}

	logger.Infow(
		"magalix agent started.....",
		"version", version,
		"args", fmt.Sprintf("%q", utils.GetSanitizedArgs()),
	)

	// TODO: remove
	// a hack to set default timeout for all http requests
	http.DefaultClient = &http.Client{
		Timeout: 20 * time.Second,
	}

	port := args["--port"].(string)
	probes := NewProbesServer(":" + port)
	go func() {
		err = probes.Start()
		if err != nil {
			logger.Fatalw("unable to start probes server", "error", err)
			os.Exit(1)
		}
	}()

	startID = uuid.NewV4().String()

	accountID := utils.ExpandEnvUUID(args, "--account-id")
	clusterID := utils.ExpandEnvUUID(args, "--cluster-id")

	secret, err := base64.StdEncoding.DecodeString(
		utils.ExpandEnv(args, "--client-secret", false),
	)
	if err != nil {
		logger.Fatalw(
			"unable to decode base64 secret specified as --client-secret flag",
			"error", err,
		)
		os.Exit(1)
	}

	kRestConfig, err := getKRestConfig(args)

	kube, err := kuber.InitKubernetes(kRestConfig)
	if err != nil {
		logger.Fatalw("unable to initialize Kubernetes", "error", err)
		os.Exit(1)
	}

	k8sServerVersion, err := kube.GetServerVersion()
	if err != nil {
		logger.Warnw("failed to discover server version", "error", err)
	}

	agentPermissions, err := kube.GetAgentPermissions()
	if err != nil {
		agentPermissions = err.Error()
		logger.Warnw("Failed to get agent permissions", "error", err)
	}

	gatewayUrl := args["--gateway"].(string)
	protoHandshakeTime := utils.MustParseDuration(args, "--timeout-proto-handshake")
	protoWriteTime := utils.MustParseDuration(args, "--timeout-proto-write")
	protoReadTime := utils.MustParseDuration(args, "--timeout-proto-read")
	protoReconnectTime := utils.MustParseDuration(args, "--timeout-proto-reconnect")
	protoBackoffTime := utils.MustParseDuration(args, "--timeout-proto-backoff")
	sendLogs := !args["--no-send-logs"].(bool)
	mgxGateway := gateway.New(
		gatewayUrl,
		accountID,
		clusterID,
		secret,
		version,
		startID,
		k8sServerVersion,
		agentPermissions,
		protoHandshakeTime,
		protoWriteTime,
		protoReadTime,
		protoReconnectTime,
		protoBackoffTime,
		sendLogs)

	logLevel := args["--log-level"].(string)
	if err := ConfigureGlobalLogger(accountID, clusterID, logLevel, mgxGateway.GetLogsWriteSyncer()); err != nil {
		logger.Fatalw("failed to configure logger. %w", err)
		os.Exit(1)
	}
	defer logger.Sync()

	dynamicClient, err := dynamic.NewForConfig(kRestConfig)
	parentsStore := kuber.NewParentsStore()
	const observerDefaultResyncTime = time.Minute * 5
	observer := kuber.NewObserver(
		dynamicClient,
		parentsStore,
		make(chan struct{}),
		observerDefaultResyncTime,
	)
	err = observer.WaitForCacheSync()
	if err != nil {
		logger.Fatalw("unable to start observer", "error", err)
	}
	kubeletPort := args["--kubelet-port"].(string)
	metricsInterval := utils.MustParseDuration(args, "--metrics-interval")
	kubeletBackoffSleepTime := utils.MustParseDuration(args, "--kubelet-backoff-sleep")
	kubeletBackoffMaxRetries := utils.MustParseInt(args, "--kubelet-backoff-max-retries")
	metricsSource, err := metrics.NewMetrics(
		observer,
		kube,
		kubeletPort,
		metricsInterval,
		kubeletBackoffSleepTime,
		kubeletBackoffMaxRetries,
	)
	if err != nil {
		logger.Fatalf("unable to initialize metrics source, error: %w", err)
		os.Exit(1)
	}

	k8sMinorVersion, err := kube.GetServerMinorVersion()
	if err != nil {
		logger.Warnw("failed to discover server minor version", "error", err)
	}

	driver := local.New()
	backend, err := opa.NewBackend(opa.Driver(driver))
	if err != nil {
		logger.Fatal(err)
	}

	opaClient, err := backend.NewClient(opa.Targets(&target.K8sValidationTarget{}))

	ew := entities.NewEntitiesWatcher(observer, k8sMinorVersion, opaClient)

	executorWorkers := utils.MustParseInt(args, "--executor-workers")
	dryRun := args["--dry-run"].(bool)
	automationExecutor := executor.NewExecutor(
		kube,
		observer,
		executorWorkers,
		dryRun,
	)

	// @TODO: Replace below logic with logic for getting templates and constraints from policy service and adding it to opa cache

	// data, err := kube.Clientset.RESTClient().
	// 	Get().
	// 	AbsPath("/apis/constraints.gatekeeper.sh/v1beta1").
	// 	Resource("K8sRequiredLabels").
	// 	Name("ns-must-have-gk").
	// 	DoRaw(context.TODO())

	// constraint := unstructured.Unstructured{}
	// json.Unmarshal(data, &constraint)

	// data, err = kube.Clientset.RESTClient().
	// 	Get().
	// 	AbsPath("/apis/templates.gatekeeper.sh/v1beta1").
	// 	Resource("constrainttemplates").
	// 	Name("k8srequiredlabels").
	// 	DoRaw(context.TODO())

	// template := &templates.ConstraintTemplate{}
	// json.Unmarshal(data, &template)

	// ctx := context.TODO()
	// opaClient.AddConstraint(ctx, template)
	// opaClient.AddConstraint(ctx, &constraint)

	auditor := auditor.NewAuditor(opaClient, parentsStore)
	//webhookHandler, err := webhook.NewWebHookHandler(webHookName, opaClient, kube)
	if err != nil {
		logger.Fatalw("Error while creating validating webhook server", "errror", err)
	}

	// init gateway
	mgxAgent := agent.New(
		metricsSource,
		ew,
		automationExecutor,
		mgxGateway,
		func(level *agent.LogLevel) error {
			return ConfigureGlobalLogger(accountID, clusterID, level.Level, mgxGateway.GetLogsWriteSyncer())
		},
		auditor,
		//webhookHandler,
	)

	probes.IsReady = true

	err = mgxAgent.Start()
	if err != nil {
		logger.Fatal(err)
		os.Exit(1)
	}
}

func getKRestConfig(
	args map[string]interface{},
) (config *rest.Config, err error) {
	if args["--kube-incluster"].(bool) {
		logger.Info("initializing kubernetes incluster config")

		config, err = rest.InClusterConfig()
		if err != nil {
			return nil, errors.Wrap(err, "unable to get incluster config")
		}

	} else {
		logger.Info("initializing kubernetes user-defined config")

		token, _ := args["--kube-token"].(string)
		if token == "" {
			token = os.Getenv("KUBE_TOKEN")
		}

		config = &rest.Config{}
		config.ContentType = runtime.ContentTypeJSON
		config.APIPath = "/api"
		config.Host = args["--kube-url"].(string)
		config.BearerToken = token

		{
			tlsClientConfig := rest.TLSClientConfig{}
			rootCAFile, ok := args["--kube-root-ca-cert"].(string)
			if ok {
				if _, err := cert.NewPool(rootCAFile); err != nil {
					fmt.Printf("Expected to load root CA config from %s, but got err: %v", rootCAFile, err)
				} else {
					tlsClientConfig.CAFile = rootCAFile
				}
				config.TLSClientConfig = tlsClientConfig
			}
		}

		if args["--kube-insecure"].(bool) {
			config.Insecure = true
		}
	}

	config.Timeout = utils.MustParseDuration(args, "--kube-timeout")

	return
}

// ConfigureGlobalLogger sets additional info and log level for global logger
func ConfigureGlobalLogger(accountId uuid.UUID, clusterId uuid.UUID, level string, logsSink zapcore.WriteSyncer) error {
	var loggerLevel logger.Level
	switch level {
	case "info":
		loggerLevel = logger.InfoLevel
	case "debug":
		loggerLevel = logger.DebugLevel
	case "warn":
		loggerLevel = logger.WarnLevel
	case "error":
		loggerLevel = logger.ErrorLevel
	default:
		return fmt.Errorf("unsupported log level %s", level)
	}
	logger.ConfigWriterSync(loggerLevel, logsSink)
	logger.WithGlobal("accountID", accountId, "clusterID", clusterId)
	return nil
}

func getVersion() string {
	return strings.Join([]string{
		"magalix agent " + version,
		"protocol/major: " + fmt.Sprint(client.ProtocolMajorVersion),
		"protocol/minor: " + fmt.Sprint(client.ProtocolMinorVersion),
	}, "\n")
}
