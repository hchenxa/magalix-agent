package main

import (
	"encoding/base64"
	"fmt"
	"github.com/MagalixCorp/magalix-agent/executor"
	"github.com/MagalixCorp/magalix-agent/kuber"
	"github.com/MagalixCorp/magalix-agent/utils"
	"strings"

	"github.com/MagalixCorp/magalix-agent/client"
	"github.com/MagalixCorp/magalix-agent/events"
	"github.com/MagalixCorp/magalix-agent/metrics"
	"github.com/MagalixCorp/magalix-agent/scanner"
	"github.com/MagalixTechnologies/log-go"
	"github.com/MagalixTechnologies/uuid-go"
	"github.com/docopt/docopt-go"
	"github.com/reconquest/karma-go"
	"os"
)

var usage = `agent - magalix services agent.

Usage:
  agent -h | --help
  agent [options] (--kube-url= | --kube-incluster) [--skip-namespace=]... [--source=]...

Options:
  --gateway <address>                        Connect to specified Magalix Kubernetes Agent gateway.
                                              [default: ws://gateway.agent.magalix.cloud]
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
  --skip-namespace <pattern>                 Skip namespace matching a pattern (e.g. system-*),
                                              can be specified multiple times.
  --source <source>                          Specify source for metrics instead of
                                              automatically detected.
                                              Supported sources are:
                                              * kubelet;
  --kubelet-address <addr>                   Override kubelet nodes address.
  --kubelet-port <port>                      Override kubelet port for
                                              automatically discovered nodes.
                                              [default: 10255]
  --kubelet-backoff-sleep <duration>         Timeout of backoff policy.
                                              Timeout will be multiplied from 1 to 10.
                                              [default: 300ms]
  --kubelet-backoff-max-retries <retries>    Max reties of backoff policy, then consider failed.
                                              [default: 5]
  --metrics-interval <duration>              Metrics request and send interval.
                                              [default: 1m]
  --events-buffer-flush-interval <duration>  Events batch writer flush interval.
                                              [default: 10s]
  --events-buffer-size <size>                Events batch writer buffer size.
                                              [default: 20]
  --timeout-proto-handshake <duration>       Timeout to do a websocket handshake.
                                              [default: 10s]
  --timeout-proto-write <duration>           Timeout to write a message to websocket channel.
                                              [default: 60s]
  --timeout-proto-read <duration>            Timeout to read a message from websocket channel.
                                              [default: 60s]
  --timeout-proto-reconnect <duration>       Timeout between reconneting retries.
                                              [default: 1s]
  --timeout-proto-backoff <duration>         Timeout of backoff policy.
                                              Timeout will be multipled from 1 to 10.
                                              [default: 300ms]
  --opt-in-analysis-data					 Send anonymous data for analysis.
											  [default: false]
  --disable-metrics                          Disable metrics collecting and sending.
  --disable-events                           Disable events collecting and sending.
  --dry-run                                  Disable decision execution.
  --debug                                    Enable debug messages.
  --trace                                    Enable debug and trace messages.
  --trace-log <path>                         Write log messages to specified file
                                              [default: trace.log]
  -h --help                                  Show this help.
  --version                                  Show version.
`

var version = "[manual build]"

func getVersion() string {
	return strings.Join([]string{
		"magalix agent " + version,
		"protocol/major: " + fmt.Sprint(client.ProtocolMajorVersion),
		"protocol/minor: " + fmt.Sprint(client.ProtocolMinorVersion),
	}, "\n")
}

func main() {
	args, err := docopt.ParseArgs(usage, nil, getVersion())
	if err != nil {
		panic(err)
	}

	stderr := log.New(
		args["--debug"].(bool),
		args["--trace"].(bool),
		args["--trace-log"].(string),
	)
	// we need to disable default exit 1 for FATAL messages because we also
	// need to send fatal messages on the remote server and send bye packet
	// after fatal message (if we can), therefore all exits will be controlled
	// manually
	stderr.SetExiter(func(int) {})
	utils.SetLogger(stderr)

	stderr.Infof(
		karma.Describe("version", version).
			Describe("args", fmt.Sprintf("%q", utils.GetSanitizedArgs())),
		"magalix agent started",
	)

	secret, err := base64.StdEncoding.DecodeString(
		utils.ExpandEnv(args, "--client-secret", false),
	)
	if err != nil {
		stderr.Fatalf(
			err,
			"unable to decode base64 secret specified as --client-secret flag",
		)
		os.Exit(1)
	}

	var (
		accountID = utils.ExpandEnvUUID(args, "--account-id")
		clusterID = utils.ExpandEnvUUID(args, "--cluster-id")

		metricsEnabled = !args["--disable-metrics"].(bool)
		eventsEnabled  = !args["--disable-events"].(bool)

		skipNamespaces []string
	)

	if namespaces, ok := args["--skip-namespace"].([]string); ok {
		skipNamespaces = namespaces
	}

	gwClient, err := client.InitClient(args, accountID, clusterID, secret, stderr)

	defer gwClient.WaitExit()
	defer gwClient.Recover()

	if err != nil {
		stderr.Fatalf(err, "unable to connect to gateway")
		os.Exit(1)
	}

	kube, err := kuber.InitKubernetes(args, gwClient)
	if err != nil {
		stderr.Fatalf(err, "unable to initialize Kubernetes")
		os.Exit(1)
	}

	entityScanner := scanner.InitScanner(
		gwClient,
		kube,
		skipNamespaces,
		accountID,
		clusterID,
		args["--opt-in-analysis-data"].(bool),
	)

	oomKilled := make(chan uuid.UUID)

	executor.InitExecutor(
		gwClient,
		kube,
		entityScanner,
		oomKilled,
		args,
	)

	if eventsEnabled {
		events.InitEvents(
			gwClient,
			kube,
			skipNamespaces,
			entityScanner,
			oomKilled,
			args,
		)
	}

	if metricsEnabled {
		_, err := metrics.InitMetrics(gwClient, entityScanner, args)
		if err != nil {
			gwClient.Fatalf(err, "unable to initialize metrics sources")
			os.Exit(1)
		}
	}

}
